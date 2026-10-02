package eventtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/prometheus/prompb"
)

const (
	metricClaim    = "app_event_time_claim_seconds"
	metricTerminal = "app_event_time_terminal_window_count"
)

// linSink is a sink that can refuse the requests carrying a named metric.
type linSink struct {
	sink
	refuse map[string]bool
	// refuseFrom, when set, refuses a request carrying a claim sample stamped
	// at or after it.
	refuseFrom int64
}

func (s *linSink) write(ctx context.Context, req *prompb.WriteRequest) error {
	s.mu.Lock()
	for _, ts := range req.Timeseries {
		if s.refuse[label(ts, "__name__")] {
			s.mu.Unlock()
			return errors.New("write refused")
		}
		if s.refuseFrom != 0 && label(ts, "__name__") == metricClaim && ts.Samples[0].Timestamp >= s.refuseFrom {
			s.mu.Unlock()
			return errors.New("write refused")
		}
	}
	s.mu.Unlock()
	return s.sink.write(ctx, req)
}

func (s *linSink) setRefuse(names ...string) {
	s.mu.Lock()
	s.refuse = make(map[string]bool)
	for _, n := range names {
		s.refuse[n] = true
	}
	s.mu.Unlock()
}

// series returns every written sample of name as label set -> value, keyed
// by the given labels plus the timestamp.
func (s *linSink) series(name string, keys ...string) map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64)
	for _, req := range s.reqs {
		for _, ts := range req.Timeseries {
			if label(ts, "__name__") != name {
				continue
			}
			var b strings.Builder
			for _, k := range keys {
				b.WriteString(label(ts, k))
				b.WriteByte('|')
			}
			for _, smp := range ts.Samples {
				k := fmt.Sprintf("%s%d", b.String(), smp.Timestamp)
				// The store keeps the maximum of equal-timestamp samples.
				if v, ok := out[k]; !ok || smp.Value > v {
					out[k] = smp.Value
				}
			}
		}
	}
	return out
}

func (s *linSink) count(name string) int {
	return len(s.samples(name))
}

var linCfg = func() Config {
	c := cfg
	c.Lineage = true
	c.TerminalFamilies = []string{fam}
	return c
}()

func newLinAcc(t *testing.T, partitions ...int32) (*Accumulator, *linSink) {
	t.Helper()
	s := &linSink{}
	a, err := New(linCfg, "job", "pod-a", 2, s.write, func(string) (int, error) { return len(partitions), nil })
	if err != nil {
		t.Fatal(err)
	}
	// A writer's end is read at t0 plus an hour: its owed windows end there.
	a.lin.now = func() time.Time { return time.UnixMilli(t0 + 60*min) }
	a.StartGeneration(map[string][]int32{topic: partitions}, time.UnixMilli(t0))
	for _, p := range partitions {
		a.Claim(topic, p).Start(0, 1_000_000)
	}
	return a, s
}

// hand counts one request as the caller would: handed on first, then stamped
// with the writer it was received under.
func hand(a *Accumulator, g Generation, partition int32, host string, clientMs, serverMs, nowMs int64) {
	a.Claim(topic, partition).Hand()
	s := a.Shard(0)
	st := s.StampAs(g, topic, partition, Reading{EventMs: clientMs, PrimaryMs: serverMs}, nowMs)
	s.CountLabels(st, fam, famLabels, host)
}

func register(a *Accumulator, partitions ...int32) {
	for _, p := range partitions {
		a.Register(a.Generation(), topic, p, "pod-z/1", time.UnixMilli(t0))
	}
}

func TestWithoutLineageNothingNewIsWrittenOrExposed(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0, t0, t0)
	observe(a, 0, 0, "h", t0+15*min, t0+15*min, t0+15*min)
	flush(t, a, t0+15*min)
	for _, m := range []string{metricClaim, metricTerminal} {
		if n := len(s.samples(m)); n != 0 {
			t.Fatalf("%s written %d times", m, n)
		}
	}
	for _, w := range s.samples(metricWatermark) {
		if w.generation != "" {
			t.Fatalf("watermark carries a generation: %+v", w)
		}
	}
	if a.WriterID() != "" || a.Generation() != 0 {
		t.Fatalf("writer %q %d", a.WriterID(), a.Generation())
	}
	out, err := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_observations_total", "app_event_time_gate_held_seconds")
	if err != nil || len(out) != 0 {
		t.Fatalf("lineage self-metrics exposed: %q %v", out, err)
	}
}

