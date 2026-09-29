package eventtime

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/prompb"
)

const (
	topic = "t"
	fam   = "app_requests_total"
	min   = int64(60_000)
)

var famLabels = []string{"host"}

// t0 is a window boundary, so window arithmetic reads plainly.
var t0 = time.Date(2025, 1, 6, 12, 0, 0, 0, time.UTC).UnixMilli()

type sink struct {
	mu   sync.Mutex
	reqs []*prompb.WriteRequest
	fail bool
}

func (s *sink) write(_ context.Context, req *prompb.WriteRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("write refused")
	}
	s.reqs = append(s.reqs, req)
	return nil
}

type sample struct {
	name, host, generation string
	ts                     int64
	v                      float64
}

func label(ts prompb.TimeSeries, name string) string {
	for _, l := range ts.Labels {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

// samples returns every written sample of the named metric, in write order.
func (s *sink) samples(name string) []sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sample
	for _, req := range s.reqs {
		for _, ts := range req.Timeseries {
			if label(ts, "__name__") != name {
				continue
			}
			for _, smp := range ts.Samples {
				out = append(out, sample{name, label(ts, "host"), label(ts, LabelGeneration), smp.Timestamp, smp.Value})
			}
		}
	}
	return out
}

func (s *sink) reset() {
	s.mu.Lock()
	s.reqs = nil
	s.mu.Unlock()
}

var cfg = Config{
	Window:        time.Minute,
	EarlyLateness: time.Minute,
	MaxLateness:   10 * time.Minute,
	FutureSkew:    time.Minute,
	IdleTimeout:   30 * time.Second,
	Names: Names{
		Prefix:       "app_event_time",
		DelayMetric:  "upload_delay_seconds",
		EventSources: [3]string{"client_ts", "reference_client_missing", "reference_client_ahead"},
		References:   [2]string{"server_ts", "kafka_ts"},
		Classes:      testClasses,
	},
}

// The tests name the clocks and classes as a client-reporting producer would:
// an event clock on the client, a server receive time, then the log's own
// timestamp.
var testClasses = []string{"ok", "http_error", "net_error"}

const (
	ClassOK Class = iota
	ClassHTTPError
	ClassNetError
)

const (
	metricWatermark  = "app_event_time_watermark_seconds"
	metricPartitions = "app_event_time_partitions"
	metricLateness   = "app_event_time_lateness_seconds"
)

func newAcc(t *testing.T, partitions ...int32) (*Accumulator, *sink) {
	t.Helper()
	s := &sink{}
	a, err := New(cfg, "job", "pod-a", 2, s.write, func(string) (int, error) { return len(partitions), nil })
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: partitions}, time.UnixMilli(t0))
	for _, p := range partitions {
		// Behind: a partition with messages outstanding never counts as idle.
		a.Claim(topic, p).Start(0, 1_000_000)
	}
	return a, s
}

// observe counts one request for host with the given client and server times.
func observe(a *Accumulator, shard int, partition int32, host string, clientMs, serverMs, nowMs int64) {
	s := a.Shard(shard)
	st := s.Stamp(topic, partition, Reading{EventMs: clientMs, PrimaryMs: serverMs}, nowMs)
	s.CountLabels(st, fam, famLabels, host)
}

func flush(t *testing.T, a *Accumulator, nowMs int64) {
	t.Helper()
	if err := a.Flush(context.Background(), time.UnixMilli(nowMs)); err != nil {
		t.Fatal(err)
	}
}

const windowFam = "app_requests_window_count"

// A window family must not keep the counter's _total suffix: anything that
// matches cumulative counters by name (a recording rule, a stream aggregation)
// would otherwise match the windows too.
func TestDefaultWindowNameDropsTheCounterSuffix(t *testing.T) {
	if got := DefaultWindowName(fam); got != windowFam {
		t.Fatalf("got %q", got)
	}
	if got := DefaultWindowName("members"); got != "members_window_count" {
		t.Fatalf("a name without _total: got %q", got)
	}
}

