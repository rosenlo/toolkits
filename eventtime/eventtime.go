// Package eventtime counts observations from a partitioned log in windows of
// their own event time, and remote-writes each window stamped with that time.
//
// A cumulative counter carries the time it was exported. Under a consumer lag L
// an event lands L late, and unevenly, because lag is per partition. This
// package aligns the counts in the producer instead:
//
//   - Every message has an event clock (when the thing it records happened) and
//     up to two reference clocks (when the pipeline saw it: a primary, such as a
//     server receive time, then a secondary, such as the log's own timestamp),
//     then processing time. An event clock ahead of the reference by more than
//     FutureSkew is replaced by the reference; one behind it is kept however far,
//     because a buffering producer may deliver old events late, and those can be
//     the signal.
//   - Counts are kept per writer, per window, per series. A writer is this
//     process during one consumer-group session (the instance and generation
//     labels), so every written series only ever grows, and writers sum. Never
//     drop either label: the store keeps the maximum of equal-timestamp samples,
//     so two writers on one series would collapse to the larger partial count.
//   - The arrival watermark of a partition is the latest reference time
//     processed on it; the pod's is the minimum over its assigned partitions. A
//     window is first written once the watermark passes its end plus
//     EarlyLateness. After that, any change is re-written at the same timestamp
//     with the larger, accumulated value, until the watermark passes its end
//     plus MaxLateness, when it is sealed. A later event is counted as late,
//     never silently.
//   - The watermark is published per partition, and only after the windows it
//     covers were written, so a reader that takes each partition's maximum and
//     requires every partition can tell how far the counts are complete.
//
// Three kinds of family share the windows. A counter (Shard.Count) counts
// observations. A distinct family (Shard.Distinct) counts members: its value is
// how many distinct members were seen, capped, and its members are held only
// until the watermark passes the window's end plus EarlyLateness plus
// DistinctHorizon, after which its value is frozen. A histogram family
// (Shard.Observe) keeps a count, a sum and optional buckets; its values must not
// be negative, because a re-write can only raise a sample.
//
// Re-writing at an equal timestamp relies on the store keeping the maximum of
// equal-timestamp samples (VictoriaMetrics does, with deduplication on). Check
// that on the target before relying on the re-writes.
//
// The hot path takes only its worker's own shard lock; the flusher is the one
// other party, every flush interval.
package eventtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

// LabelGeneration names the consumer-group session a window series belongs to.
// Its value is the session's start in unix milliseconds, so a restarted
// container, which keeps its hostname, never reuses one.
const LabelGeneration = "generation"

// DefaultWindowName is the windowed family written for a counter family:
// requests_total -> requests_window_count.
func DefaultWindowName(counter string) string {
	return strings.TrimSuffix(counter, "_total") + "_window_count"
}

// Names are what the accumulator calls the series it writes and exposes. The
// zero value takes the defaults.
type Names struct {
	// Prefix starts every series written beside the windows and every
	// self-metric. Default "event_time".
	Prefix string
	// DelayMetric is the self-metric suffix of the primary reference minus the
	// event clock, by class. Default "reference_delay_seconds".
	DelayMetric string
	// EventSources label a stamp by where its event time came from: the event
	// clock, the reference because the event clock was missing, and the
	// reference because the event clock was ahead of it. Default
	// "event", "reference_event_missing", "reference_event_ahead".
	EventSources [3]string
	// References label the primary and secondary reference clocks. Processing
	// time is always "processing". Default "primary", "secondary".
	References [2]string
	// Classes label the outcome classes of the delay histogram; Reading.Class
	// indexes it. Default a single class, "all".
	Classes []string
	// WindowName maps a counter or distinct family to its windowed name.
	// Default DefaultWindowName. A histogram family base is written as
	// base_window_count, base_window_sum and base_window_bucket.
	WindowName func(family string) string
}

func (n Names) withDefaults() Names {
	if n.Prefix == "" {
		n.Prefix = "event_time"
	}
	if n.DelayMetric == "" {
		n.DelayMetric = "reference_delay_seconds"
	}
	if n.EventSources == [3]string{} {
		n.EventSources = [3]string{"event", "reference_event_missing", "reference_event_ahead"}
	}
	if n.References == [2]string{} {
		n.References = [2]string{"primary", "secondary"}
	}
	if len(n.Classes) == 0 {
		n.Classes = []string{"all"}
	}
	if n.WindowName == nil {
		n.WindowName = DefaultWindowName
	}
	return n
}

var metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func (n Names) validate() error {
	for _, s := range []string{n.Prefix, n.Prefix + "_" + n.DelayMetric} {
		if !metricNameRe.MatchString(s) {
			return fmt.Errorf("not a metric name: %q", s)
		}
	}
	if len(n.Classes) > math.MaxUint8 {
		return fmt.Errorf("at most %d classes, got %d", math.MaxUint8, len(n.Classes))
	}
	for _, c := range n.Classes {
		if c == "" {
			return errors.New("a class name must not be empty")
		}
	}
	for _, s := range n.EventSources {
		if s == "" {
			return errors.New("an event source name must not be empty")
		}
	}
	for _, s := range n.References {
		if s == "" {
			return errors.New("a reference name must not be empty")
		}
	}
	return nil
}

// Config bounds the windows. Validate before use.
type Config struct {
	Window        time.Duration
	EarlyLateness time.Duration
	MaxLateness   time.Duration
	FutureSkew    time.Duration
	IdleTimeout   time.Duration
	// MaxHold bounds how far the pod's watermark may trail processing time on
	// account of any one partition: a partition that is stuck, or that has shown
	// no reference time at all, holds the pod back at most this long. What it
	// then delivers for the windows already written becomes re-writes, or late
	// once sealed. Zero leaves the watermark unbounded.
	MaxHold time.Duration
	// WriteBatch is the most series one write request carries. Each request is
	// committed on its own, so a flush that runs out of time keeps what it
	// wrote. Zero takes DefaultWriteBatch.
	WriteBatch int
	// MaxRowsPerFlush bounds the series one flush writes, oldest window first.
	// A flush that reaches it publishes no watermark, and the next one carries
	// on. Zero leaves it unbounded.
	MaxRowsPerFlush int
	// DistinctHorizon is how long past EarlyLateness a distinct family's
	// members are held. Zero leaves the distinct families off: Distinct then
	// records nothing.
	DistinctHorizon time.Duration
	// DistinctCap is the most members one distinct series holds per window, so
	// its value saturates there. Zero takes DefaultDistinctCap.
	DistinctCap int
	// HistogramBuckets are the upper bounds written for a histogram family, by
	// family base name. A family without an entry writes count and sum only.
	HistogramBuckets map[string][]float64
	// DelayBuckets bound the delay self-metric. Default DefaultDelayBuckets.
	DelayBuckets []float64
	// ArrivalLagBuckets bound the arrival-lag self-metric. Default
	// DefaultArrivalLagBuckets.
	ArrivalLagBuckets []float64
	// Lineage turns on what a reader needs to prove a window final: the
	// observation counters, terminal and claim records, the write gate, the
	// writer tag on a stamp, and the generation label on the watermark (see
	// lineage.go). Off, nothing written or exposed changes.
	Lineage bool
	// TerminalFamilies are the counter families a terminal record is written
	// for, by the name passed to Count. Only with Lineage. A distinct or
	// histogram family named here has no counter cells, so its records would
	// read zero: name counter families only.
	TerminalFamilies []string

	Names Names
}