func TestTerminalFamiliesNeedLineage(t *testing.T) {
	c := cfg
	c.TerminalFamilies = []string{fam}
	if c.Validate() == nil {
		t.Fatal("accepted terminal families without lineage")
	}
}

func TestTheWriterIDNamesInstanceAndGeneration(t *testing.T) {
	a, _ := newLinAcc(t, 0)
	if got, want := a.WriterID(), fmt.Sprintf("pod-a/%d", t0); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.WriterID() != "" {
		t.Fatalf("a writer between sessions: %q", a.WriterID())
	}
	// A second session in the same millisecond is still a different writer.
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0))
	if got, want := a.WriterID(), fmt.Sprintf("pod-a/%d", t0+1); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTheGateHoldsEveryWriteUntilAHandingPartitionIsRegistered(t *testing.T) {
	a, s := newLinAcc(t, 0, 1)
	g := a.Generation()
	hand(a, g, 0, "h", t0, t0, t0)
	hand(a, g, 0, "h", t0+15*min, t0+15*min, t0+15*min)
	a.Claim(topic, 1).Start(-1, 0) // empty and idle: never handed anything
	flush(t, a, t0+15*min)
	if len(s.reqs) != 0 {
		t.Fatalf("wrote under a shut gate: %d requests", len(s.reqs))
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_gate_held_seconds")
	if !strings.Contains(string(out), "app_event_time_gate_held_seconds 0\n") {
		// The gate closed at this flush, so it has been held for 0s.
		t.Fatalf("gauge: %s", out)
	}
	flush(t, a, t0+16*min)
	out, _ = testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_gate_held_seconds")
	if !strings.Contains(string(out), "app_event_time_gate_held_seconds 60\n") {
		t.Fatalf("gauge after a minute held: %s", out)
	}

	register(a, 0) // partition 1 handed nothing, so it does not hold the gate
	flush(t, a, t0+16*min)
	claims := s.series(metricClaim, "partition", "predecessor", LabelGeneration)
	if claims[fmt.Sprintf("0|pod-z/1|%d|%d", t0, t0)] == 0 || len(claims) != 1 {
		t.Fatalf("claims %v", claims)
	}
	if got := s.series(windowFam)[fmt.Sprint(t0)]; got != 1 {
		t.Fatalf("window after the gate opened: %v", s.series(windowFam))
	}
	for _, w := range s.samples(metricWatermark) {
		if w.generation != fmt.Sprint(t0) {
			t.Fatalf("watermark generation %+v", w)
		}
	}
}

// A store keeps a claim record for its retention only, and a writer may outlive
// it. So every flush re-writes the writer's claims at the flush, with the claim
// time as the value, and so does its end.
func TestAWrittenClaimIsRewrittenAtEachFlushAndAtTheEnd(t *testing.T) {
	a, s := newLinAcc(t, 0)
	register(a, 0)
	flush(t, a, t0) // the first write, stamped at the claim time
	flush(t, a, t0+15*min)
	flush(t, a, t0+30*min)
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := s.series(metricClaim, "partition", "predecessor", LabelGeneration)
	for _, at := range []int64{t0, t0 + 15*min, t0 + 30*min, t0 + 60*min} {
		if v := claims[fmt.Sprintf("0|pod-z/1|%d|%d", t0, at)]; v != float64(t0)/1000 {
			t.Fatalf("claim at %d: %v, all %v", (at-t0)/min, v, claims)
		}
	}
	if len(claims) != 4 {
		t.Fatalf("claims %v", claims)
	}
}

// A failed re-write is counted, and leaves the gate open: the claim was
// written once, which is what the gate waits for.
func TestAFailedClaimRewriteLeavesTheGateOpen(t *testing.T) {
	a, s := newLinAcc(t, 0)
	g := a.Generation()
	register(a, 0)
	flush(t, a, t0)
	s.setRefuse(metricClaim)
	hand(a, g, 0, "h", t0+15*min, t0+15*min, t0+15*min)
	if err := a.Flush(context.Background(), time.UnixMilli(t0+15*min)); err == nil {
		t.Fatal("no error for a refused claim re-write")
	}
	if a.gateShut() {
		t.Fatal("a failed re-write shut the gate")
	}
	if n := s.count(metricWatermark); n == 0 {
		t.Fatal("no watermark while the gate is open")
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_write_errors_total")
	if !strings.Contains(string(out), `stage="claims"} 1`) {
		t.Fatalf("write errors: %s", out)
	}
}

// A partition registered later is written in its own request: a refused
// re-write of the others must not hold its first write, and so the gate.
func TestAFailedRewriteDoesNotHoldANewPartitionsFirstWrite(t *testing.T) {
	a, s := newLinAcc(t, 0, 1)
	g := a.Generation()
	register(a, 0)
	flush(t, a, t0)
	s.refuseFrom = t0 + 1 // refuse the claim samples stamped after the claim time: the re-writes
	register(a, 1)
	hand(a, g, 1, "h", t0+15*min, t0+15*min, t0+15*min)
	if err := a.Flush(context.Background(), time.UnixMilli(t0+15*min)); err == nil {
		t.Fatal("no error for a refused claim re-write")
	}
	if a.gateShut() {
		t.Fatal("the refused re-write held partition 1's first write")
	}
}

// A re-write is never stamped at or before the last one: a store that keeps
// samples in order would refuse it.
func TestAClaimIsNeverRewrittenAtOrBeforeTheLastRewrite(t *testing.T) {
	a, s := newLinAcc(t, 0)
	register(a, 0)
	flush(t, a, t0)
	flush(t, a, t0+90*min) // past the end's clock, t0+60min
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := s.series(metricClaim, "partition")
	if _, ok := claims[fmt.Sprintf("0|%d", t0+60*min)]; ok || len(claims) != 2 {
		t.Fatalf("claims %v", claims)
	}
}

func TestTheGateShutsAgainWhenAnotherPartitionHandsItsFirstMessage(t *testing.T) {
	a, s := newLinAcc(t, 0, 1)
	g := a.Generation()
	register(a, 0)
	hand(a, g, 0, "h", t0+15*min, t0+15*min, t0+15*min)
	hand(a, g, 1, "h", t0+15*min, t0+15*min, t0+15*min)
	flush(t, a, t0+15*min)
	if n := s.count(metricWatermark); n != 0 {
		t.Fatalf("partition 1 handed and is unregistered, yet %d watermarks", n)
	}
	register(a, 1)
	flush(t, a, t0+15*min)
	if n := s.count(metricWatermark); n == 0 {
		t.Fatal("no watermark once both partitions are registered")
	}
}

func TestARegistrationForAnEndedWriterIsIgnored(t *testing.T) {
	a, _ := newLinAcc(t, 0)
	old := a.Generation()
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0+min))
	a.Register(old, topic, 0, "pod-z/1", time.UnixMilli(t0))
	a.Claim(topic, 0).Hand()
	if !a.gateShut() {
		t.Fatal("an ended writer's registration opened the new writer's gate")
	}
}