func TestStampPicksTheClockAndNeverClampsThePast(t *testing.T) {
	a, _ := newAcc(t, 0)
	s := a.Shard(0)
	now := t0 + 10*min
	cases := []struct {
		name string
		r    Reading
		want int64
	}{
		{"client time", Reading{EventMs: t0 + 5*min + 30_000, PrimaryMs: t0 + 6*min}, t0 + 5*min},
		{"buffered for an hour is kept", Reading{EventMs: t0 - 60*min + 1, PrimaryMs: t0 + 6*min}, t0 - 60*min},
		{"ahead within the skew is kept", Reading{EventMs: t0 + 6*min + 59_000, PrimaryMs: t0 + 6*min}, t0 + 6*min},
		{"ahead beyond the skew takes server time", Reading{EventMs: t0 + 9*min, PrimaryMs: t0 + 6*min}, t0 + 6*min},
		{"no client time takes server time", Reading{PrimaryMs: t0 + 7*min + 1}, t0 + 7*min},
		{"no server time takes Kafka time", Reading{SecondaryMs: t0 + 8*min + 1}, t0 + 8*min},
		{"no reference takes processing time", Reading{}, t0 + 10*min},
	}
	for _, c := range cases {
		if got := s.Stamp(topic, 0, c.r, now).window; got != c.want {
			t.Errorf("%s: window %d, want %d", c.name, (got-t0)/min, (c.want-t0)/min)
		}
	}
	a.foldStatsOnly()
	st := a.total.stamps
	if st[eventPresent][refPrimary] != 3 || st[eventAhead][refPrimary] != 1 ||
		st[eventMissing][refPrimary] != 1 || st[eventMissing][refSecondary] != 1 ||
		st[eventMissing][refProcessing] != 1 {
		t.Fatalf("stamp sources = %v", st)
	}
	// Only the messages with both clocks feed the upload delay.
	if n := a.total.delay[ClassOK].count; n != 4 {
		t.Fatalf("upload-delay observations = %d, want 4", n)
	}
}

func TestAWindowIsWrittenOnceTheWatermarkPassesItsEndPlusEarlyLateness(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+10_000, t0+20_000, t0+20_000) // window t0
	observe(a, 1, 0, "h", t0+50_000, t0+61_000, t0+61_000) // window t0, watermark t0+61s

	// Watermark t0+61s: the window's end plus a minute is not reached.
	flush(t, a, t0+61_000)
	if got := s.samples(windowFam); len(got) != 0 {
		t.Fatalf("written before the early edge: %v", got)
	}

	observe(a, 0, 0, "h", t0+2*min, t0+2*min, t0+2*min) // window t0+2m, watermark t0+2m
	flush(t, a, t0+2*min)
	got := s.samples(windowFam)
	if len(got) != 1 || got[0].ts != t0 || got[0].v != 2 || got[0].host != "h" {
		t.Fatalf("got %+v, want one sample of 2 at the window start", got)
	}
	if got[0].generation != fmt.Sprint(t0) {
		t.Fatalf("generation = %q", got[0].generation)
	}
	// The watermark is published every flush, after the windows it covers.
	wm := s.samples(metricWatermark)
	if len(wm) != 2 || wm[0].v != float64(t0+61_000)/1000 || wm[1].v != float64(t0+2*min)/1000 {
		t.Fatalf("watermark = %+v", wm)
	}
	if p := s.samples(metricPartitions); len(p) != 2 || p[1].v != 1 {
		t.Fatalf("partitions = %+v", p)
	}
}

func TestALateEventReWritesItsWindowWithTheAccumulatedValue(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	observe(a, 0, 0, "other", t0+1_000, t0+1_000, t0+1_000)
	observe(a, 0, 0, "h", t0+2*min, t0+2*min, t0+2*min)
	flush(t, a, t0+2*min)
	s.reset()

	// Buffered on the device: its window was written a minute ago.
	observe(a, 0, 0, "h", t0+5_000, t0+3*min, t0+3*min)
	flush(t, a, t0+3*min)
	got := s.samples(windowFam)
	if len(got) != 1 || got[0].ts != t0 || got[0].v != 2 || got[0].host != "h" {
		t.Fatalf("got %+v, want only h re-written as 2 at the same timestamp", got)
	}
	a.statsMu.Lock()
	rewrites := a.counters.rewrites
	a.statsMu.Unlock()
	if rewrites != 1 {
		t.Fatalf("rewrites = %d", rewrites)
	}
}