// DefaultDistinctCap bounds a distinct series' members per window.
const DefaultDistinctCap = 64

// DefaultWriteBatch is the default number of series per write request.
const DefaultWriteBatch = 10000

var (
	// DefaultDelayBuckets span an event clock ahead of the reference (negative)
	// to a day behind it.
	DefaultDelayBuckets = []float64{-600, -60, -10, -1, 0, 1, 5, 15, 30, 60, 120, 300, 600, 1800,
		3600, 7200, 10800, 14400, 18000, 21600, 86400}
	// DefaultArrivalLagBuckets measure processing time behind the reference
	// clock: the consumer's lag plus the pipeline in front of it.
	DefaultArrivalLagBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200}
)

// Validate reports a configuration the accumulator cannot honour.
func (c Config) Validate() error {
	switch {
	case c.Window < time.Second || c.Window%time.Second != 0:
		return fmt.Errorf("window must be a whole number of seconds, at least 1s: %v", c.Window)
	case c.EarlyLateness < 0:
		return fmt.Errorf("early_lateness must not be negative: %v", c.EarlyLateness)
	case c.MaxLateness < c.EarlyLateness+c.Window:
		return fmt.Errorf("max_lateness (%v) must be at least early_lateness plus one window (%v)",
			c.MaxLateness, c.EarlyLateness+c.Window)
	case c.FutureSkew < 0:
		return fmt.Errorf("future_skew must not be negative: %v", c.FutureSkew)
	case c.IdleTimeout <= 0:
		return fmt.Errorf("idle_timeout must be positive: %v", c.IdleTimeout)
	case c.MaxHold < 0:
		return fmt.Errorf("max_hold must not be negative: %v", c.MaxHold)
	case c.WriteBatch < 0:
		return fmt.Errorf("write batch must not be negative: %d", c.WriteBatch)
	case c.MaxRowsPerFlush < 0:
		return fmt.Errorf("max rows per flush must not be negative: %d", c.MaxRowsPerFlush)
	case c.DistinctHorizon < 0:
		return fmt.Errorf("distinct horizon must not be negative: %v", c.DistinctHorizon)
	case c.DistinctHorizon > 0 && c.EarlyLateness+c.DistinctHorizon+c.Window > c.MaxLateness:
		return fmt.Errorf("early_lateness plus the distinct horizon plus one window (%v) must not pass max_lateness (%v)",
			c.EarlyLateness+c.DistinctHorizon+c.Window, c.MaxLateness)
	case c.DistinctCap < 0:
		return fmt.Errorf("distinct cap must not be negative: %d", c.DistinctCap)
	case len(c.TerminalFamilies) > 0 && !c.Lineage:
		return errors.New("terminal families need lineage")
	}
	for _, f := range c.TerminalFamilies {
		if _, ok := c.HistogramBuckets[f]; ok {
			return fmt.Errorf("terminal family %s is a histogram: terminal records are for counter families", f)
		}
	}
	for family, bounds := range c.HistogramBuckets {
		if err := checkBounds(bounds); err != nil {
			return fmt.Errorf("histogram buckets of %s: %w", family, err)
		}
	}
	for _, b := range [][]float64{c.DelayBuckets, c.ArrivalLagBuckets} {
		if err := checkBounds(b); err != nil {
			return err
		}
	}
	return c.Names.withDefaults().validate()
}

func checkBounds(bounds []float64) error {
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			return fmt.Errorf("bound %v is not finite", b)
		}
		if i > 0 && b <= bounds[i-1] {
			return fmt.Errorf("bounds must ascend strictly: %v", bounds)
		}
	}
	return nil
}

// Writer delivers one remote-write request, retries included.
type Writer func(ctx context.Context, req *prompb.WriteRequest) error

// Class indexes Names.Classes.
type Class uint8

// Reading is one message's clocks in unix milliseconds; 0 means absent.
type Reading struct {
	EventMs     int64
	PrimaryMs   int64
	SecondaryMs int64
	Class       Class
}

// Stamp is the event-time window a message was assigned to.
type Stamp struct {
	window int64
	// gen is the writer the message was received under, 0 when untagged
	// (StampAs). With Lineage a tagged count for any other writer is not
	// stored.
	gen int64
}

const (
	eventPresent = iota
	eventMissing
	eventAhead
	numEventSources
)

const (
	refPrimary = iota
	refSecondary
	refProcessing
	numRefSources
)

type partKey struct {
	topic     string
	partition int32
}

// Claim is a partition's consumption position, written by its consuming
// goroutine and read by the flusher to tell an idle partition from a stuck one.
type Claim struct {
	received atomic.Int64 // offset of the last message received
	hwm      atomic.Int64 // the claim's high-water mark at that time
	handed   atomic.Bool  // a message was handed on this session (Lineage)
}

// Start records where the claim begins, before its first message. An initial
// offset may be a sentinel: OffsetNewest (-1) starts at the high-water mark, so
// the claim begins caught up; OffsetOldest (-2), or any other, begins behind
// until its first message says where it is. A claim outlives its session, so
// nothing a previous session received is kept.
func (c *Claim) Start(initialOffset, highWaterMark int64) {
	switch {
	case initialOffset >= 0:
		c.received.Store(initialOffset - 1)
	case initialOffset == OffsetNewest:
		c.received.Store(highWaterMark - 1)
	default:
		c.received.Store(-1)
	}
	c.hwm.Store(highWaterMark)
	c.handed.Store(false)
}

