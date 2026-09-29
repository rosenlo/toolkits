package eventtime

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/prompb"
)

const histFam = "app_request_duration_seconds"

func newHistAcc(t *testing.T, buckets []float64, partitions ...int32) (*Accumulator, *sink) {
	t.Helper()
	c := cfg
	if buckets != nil {
		c.HistogramBuckets = map[string][]float64{histFam: buckets}
	}
	s := &sink{}
	a, err := New(c, "job", "pod-a", 2, s.write, func(string) (int, error) { return len(partitions), nil })
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: partitions}, time.UnixMilli(t0))
	for _, p := range partitions {
		a.Claim(topic, p).Start(0, 1_000_000)
	}
	return a, s
}

func observeValue(a *Accumulator, shard int, host string, eventMs, refMs, nowMs int64, v float64) {
	s := a.Shard(shard)
	st := s.Stamp(topic, 0, Reading{EventMs: eventMs, PrimaryMs: refMs}, nowMs)
	s.Observe(st, SeriesKey(histFam, host), histFam, famLabels, []string{host}, v)
}

// bucketSamples returns the le -> value of the named bucket family's written
// samples at ts, the last write winning, as the store keeps the maximum.
func (s *sink) bucketSamples(name string, ts int64) map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64)
	for _, req := range s.reqs {
		for _, series := range req.Timeseries {
			if label(series, "__name__") != name {
				continue
			}
			for _, smp := range series.Samples {
				if smp.Timestamp == ts && smp.Value >= out[label(series, "le")] {
					out[label(series, "le")] = smp.Value
				}
			}
		}
	}
	return out
}

func TestAHistogramWritesCountSumAndCumulativeBuckets(t *testing.T) {
	a, s := newHistAcc(t, []float64{0.1, 1}, 0)
	for i, v := range []float64{0.05, 0.5, 0.5, 3} {
		observeValue(a, i%2, "h", t0+1_000, t0+5*min, t0+5*min, v)
	}
	flush(t, a, t0+5*min)
	if got := s.samples(histFam + "_window_count"); len(got) != 1 || got[0].v != 4 || got[0].ts != t0 {
		t.Fatalf("count = %+v", got)
	}
	if got := s.samples(histFam + "_window_sum"); len(got) != 1 || got[0].v != 4.05 {
		t.Fatalf("sum = %+v", got)
	}
	want := map[string]float64{"0.1": 1, "1": 3, "+Inf": 4}
	got := s.bucketSamples(histFam+"_window_bucket", t0)
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for le, v := range want {
		if got[le] != v {
			t.Fatalf("bucket le=%s = %v, want %v (all %v)", le, got[le], v, got)
		}
	}
	for _, name := range []string{histFam + "_window_count", histFam + "_window_sum", histFam + "_window_bucket"} {
		for _, req := range s.reqs {
			for _, ts := range req.Timeseries {
				if label(ts, "__name__") == name && (label(ts, "instance") != "pod-a" || label(ts, LabelGeneration) == "") {
					t.Fatalf("%s lacks its writer labels: %v", name, ts.Labels)
				}
			}
		}
	}
}

func TestAHistogramWithoutBucketsWritesCountAndSumOnly(t *testing.T) {
	a, s := newHistAcc(t, nil, 0)
	observeValue(a, 0, "h", t0+1_000, t0+5*min, t0+5*min, 2)
	flush(t, a, t0+5*min)
	if len(s.samples(histFam+"_window_count")) != 1 || len(s.samples(histFam+"_window_sum")) != 1 {
		t.Fatal("count or sum missing")
	}
	if got := s.bucketSamples(histFam+"_window_bucket", t0); len(got) != 0 {
		t.Fatalf("buckets written without being configured: %v", got)
	}
}

// A late observation re-writes all of a histogram's series at the same
// timestamp, each no smaller than before, so the store's max keeps the latest.
func TestALateObservationReWritesTheWholeHistogramUpward(t *testing.T) {
	a, s := newHistAcc(t, []float64{1}, 0)
	observeValue(a, 0, "h", t0+1_000, t0+5*min, t0+5*min, 0.5)
	flush(t, a, t0+5*min)
	observeValue(a, 1, "h", t0+2_000, t0+6*min, t0+6*min, 2)
	flush(t, a, t0+6*min)
	count := s.samples(histFam + "_window_count")
	sum := s.samples(histFam + "_window_sum")
	if len(count) != 2 || count[1].v != 2 || count[1].ts != t0 {
		t.Fatalf("count = %+v", count)
	}
	if len(sum) != 2 || sum[1].v != 2.5 || sum[1].v < sum[0].v {
		t.Fatalf("sum = %+v", sum)
	}
	if got := s.bucketSamples(histFam+"_window_bucket", t0); got["1"] != 1 || got["+Inf"] != 2 {
		t.Fatalf("buckets = %v", got)
	}
}