func TestAWindowPastMaxLatenessIsSealedAndLaterEventsAreCountedLate(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	now := t0 + 12*min // past the window's end plus 10 minutes
	observe(a, 0, 0, "h", now, now, now)
	flush(t, a, now)
	if got := a.sealedBefore.Load(); got != t0+2*min {
		t.Fatalf("sealed before %d min, want 2", (got-t0)/min)
	}
	s.reset()

	observe(a, 0, 0, "h", t0+2_000, now+1_000, now+1_000)
	flush(t, a, now+1_000)
	for _, smp := range s.samples(windowFam) {
		if smp.ts == t0 {
			t.Fatalf("a sealed window was written again: %+v", smp)
		}
	}
	a.statsMu.Lock()
	late := a.total.late[fam]
	a.statsMu.Unlock()
	if late != 1 {
		t.Fatalf("late = %d, want 1", late)
	}
}

// An event stamped before its window was sealed but folded in after is late,
// not resurrected into a window whose written value is final.
func TestAnEventStampedBeforeSealingButFoldedAfterIsLate(t *testing.T) {
	a, _ := newAcc(t, 0)
	s := a.Shard(0)
	st := s.Stamp(topic, 0, Reading{EventMs: t0 + 1_000, PrimaryMs: t0 + 1_000}, t0+1_000)
	s.CountLabels(st, fam, famLabels, "h")
	// The flush seals window t0 in the same pass that folds the event in, so
	// seal the boundary first and fold afterwards.
	a.sealedBefore.Store(t0 + min)
	flush(t, a, t0+1_000)
	a.statsMu.Lock()
	late := a.total.late[fam]
	a.statsMu.Unlock()
	if late != 1 {
		t.Fatalf("late = %d, want 1", late)
	}
}

func TestTheSlowestPartitionHoldsThePodWatermark(t *testing.T) {
	a, s := newAcc(t, 0, 1)
	observe(a, 0, 0, "h", t0+1_000, t0+5*min, t0+5*min)
	// Partition 1 has processed nothing: no watermark, nothing written.
	flush(t, a, t0+5*min)
	if got := s.samples(windowFam); len(got) != 0 {
		t.Fatalf("written with a partition unaccounted for: %v", got)
	}
	observe(a, 1, 1, "h", t0+1_000, t0+2*min, t0+5*min)
	flush(t, a, t0+5*min)
	wm := s.samples(metricWatermark)
	if len(wm) != 2 || wm[0].v != float64(t0+2*min)/1000 {
		t.Fatalf("watermark = %+v, want partition 1's for both", wm)
	}
	if got := s.samples(windowFam); len(got) != 1 || got[0].v != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestAnIdlePartitionReleasesTheWatermarkAndAStuckOneDoesNot(t *testing.T) {
	a, s := newAcc(t, 0, 1)
	observe(a, 0, 0, "h", t0+1_000, t0+5*min, t0+5*min)

	// Partition 1 is quiet with messages outstanding: stuck, so it holds.
	flush(t, a, t0+5*min)
	if len(s.samples(metricWatermark)) != 0 {
		t.Fatal("a stuck partition released the watermark")
	}

	// Caught up and quiet past the idle timeout: it no longer holds.
	a.Claim(topic, 1).Received(41, 42)
	flush(t, a, t0+5*min)
	wm := s.samples(metricWatermark)
	if len(wm) == 0 {
		t.Fatal("an idle partition still holds the watermark")
	}
	// It releases to now minus the idle timeout, not further.
	if want := float64(t0+5*min-30_000) / 1000; wm[0].v != want {
		t.Fatalf("watermark %v, want %v", wm[0].v, want)
	}
}

func TestThePublishedWatermarkNeverGoesBack(t *testing.T) {
	a, _ := newAcc(t, 0)
	a.Claim(topic, 0).Received(9, 10)
	flush(t, a, t0+5*min) // idle release: t0+5m-30s
	w1 := a.podW
	observe(a, 0, 0, "h", t0+1_000, t0+2*min, t0+5*min) // a straggler with an older reference
	flush(t, a, t0+5*min)
	if a.podW < w1 {
		t.Fatalf("watermark went back from %d to %d", w1, a.podW)
	}
}

func TestAFailedWriteKeepsTheCountsAndPublishesNoWatermark(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+1_000, t0+2*min, t0+2*min)
	s.fail = true
	if err := a.Flush(context.Background(), time.UnixMilli(t0+2*min)); err == nil {
		t.Fatal("a refused write reported success")
	}
	s.fail = false
	if len(s.samples(metricWatermark)) != 0 {
		t.Fatal("a watermark was published over unwritten windows")
	}
	flush(t, a, t0+2*min)
	if got := s.samples(windowFam); len(got) != 1 || got[0].v != 1 {
		t.Fatalf("the retry wrote %+v", got)
	}
}