// OffsetNewest and OffsetOldest are the conventional Kafka client sentinels for
// an initial offset, kept here so the package needs no Kafka client.
const (
	OffsetNewest = -1
	OffsetOldest = -2
)

// Received records one message handed on for processing.
func (c *Claim) Received(offset, highWaterMark int64) {
	c.received.Store(offset)
	c.hwm.Store(highWaterMark)
}

// caughtUp reports whether the claim has handed on everything up to its
// high-water mark. An empty partition (high-water mark 0) is caught up.
func (c *Claim) caughtUp() bool {
	hwm := c.hwm.Load()
	return hwm >= 0 && c.received.Load()+1 >= hwm
}

type kind uint8

const (
	kindCounter kind = iota
	kindDistinct
	kindHistogram
)

type delta struct {
	name        string
	labelNames  []string
	labelValues []string
	n           uint64
}

// hdelta is one histogram series' observations in one window of one shard.
type hdelta struct {
	name        string
	labelNames  []string
	labelValues []string
	bounds      []float64
	n           uint64
	sum         float64
	buckets     []uint64 // per bucket, not cumulative; the last is +Inf
}

type partSeen struct {
	maxRef int64
	lastAt int64
}

// dset is one distinct series' members in one window of one shard.
type dset struct {
	name        string
	labelNames  []string
	labelValues []string
	members     map[uint64]struct{}
}

// Shard is one worker's share of the counts since the last flush.
type Shard struct {
	acc *Accumulator

	mu        sync.Mutex
	windows   map[int64]map[string]*delta
	lastStart int64
	lastWin   map[string]*delta
	distinct  map[int64]map[string]*dset
	hist      map[int64]map[string]*hdelta
	// parts is keyed by topic, then partition: nearly always one topic, so
	// the per-message lookup is an int map behind a cached topic.
	parts     map[string]map[int32]*partSeen
	lastTopic string
	lastParts map[int32]*partSeen
	stats     *stats
}

func (s *Shard) reset() {
	s.windows = make(map[int64]map[string]*delta)
	s.lastWin = nil
	s.distinct = make(map[int64]map[string]*dset)
	s.hist = make(map[int64]map[string]*hdelta)
	s.parts = make(map[string]map[int32]*partSeen)
	s.lastParts = nil
	s.stats = s.acc.newStats()
}

// Stamp assigns a message to its window and records its clocks.
func (s *Shard) Stamp(topic string, partition int32, r Reading, nowMs int64) Stamp {
	a := s.acc
	ref, refSrc := r.PrimaryMs, refPrimary
	if ref <= 0 {
		ref, refSrc = r.SecondaryMs, refSecondary
	}
	if ref <= 0 {
		ref, refSrc = nowMs, refProcessing
	}
	ev, evSrc := r.EventMs, eventPresent
	switch {
	case ev <= 0:
		ev, evSrc = ref, eventMissing
	case ev > ref+a.futureMs:
		ev, evSrc = ref, eventAhead
	}
	arrival := ref
	if arrival > nowMs {
		arrival = nowMs
	}
	if refSrc == refProcessing {
		// A message without a reference clock says nothing about how far the
		// partition has got: processing time would jump a lagging partition's
		// watermark to now.
		arrival = 0
	}

	s.mu.Lock()
	st := s.stats
	st.stamps[evSrc][refSrc]++
	if r.EventMs > 0 && r.PrimaryMs > 0 && int(r.Class) < len(st.delay) {
		st.delay[r.Class].observe(float64(r.PrimaryMs-r.EventMs) / 1000)
	}
	st.arrivalLag.observe(float64(nowMs-ref) / 1000)
	parts := s.lastParts
	if parts == nil || s.lastTopic != topic {
		parts = s.parts[topic]
		if parts == nil {
			parts = make(map[int32]*partSeen)
			s.parts[topic] = parts
		}
		s.lastTopic, s.lastParts = topic, parts
	}
	p := parts[partition]
	if p == nil {
		p = &partSeen{}
		parts[partition] = p
	}
	if arrival > p.maxRef {
		p.maxRef = arrival
	}
	p.lastAt = nowMs
	s.mu.Unlock()

	return Stamp{window: ev - ev%a.winMs}
}

// Count adds one observation to a counter series in the stamp's window. key
// must identify the series (name and label values) across every family kind,
// and labelValues must not be modified afterwards; they are copied only when
// the series first appears.
func (s *Shard) Count(st Stamp, key, name string, labelNames, labelValues []string) {
	late := st.window < s.acc.sealedBefore.Load()
	s.mu.Lock()
	if s.acc.lin.on && s.afterEnd(st, name) {
		s.mu.Unlock()
		return
	}
	if late {
		s.stats.late[name]++
		s.mu.Unlock()
		return
	}
	win := s.lastWin
	if win == nil || s.lastStart != st.window {
		win = s.windows[st.window]
		if win == nil {
			win = make(map[string]*delta)
			s.windows[st.window] = win
		}
		s.lastStart, s.lastWin = st.window, win
	}
	d := win[key]
	if d == nil {
		d = &delta{name: name, labelNames: labelNames, labelValues: append([]string(nil), labelValues...)}
		win[key] = d
	}
	d.n++
	s.mu.Unlock()
}

// SeriesKey builds a key for Count, Distinct and Observe from a family name and
// its label values.
func SeriesKey(name string, labelValues ...string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, v := range labelValues {
		b.WriteByte(0xff)
		b.WriteString(v)
	}
	return b.String()
}

// CountLabels is Count for a caller that has no series key of its own.
func (s *Shard) CountLabels(st Stamp, name string, labelNames []string, labelValues ...string) {
	s.Count(st, SeriesKey(name, labelValues...), name, labelNames, labelValues)
}

// Distinct adds member to a distinct series in the stamp's window: the series'
// value is the number of distinct members, capped at DistinctCap. key, name and
// the labels follow Count's rules. A member for a window whose members were
// already released is counted as late. With DistinctHorizon zero it records
// nothing.
func (s *Shard) Distinct(st Stamp, key, name string, labelNames, labelValues []string, member uint64) {
	a := s.acc
	if a.distinctMs <= 0 {
		return
	}
	late := st.window < a.distinctBefore.Load() || st.window < a.sealedBefore.Load()
	s.mu.Lock()
	if a.lin.on && s.staleDistinct(st, name) {
		s.mu.Unlock()
		return
	}
	if late {
		s.stats.distinctLate[name]++
		s.mu.Unlock()
		return
	}
	win := s.distinct[st.window]
	if win == nil {
		win = make(map[string]*dset)
		s.distinct[st.window] = win
	}
	d := win[key]
	if d == nil {
		d = &dset{name: name, labelNames: labelNames, labelValues: append([]string(nil), labelValues...),
			members: make(map[uint64]struct{})}
		win[key] = d
	}
	// A shard past the cap cannot move the union below it, so it stops adding.
	if len(d.members) < a.distinctCap {
		d.members[member] = struct{}{}
	}
	s.mu.Unlock()
}