// A negative value would make a re-written sum smaller than the one already
// written, which the store's max would silently discard.
func TestANegativeOrNonFiniteObservationIsRejectedAndCounted(t *testing.T) {
	a, s := newHistAcc(t, nil, 0)
	for _, v := range []float64{-1, math.NaN(), math.Inf(1)} {
		observeValue(a, 0, "h", t0+1_000, t0+5*min, t0+5*min, v)
	}
	observeValue(a, 0, "h", t0+1_000, t0+5*min, t0+5*min, 1)
	flush(t, a, t0+5*min)
	if got := s.samples(histFam + "_window_count"); len(got) != 1 || got[0].v != 1 {
		t.Fatalf("count = %+v", got)
	}
	if n := a.total.rejected[histFam]; n != 3 {
		t.Fatalf("rejected = %d, want 3", n)
	}
}

func TestAHistogramObservationPastSealingIsLate(t *testing.T) {
	a, _ := newHistAcc(t, nil, 0)
	observeValue(a, 0, "h", t0+1_000, t0+30*min, t0+30*min, 1)
	flush(t, a, t0+30*min)
	observeValue(a, 0, "h", t0+2_000, t0+31*min, t0+31*min, 1)
	flush(t, a, t0+31*min)
	if n := a.total.late[histFam]; n != 1 {
		t.Fatalf("late = %d, want 1", n)
	}
}

func newHoldAcc(t *testing.T, hold time.Duration, partitions ...int32) (*Accumulator, *sink) {
	t.Helper()
	c := cfg
	c.MaxHold = hold
	s := &sink{}
	a, err := New(c, "job", "pod-a", 2, s.write, func(string) (int, error) { return len(partitions), nil })
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: partitions}, time.UnixMilli(t0))
	for _, p := range partitions {
		a.Claim(topic, p).Start(0, 1_000_000)
	}
	return a, s
}

// A stuck partition holds the pod back only MaxHold behind processing time.
func TestMaxHoldBoundsHowLongAStuckPartitionHolds(t *testing.T) {
	a, s := newHoldAcc(t, 2*time.Minute, 0, 1)
	observe(a, 0, 0, "h", t0+1_000, t0+10*min, t0+10*min)
	observe(a, 1, 1, "h", t0+1_000, t0+1*min, t0+10*min)
	flush(t, a, t0+10*min)
	wm := s.samples(metricWatermark)
	if len(wm) == 0 {
		t.Fatal("no watermark with max_hold set")
	}
	if want := float64(t0+8*min) / 1000; wm[0].v != want {
		t.Fatalf("watermark %v, want now minus max_hold %v", wm[0].v, want)
	}
	if a.counters.holdReleases != 1 || a.gauges.heldByMaxHold != 1 {
		t.Fatalf("hold releases = %d, held = %d", a.counters.holdReleases, a.gauges.heldByMaxHold)
	}
}

// Without MaxHold the same stuck partition holds the pod back indefinitely.
func TestWithoutMaxHoldAStuckPartitionHolds(t *testing.T) {
	a, s := newHoldAcc(t, 0, 0, 1)
	observe(a, 0, 0, "h", t0+1_000, t0+10*min, t0+10*min)
	observe(a, 1, 1, "h", t0+1_000, t0+1*min, t0+10*min)
	flush(t, a, t0+10*min)
	wm := s.samples(metricWatermark)
	if len(wm) == 0 || wm[0].v != float64(t0+1*min)/1000 {
		t.Fatalf("watermark = %+v, want the stuck partition's", wm)
	}
}

// A partition that has shown no reference time at all no longer blocks the
// watermark forever, and is released no further than the newest reference.
func TestMaxHoldReleasesAPartitionWithNoEvidenceOnlyToTheNewestReference(t *testing.T) {
	a, s := newHoldAcc(t, 2*time.Minute, 0, 1)
	observe(a, 0, 0, "h", t0+1_000, t0+3*min, t0+10*min)
	flush(t, a, t0+10*min)
	wm := s.samples(metricWatermark)
	if len(wm) == 0 {
		t.Fatal("a partition without evidence still blocks the watermark")
	}
	if want := float64(t0+3*min) / 1000; wm[0].v != want {
		t.Fatalf("watermark %v, want the newest reference %v", wm[0].v, want)
	}
}