func TestAnUnwrittenWindowIsNotSealed(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+1_000, t0+12*min, t0+12*min)
	s.fail = true
	_ = a.Flush(context.Background(), time.UnixMilli(t0+12*min))
	if got := a.sealedBefore.Load(); got > t0 {
		t.Fatalf("sealed past an unwritten window: %d min", (got-t0)/min)
	}
	// Past twice max_lateness it is dropped, and counted.
	_ = a.Flush(context.Background(), time.UnixMilli(t0+12*min))
	observe(a, 0, 0, "x", t0+22*min, t0+22*min, t0+22*min)
	_ = a.Flush(context.Background(), time.UnixMilli(t0+22*min))
	a.statsMu.Lock()
	dropped := a.counters.droppedUnwritten[fam]
	a.statsMu.Unlock()
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

func TestEndingASessionWritesEveryWindowAndTheNextWriterStartsFresh(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+1_000, t0+1_000, t0+1_000) // not yet past the early edge
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.samples(windowFam)
	if len(got) != 1 || got[0].v != 1 || got[0].generation != fmt.Sprint(t0) {
		t.Fatalf("got %+v", got)
	}
	s.reset()

	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0+min))
	observe(a, 0, 0, "h", t0+2_000, t0+3*min, t0+3*min)
	flush(t, a, t0+3*min)
	got = s.samples(windowFam)
	// The same window, but a new writer: its series starts at 1 and sums
	// with the old one.
	if len(got) != 1 || got[0].v != 1 || got[0].generation != fmt.Sprint(t0+min) {
		t.Fatalf("got %+v", got)
	}
}