// Observe adds value to a histogram series in the stamp's window: its count,
// its sum, and the bucket HistogramBuckets gives its family. key, name and the
// labels follow Count's rules. A negative or non-finite value is rejected and
// counted, because a re-write can only raise a sample, so a sum that fell would
// be lost.
func (s *Shard) Observe(st Stamp, key, name string, labelNames, labelValues []string, value float64) {
	a := s.acc
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		s.mu.Lock()
		s.stats.rejected[name]++
		s.mu.Unlock()
		return
	}
	late := st.window < a.sealedBefore.Load()
	s.mu.Lock()
	if a.lin.on && s.afterEnd(st, name) {
		s.mu.Unlock()
		return
	}
	if late {
		s.stats.late[name]++
		s.mu.Unlock()
		return
	}
	win := s.hist[st.window]
	if win == nil {
		win = make(map[string]*hdelta)
		s.hist[st.window] = win
	}
	d := win[key]
	if d == nil {
		bounds := a.histBuckets[name]
		d = &hdelta{name: name, labelNames: labelNames, labelValues: append([]string(nil), labelValues...),
			bounds: bounds}
		if bounds != nil {
			d.buckets = make([]uint64, len(bounds)+1)
		}
		win[key] = d
	}
	d.n++
	d.sum += value
	if d.buckets != nil {
		d.buckets[sort.SearchFloat64s(d.bounds, value)]++
	}
	s.mu.Unlock()
}

// series is one label set, held once however many windows count it: with a
// long MaxLateness a series has a cell in hundreds of windows, and a cell is
// then a pointer and two counters rather than a copy of the labels.
type series struct {
	key         string
	name        string
	kind        kind
	labelNames  []string
	labelValues []string
	bounds      []float64 // histogram only
	cells       int       // windows holding a cell for it
}

// cell is one series' count in one window. For a histogram series total is its
// observation count, and its sum and buckets live in Accumulator.hist.
type cell struct {
	total   uint64
	written uint64
}

// hcell is the rest of a histogram series' state in one window.
type hcell struct {
	sum     float64
	buckets []uint64 // per bucket, not cumulative; the last is +Inf
}

type partState struct {
	claim  *Claim
	maxRef int64
	lastAt int64
}

// Accumulator owns the windows of the current writer.
type Accumulator struct {
	winMs, earlyMs, maxMs, futureMs, idleMs, holdMs int64
	writeBatch, maxRows                             int
	distinctMs                                      int64
	distinctCap                                     int
	histBuckets                                     map[string][]float64
	delayBuckets, lagBuckets                        []float64
	names                                           Names
	descs                                           descs
	linDescs                                        lineageDescs

	instance string
	jobName  string
	// pending reports messages handed on but not yet processed. A partition
	// counts as idle only when none are, since the handed-on ones may be its.
	pending    func() int
	write      Writer
	partitions func(topic string) (int, error)
	shards     []*Shard

	// sealedBefore is the first window start not yet sealed; the hot path
	// counts an event before it as late.
	sealedBefore atomic.Int64
	// distinctBefore is the first window start whose distinct members are
	// still held; the hot path counts a member before it as late.
	distinctBefore atomic.Int64

	claimMu sync.Mutex
	claims  map[partKey]*Claim

	mu           sync.Mutex
	generation   string
	generationMs int64
	windows      map[int64]map[*series]cell // nil outside a session
	hist         map[int64]map[*series]*hcell
	series       map[string]*series
	// dirty holds, per window, the series whose count changed since their
	// last write, so a flush costs what changed rather than every open cell.
	dirty map[int64]map[*series]struct{}
	// members holds, per window, each distinct series' members until the
	// window is frozen; its cell's total is then the only record.
	members     map[int64]map[*series]map[uint64]struct{}
	parts       map[partKey]*partState
	podW        int64
	windowNames map[string]string
	// statsMu guards what the scrape reads, so a scrape never waits on a
	// flush's remote write.
	statsMu  sync.Mutex
	total    *stats
	gauges   gauges
	counters counters

	lin lineage
}

type counters struct {
	rows             uint64
	rewrites         uint64
	windowErrors     uint64
	watermarkErrors  uint64
	holdReleases     uint64
	cappedFlushes    uint64
	droppedUnwritten map[string]uint64
	claimErrors      uint64
	terminalErrors   uint64
}

type gauges struct {
	openWindows   int
	openSeries    int
	openCells     int
	openMembers   int
	heldByMaxHold int
	watermarkMs   int64
	sealedBefore  int64
}

// New builds an accumulator with one shard per worker. jobName and instance
// label every series written; partitions, if not nil, reports a topic's
// partition count beside the watermark, so a reader can prove coverage.
func New(cfg Config, jobName, instance string, workers int, write Writer,
	partitions func(topic string) (int, error)) (*Accumulator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if workers < 1 {
		workers = 1
	}
	if cfg.WriteBatch == 0 {
		cfg.WriteBatch = DefaultWriteBatch
	}
	if cfg.DistinctCap == 0 {
		cfg.DistinctCap = DefaultDistinctCap
	}
	if cfg.DelayBuckets == nil {
		cfg.DelayBuckets = DefaultDelayBuckets
	}
	if cfg.ArrivalLagBuckets == nil {
		cfg.ArrivalLagBuckets = DefaultArrivalLagBuckets
	}
	names := cfg.Names.withDefaults()
	hb := make(map[string][]float64, len(cfg.HistogramBuckets))
	for k, v := range cfg.HistogramBuckets {
		hb[k] = append([]float64(nil), v...)
	}
	a := &Accumulator{
		winMs:        cfg.Window.Milliseconds(),
		earlyMs:      cfg.EarlyLateness.Milliseconds(),
		maxMs:        cfg.MaxLateness.Milliseconds(),
		futureMs:     cfg.FutureSkew.Milliseconds(),
		idleMs:       cfg.IdleTimeout.Milliseconds(),
		holdMs:       cfg.MaxHold.Milliseconds(),
		writeBatch:   cfg.WriteBatch,
		maxRows:      cfg.MaxRowsPerFlush,
		distinctMs:   cfg.DistinctHorizon.Milliseconds(),
		distinctCap:  cfg.DistinctCap,
		histBuckets:  hb,
		delayBuckets: cfg.DelayBuckets,
		lagBuckets:   cfg.ArrivalLagBuckets,
		names:        names,
		descs:        newDescs(names),
		linDescs:     newLineageDescs(names),
		instance:     instance,
		jobName:      jobName,
		write:        write,
		partitions:   partitions,
		claims:       make(map[partKey]*Claim),
		windowNames:  make(map[string]string),
		counters:     counters{droppedUnwritten: make(map[string]uint64)},
	}
	a.lin.init(cfg)
	a.total = a.newStats()
	for i := 0; i < workers; i++ {
		s := &Shard{acc: a}
		s.reset()
		a.shards = append(a.shards, s)
	}
	return a, nil
}