func newCappedAcc(t *testing.T, rows int) (*Accumulator, *sink) {
	t.Helper()
	c := cfg
	c.MaxRowsPerFlush = rows
	c.WriteBatch = 2
	s := &sink{}
	a, err := New(c, "job", "pod-a", 1, s.write, func(string) (int, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	a.StartGeneration(map[string][]int32{topic: {0}}, time.UnixMilli(t0))
	a.Claim(topic, 0).Start(0, 1_000_000)
	return a, s
}

// A flush that reaches MaxRowsPerFlush writes the oldest windows first and
// publishes no watermark; the next flush carries on and then publishes it.
func TestAFlushCappedByMaxRowsPublishesNoWatermarkUntilItCatchesUp(t *testing.T) {
	a, s := newCappedAcc(t, 3)
	for w := int64(0); w < 3; w++ {
		for _, h := range []string{"a", "b"} {
			observe(a, 0, 0, h, t0+w*min+1_000, t0+10*min, t0+10*min)
		}
	}
	flush(t, a, t0+10*min)
	first := s.samples(windowFam)
	if len(first) < 3 || len(first) >= 6 {
		t.Fatalf("capped flush wrote %d series, want at least 3 and not all 6", len(first))
	}
	for _, smp := range first {
		if smp.ts > t0+1*min {
			t.Fatalf("capped flush wrote window %d before older ones", (smp.ts-t0)/min)
		}
	}
	if len(s.samples(metricWatermark)) != 0 {
		t.Fatal("a capped flush published the watermark")
	}
	if a.counters.cappedFlushes != 1 {
		t.Fatalf("capped flushes = %d", a.counters.cappedFlushes)
	}
	flush(t, a, t0+10*min)
	flush(t, a, t0+10*min)
	if got := len(s.samples(windowFam)); got != 6 {
		t.Fatalf("after catching up %d series written, want 6", got)
	}
	if len(s.samples(metricWatermark)) == 0 {
		t.Fatal("the watermark stayed unpublished after the backlog drained")
	}
}

// Ending a session writes everything, whatever MaxRowsPerFlush says.
func TestEndingASessionIgnoresMaxRows(t *testing.T) {
	a, s := newCappedAcc(t, 1)
	for _, h := range []string{"a", "b", "c"} {
		observe(a, 0, 0, h, t0+1_000, t0+2*min, t0+2*min)
	}
	if err := a.EndGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(s.samples(windowFam)); got != 3 {
		t.Fatalf("end of session wrote %d, want 3", got)
	}
}

func TestTheDefaultNamesAreGeneric(t *testing.T) {
	s := &sink{}
	c := cfg
	c.Names = Names{}
	a, err := New(c, "job", "pod-a", 1, s.write, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.MetricWatermark() != "event_time_watermark_seconds" {
		t.Fatalf("watermark name %q", a.MetricWatermark())
	}
	a.Shard(0).Stamp(topic, 0, Reading{EventMs: t0, PrimaryMs: t0 + 1_000}, t0+2_000)
	flush(t, a, t0+2_000)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(a)
	want := `
# HELP event_time_stamps_total Tracked messages stamped, by the clock their event time came from and the reference clock used.
# TYPE event_time_stamps_total counter
event_time_stamps_total{event_source="event",reference="primary"} 1
event_time_stamps_total{event_source="event",reference="processing"} 0
event_time_stamps_total{event_source="event",reference="secondary"} 0
event_time_stamps_total{event_source="reference_event_ahead",reference="primary"} 0
event_time_stamps_total{event_source="reference_event_ahead",reference="processing"} 0
event_time_stamps_total{event_source="reference_event_ahead",reference="secondary"} 0
event_time_stamps_total{event_source="reference_event_missing",reference="primary"} 0
event_time_stamps_total{event_source="reference_event_missing",reference="processing"} 0
event_time_stamps_total{event_source="reference_event_missing",reference="secondary"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "event_time_stamps_total"); err != nil {
		t.Fatal(err)
	}
	if n, err := testutil.GatherAndCount(reg, "event_time_reference_delay_seconds"); err != nil || n != 1 {
		t.Fatalf("delay series = %d, %v", n, err)
	}
}

func TestConfigValidateRejectsTheNewBounds(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"negative max_hold":      func(c *Config) { c.MaxHold = -time.Second },
		"negative max rows":      func(c *Config) { c.MaxRowsPerFlush = -1 },
		"unsorted buckets":       func(c *Config) { c.HistogramBuckets = map[string][]float64{"x": {1, 0.5}} },
		"infinite bucket":        func(c *Config) { c.HistogramBuckets = map[string][]float64{"x": {math.Inf(1)}} },
		"bad prefix":             func(c *Config) { c.Names.Prefix = "no spaces allowed" },
		"empty class":            func(c *Config) { c.Names.Classes = []string{"ok", ""} },
		"unsorted delay buckets": func(c *Config) { c.DelayBuckets = []float64{2, 1} },
	} {
		c := cfg
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Keep prompb in use for the helpers above even if a test drops it.
var _ = prompb.Label{}