// Sealing writes a terminal record for every window the writer owes, from its
// claim minus max_lateness, zeros included, stamped at the window start.
func TestSealingWritesATerminalRecordForEveryOwedWindow(t *testing.T) {
	a, s := newLinAcc(t, 0)
	g := a.Generation()
	register(a, 0)
	hand(a, g, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	hand(a, g, 0, "k", t0+2_000, t0+2_000, t0+2_000)
	hand(a, g, 0, "h", t0+15*min, t0+15*min, t0+15*min)
	flush(t, a, t0+15*min)

	rec := s.series(metricTerminal, "family", "stage")
	// Sealed below floor(w - max_lateness) = t0+5min; owed from t0-10min.
	for start := t0 - 10*min; start < t0+5*min; start += min {
		for _, stage := range []string{"written", "folded"} {
			k := fmt.Sprintf("%s|%s|%d", windowFam, stage, start)
			want := 0.0
			if start == t0 {
				want = 2
			}
			if v, ok := rec[k]; !ok || v != want {
				t.Fatalf("%s: got %v (present %v), want %v", k, v, ok, want)
			}
		}
	}
	if len(rec) != 2*15 {
		t.Fatalf("%d records, want %d", len(rec), 2*15)
	}
}

// A new writer starts sealed below its claim minus max_lateness: it cannot
// count into a window it owes no terminal record for.
func TestAWriterCannotCountIntoAWindowBeforeItsOwedRange(t *testing.T) {
	a, s := newLinAcc(t, 0)
	g := a.Generation()
	register(a, 0)
	hand(a, g, 0, "h", t0-11*min, t0, t0)
	flush(t, a, t0)
	if s.count(windowFam) != 0 {
		t.Fatal("counted into a window before the owed range")
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_late_total")
	if !strings.Contains(string(out), `family="`+fam+`"} 1`) {
		t.Fatalf("late: %s", out)
	}
}

func TestEndingAWriterWritesItsWindowsThenEveryOwedTerminalRecord(t *testing.T) {
	a, s := newLinAcc(t, 0)
	g := a.Generation()
	register(a, 0)
	hand(a, g, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	flush(t, a, t0+1_000)
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.series(windowFam)[fmt.Sprint(t0)]; got != 1 {
		t.Fatalf("window: %v", s.series(windowFam))
	}
	rec := s.series(metricTerminal, "stage")
	if rec[fmt.Sprintf("written|%d", t0)] != 1 || rec[fmt.Sprintf("folded|%d", t0)] != 1 {
		t.Fatalf("records of t0: %v", rec)
	}
	// Every window from the claim minus max_lateness through the end plus
	// future_skew, and nothing past it.
	for start := t0 - 10*min; start <= t0+61*min; start += min {
		if _, ok := rec[fmt.Sprintf("written|%d", start)]; !ok {
			t.Fatalf("owed window %d has no record", (start-t0)/min)
		}
	}
	if _, ok := rec[fmt.Sprintf("written|%d", t0+62*min)]; ok {
		t.Fatal("a record past the end plus future_skew")
	}
}

// A data batch that fails at the end is counted as dropped, and the record says
// what was really written: folded - written = dropped.
func TestAFailedFinalWriteIsDroppedAndTheRecordCarriesTheTruePair(t *testing.T) {
	a, s := newLinAcc(t, 0)
	g := a.Generation()
	register(a, 0)
	flush(t, a, t0) // writes the claim record
	hand(a, g, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	hand(a, g, 0, "h", t0+2_000, t0+2_000, t0+2_000)
	s.setRefuse(windowFam)
	err := a.EndGeneration(context.Background())
	if err == nil {
		t.Fatal("no error for a refused window write")
	}
	rec := s.series(metricTerminal, "stage")
	if rec[fmt.Sprintf("written|%d", t0)] != 0 || rec[fmt.Sprintf("folded|%d", t0)] != 2 {
		t.Fatalf("records of t0: %v", rec)
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_dropped_unwritten_total")
	if !strings.Contains(string(out), `family="`+fam+`"} 2`) {
		t.Fatalf("dropped: %s", out)
	}
}

func TestAWriterEndingWithTheGateShutWritesNothing(t *testing.T) {
	a, s := newLinAcc(t, 0)
	hand(a, a.Generation(), 0, "h", t0+1_000, t0+1_000, t0+1_000)
	if err := a.EndGeneration(context.Background()); !errors.Is(err, errGateHeld) {
		t.Fatalf("err %v", err)
	}
	if len(s.reqs) != 0 {
		t.Fatalf("%d requests written", len(s.reqs))
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_dropped_unwritten_total")
	if !strings.Contains(string(out), `family="`+fam+`"} 1`) {
		t.Fatalf("dropped: %s", out)
	}
}

// A gate held past twice max_lateness drops the cells and counts them, and the
// record written once the gate opens still carries the true pair.
func TestARecordWrittenAfterALongHeldGateCarriesTheTruePair(t *testing.T) {
	a, s := newLinAcc(t, 0)
	g := a.Generation()
	hand(a, g, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	hand(a, g, 0, "h", t0+25*min, t0+25*min, t0+25*min)
	flush(t, a, t0+25*min) // w - 2 max_lateness = t0+5min: window t0 dropped
	if len(s.reqs) != 0 {
		t.Fatal("wrote under a shut gate")
	}
	register(a, 0)
	flush(t, a, t0+25*min)
	rec := s.series(metricTerminal, "stage")
	if rec[fmt.Sprintf("written|%d", t0)] != 0 || rec[fmt.Sprintf("folded|%d", t0)] != 1 {
		t.Fatalf("records of t0: %v", rec)
	}
	if _, ok := s.series(windowFam)[fmt.Sprint(t0)]; ok {
		t.Fatal("the dropped window was written")
	}
}

// A message received by one writer and counted after it ended lands in no
// writer, and neither do counts made between writers.
func TestACountForAnEndedWriterIsAfterTheEnd(t *testing.T) {
	a, s := newLinAcc(t, 0)
	old := a.Generation()
	register(a, 0)
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	hand(a, old, 0, "h", t0+1_000, t0+1_000, t0+1_000)  // tagged, after the end
	observe(a, 0, 0, "h", t0+2_000, t0+2_000, t0+2_000) // untagged, between writers
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0+min))
	register(a, 0)
	hand(a, a.Generation(), 0, "h", t0+15*min, t0+15*min, t0+15*min)
	flush(t, a, t0+15*min)
	for _, smp := range s.samples(windowFam) {
		if smp.ts == t0 && smp.generation == fmt.Sprint(t0+min) {
			t.Fatalf("the new writer wrote the old writer's count: %+v", smp)
		}
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_after_end_total")
	if !strings.Contains(string(out), `family="`+fam+`"} 2`) {
		t.Fatalf("after_end: %s", out)
	}
}

// intake = folded + late + after_end once the shards are flushed.
func TestTheBooksBalance(t *testing.T) {
	a, _ := newLinAcc(t, 0)
	g := a.Generation()
	register(a, 0)
	hand(a, g, 0, "h", t0+1_000, t0+1_000, t0+1_000)       // folded
	hand(a, g, 0, "h", t0-11*min, t0, t0)                  // late: before the owed range
	hand(a, Generation(1), 0, "h", t0+2_000, t0+2_000, t0) // after the end: another writer
	flush(t, a, t0+2_000)
	want := map[string]string{
		`app_event_time_observations_total{family="` + fam + `",stage="intake"} 3`: "",
		`app_event_time_observations_total{family="` + fam + `",stage="folded"} 1`: "",
		`app_event_time_late_total{family="` + fam + `"} 1`:                        "",
		`app_event_time_after_end_total{family="` + fam + `"} 1`:                   "",
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_observations_total",
		"app_event_time_late_total", "app_event_time_after_end_total")
	for line := range want {
		if !strings.Contains(string(out), line) {
			t.Fatalf("missing %s in\n%s", line, out)
		}
	}
}

// With no faults, over writers that end while workers are still counting,
// every observation is folded into exactly one writer or counted late or after
// the end, and each writer's written data per window equals its terminal
// record. Run with -race.
func TestEveryWritersDataEqualsItsTerminalRecordsAcrossRebalances(t *testing.T) {
	a, s := newLinAcc(t, 0)
	register(a, 0)
	const writers, perWorker, workers = 4, 300, 3
	var total int
	for w := 0; w < writers; w++ {
		g := a.Generation()
		register(a, 0)
		var wg sync.WaitGroup
		for k := 0; k < workers; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					ev := t0 + int64(i%7)*min + int64(k)
					hand(a, g, 0, "h", ev, ev, ev)
				}
			}(k)
		}
		if err := a.Flush(context.Background(), time.UnixMilli(t0+2*min)); err != nil {
			t.Fatal(err)
		}
		if err := a.EndGeneration(context.Background()); err != nil {
			t.Fatal(err)
		}
		wg.Wait() // some counts land after the end
		total += workers * perWorker
		a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0))
		a.Claim(topic, 0).Start(0, 1_000_000)
	}

	data := s.series(windowFam, LabelGeneration)
	rec := s.series(metricTerminal, LabelGeneration, "stage")
	var written float64
	for k, v := range data {
		written += v
		gen, ts, _ := strings.Cut(k, "|")
		if r := rec[gen+"|written|"+ts]; r != v {
			t.Fatalf("writer %s window %s: data %v, record %v", gen, ts, v, r)
		}
	}
	for k, v := range rec {
		gen, rest, _ := strings.Cut(k, "|")
		stage, ts, _ := strings.Cut(rest, "|")
		if stage == "written" && v != data[gen+"|"+ts] {
			t.Fatalf("record %s = %v without matching data %v", k, v, data[gen+"|"+ts])
		}
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_after_end_total", "app_event_time_late_total")
	after := metricValue(string(out), "app_event_time_after_end_total")
	late := metricValue(string(out), "app_event_time_late_total")
	if int(written)+after+late != total {
		t.Fatalf("written %v + after_end %d + late %d != %d", written, after, late, total)
	}
}

func metricValue(text, name string) int {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			var v float64
			fmt.Sscan(line[strings.LastIndexByte(line, ' ')+1:], &v)
			return int(v)
		}
	}
	return 0
}