// SetPending installs the count of messages handed on but not processed.
func (a *Accumulator) SetPending(pending func() int) {
	a.pending = pending
}

// Shard returns worker i's shard; nil on a nil accumulator, which leaves the
// caller's event-time recording off.
func (a *Accumulator) Shard(i int) *Shard {
	if a == nil {
		return nil
	}
	return a.shards[i%len(a.shards)]
}

// Claim returns the position record for a partition.
func (a *Accumulator) Claim(topic string, partition int32) *Claim {
	k := partKey{topic, partition}
	a.claimMu.Lock()
	defer a.claimMu.Unlock()
	c := a.claims[k]
	if c == nil {
		c = &Claim{}
		c.hwm.Store(-1) // unknown until Start
		c.received.Store(-1)
		a.claims[k] = c
	}
	return c
}

func (a *Accumulator) openSession() {
	a.windows = make(map[int64]map[*series]cell)
	a.hist = make(map[int64]map[*series]*hcell)
	a.series = make(map[string]*series)
	a.dirty = make(map[int64]map[*series]struct{})
	a.members = make(map[int64]map[*series]map[uint64]struct{})
}

func (a *Accumulator) closeSession() {
	a.windows, a.hist, a.series, a.dirty, a.members = nil, nil, nil, nil, nil
}

// StartGeneration opens a new writer for a session's assigned partitions.
func (a *Accumulator) StartGeneration(assigned map[string][]int32, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	genMs := now.UnixMilli()
	if a.lin.on && genMs <= a.generationMs {
		// Unique in the process: a writer id names one session.
		genMs = a.generationMs + 1
	}
	a.generationMs = genMs
	a.generation = strconv.FormatInt(a.generationMs, 10)
	a.openSession()
	a.parts = make(map[partKey]*partState)
	for topic, partitions := range assigned {
		for _, p := range partitions {
			k := partKey{topic, p}
			a.parts[k] = &partState{claim: a.Claim(topic, p)}
		}
	}
	a.podW = 0
	a.sealedBefore.Store(0)
	a.distinctBefore.Store(0)
	if a.lin.on {
		a.startLineage()
	}
}

// EndGeneration writes every changed window, including those the watermark
// has not reached, and closes the writer: its partitions are gone, and the
// next session's writer starts its own series. It reports a failed write,
// whose counts are lost with the session.
func (a *Accumulator) EndGeneration(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.windows == nil {
		return nil
	}
	if a.lin.on {
		// Before the merge takes the shard locks: a count either lands in it
		// or is seen as after the end, never in the next writer.
		a.lin.current.Store(0)
	}
	a.merge()
	var err error
	if a.lin.on {
		err = a.endLineage(ctx)
	} else {
		err = a.writeAllOrDrop(ctx)
	}
	a.closeSession()
	a.parts = nil
	a.podW = 0
	a.sealedBefore.Store(0)
	a.distinctBefore.Store(0)
	a.refreshGauges(time.Now().UnixMilli())
	return err
}