// Every observation is written exactly once or counted late, whatever the
// interleaving of workers and flushes. Run with -race.
func TestConcurrentWorkersAndFlushesConserveEveryObservation(t *testing.T) {
	a, s := newAcc(t, 0, 1, 2, 3)
	const perWorker = 20_000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var clock sync.Mutex
	now := t0
	tick := func() int64 {
		clock.Lock()
		defer clock.Unlock()
		now += 50
		return now
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < perWorker; i++ {
				n := tick()
				p := int32(r.Intn(4))
				delay := int64(r.ExpFloat64() * 120_000) // an upload delay, minutes at the tail
				observe(a, w, p, fmt.Sprint("h", r.Intn(5)), n-delay, n, n)
			}
		}(w)
	}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for {
			select {
			case <-stop:
				return
			default:
				clock.Lock()
				n := now
				clock.Unlock()
				_ = a.Flush(context.Background(), time.UnixMilli(n))
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-flushed
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Each series' final value per window is its last write.
	last := make(map[string]float64)
	for _, smp := range s.samples(windowFam) {
		k := fmt.Sprint(smp.host, smp.ts)
		if smp.v < last[k] {
			t.Fatalf("series %s went down: %v after %v", k, smp.v, last[k])
		}
		last[k] = smp.v
	}
	var written float64
	for _, v := range last {
		written += v
	}
	a.statsMu.Lock()
	late := a.total.late[fam]
	dropped := a.counters.droppedUnwritten[fam]
	a.statsMu.Unlock()
	if total := written + float64(late+dropped); total != 2*perWorker {
		t.Fatalf("written %v + late %d + dropped %d = %v, want %d", written, late, dropped, total, 2*perWorker)
	}
	if late == 0 {
		t.Log("no observation was late; the tail did not reach max_lateness")
	}
}

func TestTheCollectorReportsTheFoldedStats(t *testing.T) {
	a, _ := newAcc(t, 0)
	s := a.Shard(0)
	s.Stamp(topic, 0, Reading{EventMs: t0, PrimaryMs: t0 + 90_000, Class: ClassNetError}, t0+100_000)
	flush(t, a, t0+100_000)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(a)
	if n, err := testutil.GatherAndCount(reg, "app_event_time_upload_delay_seconds"); err != nil || n != len(testClasses) {
		t.Fatalf("upload delay series = %d, %v", n, err)
	}
	want := `
# HELP app_event_time_stamps_total Tracked messages stamped, by the clock their event time came from and the reference clock used.
# TYPE app_event_time_stamps_total counter
app_event_time_stamps_total{event_source="client_ts",reference="kafka_ts"} 0
app_event_time_stamps_total{event_source="client_ts",reference="processing"} 0
app_event_time_stamps_total{event_source="client_ts",reference="server_ts"} 1
app_event_time_stamps_total{event_source="reference_client_ahead",reference="kafka_ts"} 0
app_event_time_stamps_total{event_source="reference_client_ahead",reference="processing"} 0
app_event_time_stamps_total{event_source="reference_client_ahead",reference="server_ts"} 0
app_event_time_stamps_total{event_source="reference_client_missing",reference="kafka_ts"} 0
app_event_time_stamps_total{event_source="reference_client_missing",reference="processing"} 0
app_event_time_stamps_total{event_source="reference_client_missing",reference="server_ts"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "app_event_time_stamps_total"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidate(t *testing.T) {
	bad := map[string]Config{
		"sub-second window":    {Window: 1500 * time.Millisecond, MaxLateness: time.Hour, IdleTimeout: time.Second},
		"max below early+win":  {Window: time.Minute, EarlyLateness: time.Minute, MaxLateness: time.Minute, IdleTimeout: time.Second},
		"no idle timeout":      {Window: time.Minute, MaxLateness: time.Hour},
		"negative future skew": {Window: time.Minute, MaxLateness: time.Hour, FutureSkew: -1, IdleTimeout: time.Second},
	}
	for name, c := range bad {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Review round 1: a partition with no data, or claimed at sarama's
// OffsetNewest sentinel, is caught up from the start and must not hold the pod
// watermark at 0 for the whole session.
func TestAnEmptyOrNewestStartedPartitionIsIdleNotStuck(t *testing.T) {
	for name, start := range map[string][2]int64{
		"empty partition":           {0, 0},
		"new group at OffsetNewest": {-1, 500},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sink{}
			a, err := New(cfg, "job", "pod-a", 1, s.write, nil)
			if err != nil {
				t.Fatal(err)
			}
			a.StartGeneration(map[string][]int32{topic: {0, 1}}, time.UnixMilli(t0))
			a.Claim(topic, 0).Start(0, 1_000_000)
			a.Claim(topic, 1).Start(start[0], start[1])
			observe(a, 0, 0, "h", t0+1_000, t0+5*min, t0+5*min)
			flush(t, a, t0+5*min)
			if len(s.samples(metricWatermark)) == 0 {
				t.Fatal("the quiet partition held the pod watermark")
			}
		})
	}
}

func TestAnOldestStartedPartitionIsBehindUntilItsFirstMessage(t *testing.T) {
	a, s := newAcc(t, 0, 1)
	a.Claim(topic, 1).Start(-2, 500)
	observe(a, 0, 0, "h", t0+1_000, t0+5*min, t0+5*min)
	flush(t, a, t0+5*min)
	if len(s.samples(metricWatermark)) != 0 {
		t.Fatal("a partition with 500 messages still to read released the watermark")
	}
}

// A message with neither server_ts nor a Kafka timestamp must not jump a
// lagging partition's watermark to processing time.
func TestAMessageWithoutAReferenceClockDoesNotMoveTheWatermark(t *testing.T) {
	a, s := newAcc(t, 0)
	observe(a, 0, 0, "h", t0+1_000, t0+2*min, t0+60*min) // 58 min behind
	st := a.Shard(0).Stamp(topic, 0, Reading{}, t0+60*min)
	a.Shard(0).CountLabels(st, fam, famLabels, "no-clock")
	flush(t, a, t0+60*min)
	wm := s.samples(metricWatermark)
	if len(wm) != 1 || wm[0].v != float64(t0+2*min)/1000 {
		t.Fatalf("watermark = %+v, want the lagging reference t0+2m", wm)
	}
}

func TestQueuedMessagesKeepAnIdlePartitionHolding(t *testing.T) {
	a, s := newAcc(t, 0, 1)
	queued := 3
	a.SetPending(func() int { return queued })
	a.Claim(topic, 1).Received(41, 42)
	observe(a, 0, 0, "h", t0+1_000, t0+5*min, t0+5*min)
	flush(t, a, t0+5*min)
	if len(s.samples(metricWatermark)) != 0 {
		t.Fatal("released while handed-on messages were still unprocessed")
	}
	queued = 0
	flush(t, a, t0+5*min)
	if len(s.samples(metricWatermark)) == 0 {
		t.Fatal("still held once the queue drained")
	}
}

// Processing time runs ahead of the reference clock by the pipeline in front
// of Kafka, so an idle release never goes past the newest reference seen.
func TestAnIdleReleaseStaysOnTheReferenceClock(t *testing.T) {
	a, s := newAcc(t, 0, 1)
	// Both partitions caught up and quiet, so only the cap decides.
	a.Claim(topic, 0).Received(9, 10)
	a.Claim(topic, 1).Received(41, 42)
	observe(a, 0, 0, "h", t0+1_000, t0+2*min, t0+9*min) // references 7 min behind processing
	flush(t, a, t0+10*min)
	wm := s.samples(metricWatermark)
	if len(wm) == 0 || wm[0].v != float64(t0+2*min)/1000 {
		t.Fatalf("watermark = %+v, want the newest reference t0+2m, not now-30s", wm)
	}
}

func TestDrainWritesWhatWasCountedAfterTheSessionEnded(t *testing.T) {
	a, s := newAcc(t, 0)
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A worker finishes a queued message after Cleanup.
	observe(a, 0, 0, "h", t0+1_000, t0+1_000, t0+1_000)
	if err := a.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := s.samples(windowFam)
	if len(got) != 1 || got[0].v != 1 || got[0].generation != fmt.Sprint(t0) {
		t.Fatalf("drain wrote %+v, want 1 under the last generation", got)
	}
}

func TestAFlushWritesOnlyWhatChanged(t *testing.T) {
	a, s := newAcc(t, 0)
	for i := 0; i < 50; i++ {
		observe(a, 0, 0, fmt.Sprint("h", i), t0+1_000, t0+2*min, t0+2*min)
	}
	flush(t, a, t0+2*min)
	s.reset()
	observe(a, 0, 0, "h7", t0+2_000, t0+3*min, t0+3*min)
	flush(t, a, t0+3*min)
	if got := s.samples(windowFam); len(got) != 1 || got[0].host != "h7" || got[0].v != 2 {
		t.Fatalf("got %+v, want only h7 re-written", got)
	}
	if n := len(a.dirty); n != 0 {
		t.Fatalf("%d windows still dirty after a successful write", n)
	}
}

// A flush that fails part-way keeps what it wrote: the next flush re-sends only
// the rest, so a backlog larger than one flush can carry still drains. Before,
// the whole backlog stayed dirty and was re-sent every flush, growing each time.
func TestAFlushThatFailsPartWayKeepsWhatItWrote(t *testing.T) {
	c := cfg
	c.WriteBatch = 10
	s := &sink{}
	okLeft := 2 // the first flush gets two requests through, then fails
	write := func(ctx context.Context, req *prompb.WriteRequest) error {
		s.mu.Lock()
		if okLeft == 0 {
			s.mu.Unlock()
			return errors.New("deadline")
		}
		okLeft--
		s.mu.Unlock()
		return s.write(ctx, req)
	}
	a, err := New(c, "job", "pod-a", 2, write, func(string) (int, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0))
	a.Claim(topic, 0).Start(0, 1_000_000)
	// 25 series in the older window, 10 in the newer one.
	for i := 0; i < 25; i++ {
		observe(a, 0, 0, fmt.Sprint("old", i), t0+1_000, t0+3*min, t0+3*min)
	}
	for i := 0; i < 10; i++ {
		observe(a, 0, 0, fmt.Sprint("new", i), t0+min+1_000, t0+3*min, t0+3*min)
	}

	if err := a.Flush(context.Background(), time.UnixMilli(t0+3*min)); err == nil {
		t.Fatal("the third request was refused, yet the flush reported success")
	}
	first := s.samples(windowFam)
	if len(first) != 20 {
		t.Fatalf("first flush wrote %d series, want the two batches of 10", len(first))
	}
	for _, smp := range first {
		if smp.ts != t0 {
			t.Fatalf("a newer window was written before the older one finished: %+v", smp)
		}
	}
	if len(s.samples(metricWatermark)) != 0 {
		t.Fatal("a watermark was published over unwritten windows")
	}

	s.reset()
	okLeft = -1 // no limit
	flush(t, a, t0+3*min)
	second := s.samples(windowFam)
	if len(second) != 15 {
		t.Fatalf("second flush wrote %d series, want only the 15 left unwritten", len(second))
	}
	seen := map[string]bool{}
	for _, smp := range append(first, second...) {
		if seen[smp.host] {
			t.Fatalf("%s written twice", smp.host)
		}
		seen[smp.host] = true
	}
	if len(seen) != 35 || len(a.dirty) != 0 {
		t.Fatalf("written %d of 35, %d windows still dirty", len(seen), len(a.dirty))
	}
	a.statsMu.Lock()
	rows, errs := a.counters.rows, a.counters.windowErrors
	a.statsMu.Unlock()
	if rows != 35 || errs != 1 {
		t.Fatalf("rows = %d, errors = %d; want 35 and 1", rows, errs)
	}
}

const (
	devFam       = "app_failing_members"
	devWindowFam = "app_failing_members_window_count"
)

func newDistinctAcc(t *testing.T, cap int) (*Accumulator, *sink) {
	t.Helper()
	c := cfg
	c.DistinctHorizon = 2 * time.Minute
	c.DistinctCap = cap
	s := &sink{}
	a, err := New(c, "job", "pod-a", 2, s.write, func(string) (int, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0))
	a.Claim(topic, 0).Start(0, 1_000_000)
	return a, s
}

// device records one member for host in the window of clientMs, and moves the
// partition's watermark to serverMs.
func device(a *Accumulator, shard int, host string, member uint64, clientMs, serverMs int64) {
	s := a.Shard(shard)
	st := s.Stamp(topic, 0, Reading{EventMs: clientMs, PrimaryMs: serverMs}, serverMs)
	s.Distinct(st, devFam+"\xff"+host, devFam, famLabels, []string{host}, member)
}

func TestADistinctFamilyCountsEachMemberOnceAcrossShards(t *testing.T) {
	a, s := newDistinctAcc(t, 0)
	device(a, 0, "h", 1, t0+1_000, t0+1_000)
	device(a, 0, "h", 2, t0+2_000, t0+2_000)
	device(a, 1, "h", 2, t0+3_000, t0+3_000) // the same member on the other worker
	device(a, 1, "h", 3, t0+4_000, t0+4_000)
	device(a, 0, "h", 3, t0+2*min, t0+2*min) // next window; watermark t0+2m
	flush(t, a, t0+2*min)
	got := s.samples(devWindowFam)
	if len(got) != 1 || got[0].ts != t0 || got[0].v != 3 || got[0].host != "h" {
		t.Fatalf("got %+v, want 3 distinct members at t0", got)
	}
}

func TestADistinctValueSaturatesAtTheCap(t *testing.T) {
	a, s := newDistinctAcc(t, 3)
	for m := uint64(1); m <= 5; m++ {
		device(a, int(m%2), "h", m, t0+int64(m)*1_000, t0+int64(m)*1_000)
	}
	device(a, 0, "h", 9, t0+2*min, t0+2*min)
	flush(t, a, t0+2*min)
	got := s.samples(devWindowFam)
	if len(got) != 1 || got[0].v != 3 {
		t.Fatalf("got %+v, want the value capped at 3", got)
	}
}

func TestADistinctSeriesIsReWrittenOnlyWhenItGrows(t *testing.T) {
	a, s := newDistinctAcc(t, 0)
	device(a, 0, "h", 1, t0+1_000, t0+1_000)
	device(a, 0, "h", 2, t0+2_000, t0+2_000)
	device(a, 0, "x", 7, t0+2*min, t0+2*min)
	flush(t, a, t0+2*min)
	s.reset()

	// A member it already holds changes nothing, so nothing is written.
	device(a, 1, "h", 2, t0+5_000, t0+2*min+1_000)
	flush(t, a, t0+2*min+1_000)
	if got := s.samples(devWindowFam); len(got) != 0 {
		t.Fatalf("re-written without growing: %+v", got)
	}
	device(a, 1, "h", 3, t0+6_000, t0+2*min+2_000)
	flush(t, a, t0+2*min+2_000)
	got := s.samples(devWindowFam)
	if len(got) != 1 || got[0].ts != t0 || got[0].v != 3 {
		t.Fatalf("got %+v, want h re-written as 3 at the same timestamp", got)
	}
}

func TestAMemberPastTheHorizonIsLateAndTheMembersAreReleased(t *testing.T) {
	a, s := newDistinctAcc(t, 0)
	device(a, 0, "h", 1, t0+1_000, t0+1_000)
	// Window t0 ends at t0+1m; + early 1m + horizon 2m = t0+4m.
	now := t0 + 4*min
	device(a, 0, "x", 9, now, now)
	flush(t, a, now)
	a.mu.Lock()
	_, held := a.members[t0]
	a.mu.Unlock()
	if held {
		t.Fatal("the window's members are still held past the horizon")
	}
	if got := a.distinctBefore.Load(); got != t0+min {
		t.Fatalf("released before %d min, want 1", (got-t0)/min)
	}
	s.reset()

	device(a, 0, "h", 2, t0+2_000, now+1_000)
	flush(t, a, now+1_000)
	for _, smp := range s.samples(devWindowFam) {
		if smp.ts == t0 {
			t.Fatalf("a released window grew: %+v", smp)
		}
	}
	a.statsMu.Lock()
	late := a.total.distinctLate[devFam]
	a.statsMu.Unlock()
	if late != 1 {
		t.Fatalf("distinct late = %d, want 1", late)
	}
	// The count survives the release: the cell still holds the written 1.
	a.mu.Lock()
	var total uint64
	for sr, c := range a.windows[t0] {
		if sr.name == devFam {
			total = c.total
		}
	}
	a.mu.Unlock()
	if total != 1 {
		t.Fatalf("released cell total = %d, want 1", total)
	}
}

func TestWithoutAHorizonDistinctRecordsNothing(t *testing.T) {
	a, s := newAcc(t, 0)
	sh := a.Shard(0)
	st := sh.Stamp(topic, 0, Reading{EventMs: t0 + 1_000, PrimaryMs: t0 + 1_000}, t0+1_000)
	sh.Distinct(st, "k", devFam, famLabels, []string{"h"}, 1)
	observe(a, 0, 0, "h", t0+2*min, t0+2*min, t0+2*min)
	flush(t, a, t0+2*min)
	if got := s.samples(devWindowFam); len(got) != 0 {
		t.Fatalf("written with the distinct families off: %+v", got)
	}
}

func TestADistinctHorizonMustFitInsideMaxLateness(t *testing.T) {
	c := cfg
	c.DistinctHorizon = 9 * time.Minute // 1m early + 9m + 1m window > 10m max
	if err := c.Validate(); err == nil {
		t.Fatal("accepted a horizon that outlives the window")
	}
	c.DistinctHorizon = 8 * time.Minute
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