// A writer that wrote while its gate was open, and ends with it shut again,
// writes no new data but still closes its books over every owed window.
func TestAWriterWhoseGateShutAgainStillWritesItsTerminalRecords(t *testing.T) {
	a, s := newLinAcc(t, 0, 1)
	g := a.Generation()
	register(a, 0)
	a.Claim(topic, 1).Start(-1, 0) // empty and idle until it hands a message
	hand(a, g, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	hand(a, g, 0, "h", t0+3*min, t0+3*min, t0+3*min)
	flush(t, a, t0+3*min)                            // gate open: window t0 written
	hand(a, g, 1, "h", t0+2_000, t0+2_000, t0+3*min) // partition 1 hands, unregistered
	flush(t, a, t0+5*min)                            // shut again: the gauge starts
	if err := a.EndGeneration(context.Background()); !errors.Is(err, errGateHeld) {
		t.Fatalf("err %v", err)
	}
	if got := s.series(windowFam)[fmt.Sprint(t0)]; got != 1 {
		t.Fatalf("window t0 carries %v, want only what was written while open", got)
	}
	rec := s.series(metricTerminal, "stage")
	if rec[fmt.Sprintf("written|%d", t0)] != 1 || rec[fmt.Sprintf("folded|%d", t0)] != 2 {
		t.Fatalf("records of t0: %v", rec)
	}
	for start := t0 - 10*min; start <= t0+61*min; start += min {
		if _, ok := rec[fmt.Sprintf("written|%d", start)]; !ok {
			t.Fatalf("owed window %d has no record", (start-t0)/min)
		}
	}
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_gate_held_seconds")
	if !strings.Contains(string(out), "app_event_time_gate_held_seconds 0\n") {
		t.Fatalf("the gauge outlives the writer: %s", out)
	}
}

func TestATerminalFamilyMustNotBeAHistogram(t *testing.T) {
	c := linCfg
	c.HistogramBuckets = map[string][]float64{fam: {1, 2}}
	if c.Validate() == nil {
		t.Fatal("accepted a histogram terminal family")
	}
}

func TestADistinctMemberForAnEndedWriterIsDistinctLate(t *testing.T) {
	c := linCfg
	c.DistinctHorizon = time.Minute
	s := &linSink{}
	a, err := New(c, "job", "pod-a", 1, s.write, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0))
	old := a.Generation()
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	sh := a.Shard(0)
	st := sh.StampAs(old, topic, 0, Reading{EventMs: t0, PrimaryMs: t0}, t0)
	sh.Distinct(st, "d", "devices", famLabels, []string{"h"}, 7)
	flush(t, a, t0) // between writers: folds the stats only
	out, _ := testutil.CollectAndFormat(a, expfmt.TypeTextPlain, "app_event_time_distinct_late_total",
		"app_event_time_after_end_total")
	if !strings.Contains(string(out), `app_event_time_distinct_late_total{family="devices"} 1`) ||
		strings.Contains(string(out), "after_end_total{") {
		t.Fatalf("got %s", out)
	}
}