// Drain writes what the workers counted after the session ended — the
// messages still queued when it closed — under the last session's writer. Call
// it at shutdown, once the workers have stopped; inside a session it does
// nothing, since the next flush carries those counts. With Lineage it writes
// nothing: the last writer's terminal records are already written, so those
// counts go on after_end_total instead (drain the queue before EndGeneration).
func (a *Accumulator) Drain(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.windows != nil {
		return nil
	}
	if a.lin.on {
		// The last writer's terminal records are written: what the workers
		// counted after them is counted, never written under it.
		a.discardShards()
		a.refreshGauges(time.Now().UnixMilli())
		return nil
	}
	if a.generation == "" {
		a.generation = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	a.openSession()
	a.merge()
	err := a.writeAllOrDrop(ctx)
	a.closeSession()
	a.refreshGauges(time.Now().UnixMilli())
	return err
}

// writeAllOrDrop writes every changed window, ignoring MaxRowsPerFlush; on
// failure the unwritten counts are counted as dropped, never lost silently. The
// caller holds a.mu.
func (a *Accumulator) writeAllOrDrop(ctx context.Context) error {
	_, err := a.writeWindows(ctx, math.MaxInt64, 0)
	if err != nil {
		a.dropDirty()
	}
	return err
}

// dropDirty counts every unwritten count as dropped. The caller holds a.mu.
func (a *Accumulator) dropDirty() {
	for start, set := range a.dirty {
		win := a.windows[start]
		for sr := range set {
			c := win[sr]
			a.addDropped(sr.name, c.total-c.written)
		}
	}
}

// Flush folds the shards in, writes the windows the watermark has passed, and
// seals the windows past MaxLateness.
func (a *Accumulator) Flush(ctx context.Context, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	nowMs := now.UnixMilli()
	defer a.refreshGauges(nowMs)
	if a.windows == nil {
		// Between sessions: the shards keep their counts for the next writer.
		a.foldStatsOnly()
		return nil
	}
	a.merge()
	var claimErr error
	gated := false
	if a.lin.on {
		claimErr = a.writeClaims(ctx, nowMs)
		gated = a.gateShut()
		a.noteGate(gated, nowMs)
	}
	w, ok := a.watermark(nowMs)
	if !ok {
		return claimErr
	}
	if gated {
		// Nothing is written until every contributing partition names this
		// writer; windows past twice max_lateness are still dropped and
		// counted, and their terminal pairs kept.
		a.release(w)
		a.seal(w)
		return claimErr
	}
	capped, err := a.writeWindows(ctx, w-a.earlyMs, a.maxRows)
	if err != nil {
		a.release(w)
		a.seal(w)
		return errors.Join(claimErr, err)
	}
	if capped {
		// Windows the watermark has passed are still unwritten, so it must not
		// be published: a reader would take them as complete.
		a.statsMu.Lock()
		a.counters.cappedFlushes++
		a.statsMu.Unlock()
		a.release(w)
		a.seal(w)
		return claimErr
	}
	err = a.publishWatermark(ctx, w, nowMs)
	a.release(w)
	a.seal(w)
	if a.lin.on {
		err = errors.Join(claimErr, err, a.writeTerminals(ctx, a.sealedBefore.Load()))
	}
	return err
}

// merge folds every shard's counts into the windows. The caller holds a.mu.
func (a *Accumulator) merge() {
	sealed := a.sealedBefore.Load()
	for _, s := range a.shards {
		s.mu.Lock()
		windows, parts, st, distinct, hist := s.windows, s.parts, s.stats, s.distinct, s.hist
		s.reset()
		s.mu.Unlock()

		a.foldStats(st)
		for topic, byPartition := range parts {
			for partition, seen := range byPartition {
				ps := a.parts[partKey{topic, partition}]
				if ps == nil {
					continue // not this session's partition
				}
				if seen.maxRef > ps.maxRef {
					ps.maxRef = seen.maxRef
				}
				if seen.lastAt > ps.lastAt {
					ps.lastAt = seen.lastAt
				}
			}
		}
		for start, deltas := range windows {
			if start < sealed {
				// Stamped before its window was sealed, folded in after.
				a.statsMu.Lock()
				for _, d := range deltas {
					a.total.late[d.name] += d.n
				}
				a.statsMu.Unlock()
				continue
			}
			win := a.window(start, len(deltas))
			for key, d := range deltas {
				if a.lin.on {
					a.lin.folded[d.name] += d.n
				}
				sr := a.seriesFor(key, d.name, kindCounter, d.labelNames, d.labelValues, nil)
				c, ok := win[sr]
				if !ok {
					sr.cells++
				}
				c.total += d.n
				win[sr] = c
				a.markDirty(start, sr)
			}
		}
		a.mergeDistinct(distinct, sealed)
		a.mergeHist(hist, sealed)
	}
}

func (a *Accumulator) window(start int64, hint int) map[*series]cell {
	win := a.windows[start]
	if win == nil {
		win = make(map[*series]cell, hint)
		a.windows[start] = win
	}
	return win
}

func (a *Accumulator) seriesFor(key, name string, k kind, labelNames, labelValues []string, bounds []float64) *series {
	sr := a.series[key]
	if sr == nil {
		sr = &series{key: key, name: name, kind: k, labelNames: labelNames, labelValues: labelValues, bounds: bounds}
		a.series[key] = sr
	}
	return sr
}

func (a *Accumulator) markDirty(start int64, sr *series) {
	set := a.dirty[start]
	if set == nil {
		set = make(map[*series]struct{})
		a.dirty[start] = set
	}
	set[sr] = struct{}{}
}

// mergeDistinct folds one shard's distinct members in. A series' cell total is
// its member count, so it only grows, and it is dirty only when it grew. The
// caller holds a.mu.
func (a *Accumulator) mergeDistinct(distinct map[int64]map[string]*dset, sealed int64) {
	released := a.distinctBefore.Load()
	for start, sets := range distinct {
		if start < sealed || start < released {
			// Stamped before its window was released, folded in after.
			a.statsMu.Lock()
			for _, d := range sets {
				a.total.distinctLate[d.name] += uint64(len(d.members))
			}
			a.statsMu.Unlock()
			continue
		}
		win := a.window(start, len(sets))
		held := a.members[start]
		if held == nil {
			held = make(map[*series]map[uint64]struct{}, len(sets))
			a.members[start] = held
		}
		for key, d := range sets {
			sr := a.seriesFor(key, d.name, kindDistinct, d.labelNames, d.labelValues, nil)
			c, ok := win[sr]
			if !ok {
				sr.cells++
			}
			m := held[sr]
			if m == nil {
				m = make(map[uint64]struct{}, len(d.members))
				held[sr] = m
			}
			for member := range d.members {
				if len(m) >= a.distinctCap {
					break
				}
				m[member] = struct{}{}
			}
			grew := uint64(len(m)) > c.total
			c.total = uint64(len(m))
			win[sr] = c
			if grew {
				a.markDirty(start, sr)
			}
		}
	}
}

// mergeHist folds one shard's histogram observations in. The caller holds a.mu.
func (a *Accumulator) mergeHist(hist map[int64]map[string]*hdelta, sealed int64) {
	for start, deltas := range hist {
		if start < sealed {
			a.statsMu.Lock()
			for _, d := range deltas {
				a.total.late[d.name] += d.n
			}
			a.statsMu.Unlock()
			continue
		}
		win := a.window(start, len(deltas))
		hwin := a.hist[start]
		if hwin == nil {
			hwin = make(map[*series]*hcell, len(deltas))
			a.hist[start] = hwin
		}
		for key, d := range deltas {
			if a.lin.on {
				a.lin.folded[d.name] += d.n
			}
			sr := a.seriesFor(key, d.name, kindHistogram, d.labelNames, d.labelValues, d.bounds)
			c, ok := win[sr]
			if !ok {
				sr.cells++
			}
			c.total += d.n
			win[sr] = c
			h := hwin[sr]
			if h == nil {
				h = &hcell{}
				if sr.bounds != nil {
					h.buckets = make([]uint64, len(sr.bounds)+1)
				}
				hwin[sr] = h
			}
			h.sum += d.sum
			for i := range d.buckets {
				if i < len(h.buckets) {
					h.buckets[i] += d.buckets[i]
				}
			}
			a.markDirty(start, sr)
		}
	}
}

// release drops the members of every window the watermark has passed by
// EarlyLateness plus DistinctHorizon: their counts are final, and a later member
// is late. The counts stay in the cells, written or still dirty. The caller
// holds a.mu.
func (a *Accumulator) release(w int64) {
	if a.distinctMs <= 0 {
		return
	}
	// A window [s, s+win) is released once s+win+early+horizon <= w.
	edge := w - a.earlyMs - a.distinctMs
	for start := range a.members {
		if start+a.winMs <= edge {
			delete(a.members, start)
		}
	}
	bound := edge - a.winMs + 1 // the first start that is not yet released
	if r := bound % a.winMs; r != 0 {
		if r < 0 {
			r += a.winMs
		}
		bound += a.winMs - r
	}
	if bound > a.distinctBefore.Load() {
		a.distinctBefore.Store(bound)
	}
}

func (a *Accumulator) foldStatsOnly() {
	for _, s := range a.shards {
		s.mu.Lock()
		st := s.stats
		s.stats = a.newStats()
		s.mu.Unlock()
		a.foldStats(st)
	}
}

func (a *Accumulator) foldStats(st *stats) {
	a.statsMu.Lock()
	a.total.add(st)
	a.statsMu.Unlock()
}

// watermark returns the pod's arrival watermark in unix milliseconds, or false
// while any assigned partition has no evidence of where it is and MaxHold does
// not release it. The caller holds a.mu.
func (a *Accumulator) watermark(nowMs int64) (int64, bool) {
	if len(a.parts) == 0 {
		return 0, false
	}
	// A partition is released on the reference clock, never past the newest
	// reference time any partition has shown: processing time runs ahead of it
	// by the pipeline in front of the log.
	var newest int64
	for _, ps := range a.parts {
		if ps.maxRef > newest {
			newest = ps.maxRef
		}
	}
	queued := a.pending != nil && a.pending() > 0
	low := int64(math.MaxInt64)
	held := 0
	for _, ps := range a.parts {
		w := ps.maxRef
		last := ps.lastAt
		if last == 0 {
			last = a.generationMs
		}
		// A partition that has handed on everything it holds, whose handed-on
		// messages are all processed, and that has been quiet for the idle
		// timeout cannot hold the pod back: anything it receives next is
		// newer. One that is quiet with messages outstanding is stuck, and
		// does hold.
		if !queued && ps.claim.caughtUp() && nowMs-last >= a.idleMs {
			idle := nowMs - a.idleMs
			if idle > newest {
				idle = newest
			}
			if idle > w {
				w = idle
			}
		}
		// Nor does any partition hold it back longer than MaxHold.
		if a.holdMs > 0 {
			floor := nowMs - a.holdMs
			if floor > newest {
				floor = newest
			}
			if floor > w {
				w = floor
				held++
			}
		}
		if w <= 0 {
			return 0, false
		}
		if w < low {
			low = w
		}
	}
	if low < a.podW {
		low = a.podW
	}
	a.podW = low
	a.statsMu.Lock()
	a.counters.holdReleases += uint64(held)
	a.gauges.heldByMaxHold = held
	a.statsMu.Unlock()
	return low, true
}

// writeWindows writes every changed series of the windows ending at or before
// edge, oldest window first, in requests of at most writeBatch series. Each
// request is committed as it succeeds, so a failure or a deadline part-way
// keeps what was written, and the next flush re-sends only the rest: otherwise
// a backlog larger than one flush can carry would be re-sent whole every flush
// and never drain. With maxRows above zero it stops once at least that many
// series are written (a histogram cell is several series), and reports that it stopped with changed windows left behind. The
// caller holds a.mu.
func (a *Accumulator) writeWindows(ctx context.Context, edge int64, maxRows int) (bool, error) {
	var starts []int64
	for start := range a.dirty {
		if start+a.winMs <= edge {
			starts = append(starts, start)
		}
	}
	// Oldest first: they are the nearest to being sealed.
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	type ref struct {
		start int64
		sr    *series
		total uint64
	}
	var req *prompb.WriteRequest
	var batch []ref
	rows := 0
	commit := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := a.write(ctx, req); err != nil {
			a.statsMu.Lock()
			a.counters.windowErrors++
			a.statsMu.Unlock()
			return fmt.Errorf("event-time windows: %w", err)
		}
		var rewrites uint64
		for _, r := range batch {
			win := a.windows[r.start]
			c := win[r.sr]
			if c.written > 0 {
				rewrites++
			}
			c.written = r.total
			win[r.sr] = c
			// Nothing folds in while a.mu is held, so a written series is clean.
			set := a.dirty[r.start]
			delete(set, r.sr)
			if len(set) == 0 {
				delete(a.dirty, r.start)
			}
		}
		n := len(req.Timeseries)
		a.statsMu.Lock()
		a.counters.rewrites += rewrites
		a.counters.rows += uint64(n)
		a.statsMu.Unlock()
		rows += n
		req, batch = nil, batch[:0]
		return nil
	}
	for _, start := range starts {
		win := a.windows[start]
		// commit deletes from this set only entries already visited, which
		// ranging over a map permits.
		for sr := range a.dirty[start] {
			if maxRows > 0 && rows+pending(req) >= maxRows {
				if err := commit(); err != nil {
					return false, err
				}
				return true, nil
			}
			if req == nil {
				req = &prompb.WriteRequest{}
			}
			c := win[sr]
			req.Timeseries = a.appendWindowSeries(req.Timeseries, sr, c.total, start)
			batch = append(batch, ref{start, sr, c.total})
			if len(req.Timeseries) >= a.writeBatch {
				if err := commit(); err != nil {
					return false, err
				}
			}
		}
	}
	return false, commit()
}

func pending(req *prompb.WriteRequest) int {
	if req == nil {
		return 0
	}
	return len(req.Timeseries)
}

func (a *Accumulator) windowName(family string) string {
	name, ok := a.windowNames[family]
	if !ok {
		name = a.names.WindowName(family)
		a.windowNames[family] = name
	}
	return name
}

func (a *Accumulator) seriesLabels(name string, sr *series, extra int) []prompb.Label {
	labels := make([]prompb.Label, 0, len(sr.labelNames)+4+extra)
	labels = append(labels, prompb.Label{Name: "__name__", Value: name})
	for i, n := range sr.labelNames {
		labels = append(labels, prompb.Label{Name: n, Value: sr.labelValues[i]})
	}
	return labels
}

func (a *Accumulator) writerLabels(labels []prompb.Label) []prompb.Label {
	return append(labels,
		prompb.Label{Name: "job", Value: a.jobName},
		prompb.Label{Name: "instance", Value: a.instance},
		prompb.Label{Name: LabelGeneration, Value: a.generation},
	)
}

func (a *Accumulator) appendWindowSeries(ts []prompb.TimeSeries, sr *series, total uint64, start int64) []prompb.TimeSeries {
	sample := func(v float64) []prompb.Sample { return []prompb.Sample{{Value: v, Timestamp: start}} }
	if sr.kind != kindHistogram {
		labels := a.writerLabels(a.seriesLabels(a.windowName(sr.name), sr, 0))
		return append(ts, prompb.TimeSeries{Labels: labels, Samples: sample(float64(total))})
	}
	var h hcell
	if hc := a.hist[start][sr]; hc != nil {
		h = *hc
	}
	ts = append(ts,
		prompb.TimeSeries{Labels: a.writerLabels(a.seriesLabels(sr.name+"_window_count", sr, 0)), Samples: sample(float64(total))},
		prompb.TimeSeries{Labels: a.writerLabels(a.seriesLabels(sr.name+"_window_sum", sr, 0)), Samples: sample(h.sum)},
	)
	if sr.bounds == nil {
		return ts
	}
	var cum uint64
	for i := 0; i <= len(sr.bounds); i++ {
		le := "+Inf"
		if i < len(sr.bounds) {
			le = strconv.FormatFloat(sr.bounds[i], 'g', -1, 64)
		}
		if i < len(h.buckets) {
			cum += h.buckets[i]
		}
		labels := a.seriesLabels(sr.name+"_window_bucket", sr, 1)
		labels = append(labels, prompb.Label{Name: "le", Value: le})
		ts = append(ts, prompb.TimeSeries{Labels: a.writerLabels(labels), Samples: sample(float64(cum))})
	}
	return ts
}

// MetricWatermark, MetricPartitions and MetricLateness are the series written
// beside the windows, under the accumulator's prefix.
func (a *Accumulator) MetricWatermark() string  { return a.names.Prefix + "_watermark_seconds" }
func (a *Accumulator) MetricPartitions() string { return a.names.Prefix + "_partitions" }
func (a *Accumulator) MetricLateness() string   { return a.names.Prefix + "_lateness_seconds" }

// publishWatermark writes, per assigned partition, the watermark the windows
// just written are complete to, with the partition counts and the lateness
// bounds a reader needs to use it. The caller holds a.mu.
func (a *Accumulator) publishWatermark(ctx context.Context, w, nowMs int64) error {
	req := &prompb.WriteRequest{}
	common := func(name string, extra ...prompb.Label) []prompb.Label {
		labels := []prompb.Label{{Name: "__name__", Value: name}}
		labels = append(labels, extra...)
		return append(labels,
			prompb.Label{Name: "job", Value: a.jobName},
			prompb.Label{Name: "instance", Value: a.instance},
		)
	}
	watermarkLabels := func(extra ...prompb.Label) []prompb.Label {
		labels := common(a.MetricWatermark(), extra...)
		if a.lin.on {
			// A reader ties each writer on a partition's lineage to its own
			// watermark; max by partition still takes the furthest.
			labels = append(labels, prompb.Label{Name: LabelGeneration, Value: a.generation})
		}
		return labels
	}
	sample := func(v float64) []prompb.Sample { return []prompb.Sample{{Value: v, Timestamp: nowMs}} }
	topics := make(map[string]struct{})
	for k := range a.parts {
		topics[k.topic] = struct{}{}
		req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
			Labels: watermarkLabels(
				prompb.Label{Name: "topic", Value: k.topic},
				prompb.Label{Name: "partition", Value: strconv.Itoa(int(k.partition))}),
			Samples: sample(float64(w) / 1000),
		})
	}
	var partitionErr error
	for topic := range topics {
		if a.partitions == nil {
			continue
		}
		n, err := a.partitions(topic)
		if err != nil {
			// Without the count a reader cannot prove coverage and holds,
			// which is the safe side.
			partitionErr = errors.Join(partitionErr, err)
			continue
		}
		req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
			Labels:  common(a.MetricPartitions(), prompb.Label{Name: "topic", Value: topic}),
			Samples: sample(float64(n)),
		})
	}
	for _, b := range []struct {
		name string
		ms   int64
	}{{"early", a.earlyMs}, {"max", a.maxMs}} {
		req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
			Labels:  common(a.MetricLateness(), prompb.Label{Name: "bound", Value: b.name}),
			Samples: sample(float64(b.ms) / 1000),
		})
	}
	if err := a.write(ctx, req); err != nil {
		a.statsMu.Lock()
		a.counters.watermarkErrors++
		a.statsMu.Unlock()
		return fmt.Errorf("event-time watermark: %w", errors.Join(err, partitionErr))
	}
	if partitionErr != nil {
		return fmt.Errorf("event-time partition count: %w", partitionErr)
	}
	return nil
}

// seal drops the windows past MaxLateness whose counts are written, and moves
// the late boundary up to them. A window still holding unwritten counts stays,
// unless it is a further MaxLateness behind, when its counts are dropped and
// counted. The caller holds a.mu.
func (a *Accumulator) seal(w int64) {
	for start, set := range a.dirty {
		if start+a.winMs > w-2*a.maxMs {
			continue
		}
		win := a.windows[start]
		for sr := range set {
			c := win[sr]
			a.addDropped(sr.name, c.total-c.written)
			if !a.lin.on {
				// With lineage the window is deleted below in this same call,
				// and its terminal pair must carry what was really written.
				c.written = c.total
				win[sr] = c
			}
		}
		delete(a.dirty, start)
	}
	bound := w - a.maxMs
	bound -= bound % a.winMs
	for start := range a.dirty {
		if start < bound {
			bound = start
		}
	}
	for start, win := range a.windows {
		if start >= bound {
			continue
		}
		if a.lin.on {
			a.foldPair(start, win)
		}
		for sr := range win {
			if sr.cells--; sr.cells == 0 {
				delete(a.series, sr.key)
			}
		}
		delete(a.windows, start)
		delete(a.hist, start)
		delete(a.members, start)
	}
	if bound > a.sealedBefore.Load() {
		a.sealedBefore.Store(bound)
	}
}

func (a *Accumulator) addDropped(name string, n uint64) {
	a.statsMu.Lock()
	a.counters.droppedUnwritten[name] += n
	a.statsMu.Unlock()
}

func (a *Accumulator) refreshGauges(nowMs int64) {
	if a.lin.on {
		a.publishLineageStats(nowMs)
	}
	cells, members := 0, 0
	for _, win := range a.windows {
		cells += len(win)
	}
	for _, held := range a.members {
		for _, m := range held {
			members += len(m)
		}
	}
	a.statsMu.Lock()
	held := a.gauges.heldByMaxHold
	a.gauges = gauges{
		openWindows:   len(a.windows),
		openSeries:    len(a.series),
		openCells:     cells,
		openMembers:   members,
		heldByMaxHold: held,
		watermarkMs:   a.podW,
		sealedBefore:  a.sealedBefore.Load(),
	}
	a.statsMu.Unlock()
}
