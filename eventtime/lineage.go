package eventtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"
)

// Lineage is what lets a reader prove a window final from storage alone, when
// Config.Lineage is on. A writer's window series only grow, but a watermark says
// nothing about whether the writes it covers are visible, nor which writers
// counted into a window. So, per writer:
//
//   - A terminal record, per window and terminal family, is the total the writer
//     wrote into the window, and the total it folded; written once the window
//     is sealed, or when the writer ends. A window the writer owed and counted
//     nothing into gets zeros. The writer owes every window from its claim time
//     minus MaxLateness, floored to a window, to its end plus FutureSkew; it
//     starts sealed below that bound, so it cannot count into a window it owes
//     no record for.
//   - A claim record, per partition, names the writer that held the partition
//     before this one, as the caller read it from the log (Register). It is
//     written stamped at the claim time, then re-written at every flush and at
//     the writer's end, stamped then: a store keeps it for its retention only,
//     and a writer may outlive that.
//   - The write gate: nothing of the writer is written (windows, terminal
//     records, watermark) while any partition that has handed a message to the
//     workers (Claim.Hand) is not registered with its claim record written. A
//     writer that dies before the gate opens has written nothing, so a reader
//     that skips it loses nothing.
//   - A stamp carries the writer the message was received under (StampAs). A
//     count for any other writer is not stored and is counted as after the end:
//     a message received by one session and counted after it ended never lands
//     in the next one.
//
// Observation counters close the books: intake counts every Count and Observe,
// folded what reached a window; intake = folded + late + after-end, once the
// shards are flushed.

// Generation is a writer's token: carry it with each message from receipt to
// its worker, and stamp with it (StampAs).
type Generation int64

type lpart struct {
	registered   bool
	claimWritten bool
	predecessor  string
	atMs         int64
	lastMs       int64 // the timestamp of the last claim sample written
}

type lineage struct {
	on       bool
	terminal []string // sorted
	isTerm   map[string]bool
	// current is the writer counts are stored for; 0 between sessions.
	current atomic.Int64

	// Guarded by Accumulator.mu.
	terminatedThrough int64                           // the first window start without a terminal record
	pairs             map[int64]map[string]*[2]uint64 // window start, family: written, folded
	folded            map[string]uint64               // since the last publish
	gateSince         int64                           // when the gate closed; 0 while open
	everOpen          bool                            // the gate has opened this session

	regMu sync.Mutex
	parts map[partKey]*lpart

	// Guarded by Accumulator.statsMu.
	foldedTotal map[string]uint64
	gateHeldMs  int64

	// now is the clock a writer's end is read from: how far its owed windows
	// reach.
	now func() time.Time
}

func (l *lineage) init(cfg Config) {
	l.on = cfg.Lineage
	l.now = time.Now
	if !l.on {
		return
	}
	l.isTerm = make(map[string]bool, len(cfg.TerminalFamilies))
	for _, f := range cfg.TerminalFamilies {
		if !l.isTerm[f] {
			l.isTerm[f] = true
			l.terminal = append(l.terminal, f)
		}
	}
	sort.Strings(l.terminal)
	l.folded = make(map[string]uint64)
	l.foldedTotal = make(map[string]uint64)
}

// StampAs is Stamp for a message received under writer g. With Lineage, a
// count stamped for a writer that is no longer current is not stored.
func (s *Shard) StampAs(g Generation, topic string, partition int32, r Reading, nowMs int64) Stamp {
	st := s.Stamp(topic, partition, r, nowMs)
	st.gen = int64(g)
	return st
}

// afterEnd counts one observation in and reports whether its writer has ended.
// The caller holds s.mu.
func (s *Shard) afterEnd(st Stamp, name string) bool {
	s.stats.intake[name]++
	if st.gen != 0 && st.gen != s.acc.lin.current.Load() {
		s.stats.afterEnd[name]++
		return true
	}
	return false
}

// staleDistinct is afterEnd for a distinct member. A member is not an
// observation of the books, so it is counted where every other unstored member
// is, on distinct_late_total. The caller holds s.mu.
func (s *Shard) staleDistinct(st Stamp, name string) bool {
	if st.gen != 0 && st.gen != s.acc.lin.current.Load() {
		s.stats.distinctLate[name]++
		return true
	}
	return false
}

// Hand records that the claim is handing a message to the workers. Call it
// before the message is queued: the gate holds the writer until a partition
// that has handed anything is registered.
func (c *Claim) Hand() {
	c.handed.Store(true)
}

// Generation is the current writer's token; 0 between sessions or without
// Lineage.
func (a *Accumulator) Generation() Generation {
	return Generation(a.lin.current.Load())
}

// WriterID names the current writer across processes: instance/generation. It
// is what the caller commits with its offsets and what a successor reads back
// as its predecessor. Empty between sessions.
func (a *Accumulator) WriterID() string {
	g := a.lin.current.Load()
	if g == 0 {
		return ""
	}
	return a.instance + "/" + strconv.FormatInt(g, 10)
}

// Register records that the log names writer g on a partition, with
// predecessor as the writer the log named before it, and queues the claim
// record. A registration for a writer that is no longer current is ignored.
func (a *Accumulator) Register(g Generation, topic string, partition int32, predecessor string, at time.Time) {
	if !a.lin.on || g == 0 {
		return
	}
	a.lin.regMu.Lock()
	defer a.lin.regMu.Unlock()
	// Compared under regMu: startLineage swaps the map and the current
	// writer together, so a registration never lands in the next writer's.
	if int64(g) != a.lin.current.Load() {
		return
	}
	k := partKey{topic, partition}
	if p := a.lin.parts[k]; p != nil && p.registered {
		return
	}
	a.lin.parts[k] = &lpart{registered: true, predecessor: predecessor, atMs: at.UnixMilli()}
}

// MetricClaim and MetricTerminal are the lineage records' series.
func (a *Accumulator) MetricClaim() string    { return a.names.Prefix + "_claim_seconds" }
func (a *Accumulator) MetricTerminal() string { return a.names.Prefix + "_terminal_window_count" }

func (a *Accumulator) floorWindow(ms int64) int64 {
	r := ms % a.winMs
	if r < 0 {
		r += a.winMs
	}
	return ms - r
}

// startLineage opens the lineage state of the writer StartGeneration just
// opened. The caller holds a.mu.
func (a *Accumulator) startLineage() {
	// Counted between sessions: no writer holds them.
	a.discardShards()
	sealed := a.floorWindow(a.generationMs - a.maxMs)
	a.sealedBefore.Store(sealed)
	a.lin.terminatedThrough = sealed
	a.lin.pairs = make(map[int64]map[string]*[2]uint64)
	a.lin.gateSince = 0
	a.lin.everOpen = false
	for _, ps := range a.parts {
		ps.claim.handed.Store(false)
	}
	a.lin.regMu.Lock()
	a.lin.parts = make(map[partKey]*lpart)
	a.lin.current.Store(a.generationMs)
	a.lin.regMu.Unlock()
}

// discardShards takes every shard's counts as after the end of a writer. The
// caller holds a.mu.
func (a *Accumulator) discardShards() {
	for _, s := range a.shards {
		s.mu.Lock()
		windows, st, distinct, hist := s.windows, s.stats, s.distinct, s.hist
		s.reset()
		s.mu.Unlock()
		for _, deltas := range windows {
			for _, d := range deltas {
				st.afterEnd[d.name] += d.n
			}
		}
		for _, deltas := range hist {
			for _, d := range deltas {
				st.afterEnd[d.name] += d.n
			}
		}
		for _, sets := range distinct {
			for _, d := range sets {
				st.distinctLate[d.name] += uint64(len(d.members))
			}
		}
		a.foldStats(st)
	}
}

// gateShut reports whether a partition that handed messages is not yet
// registered with its claim record written. The caller holds a.mu.
func (a *Accumulator) gateShut() bool {
	held := false
	a.lin.regMu.Lock()
	for k, ps := range a.parts {
		if !ps.claim.handed.Load() {
			continue
		}
		if p := a.lin.parts[k]; p == nil || !p.registered || !p.claimWritten {
			held = true
			break
		}
	}
	a.lin.regMu.Unlock()
	return held
}

// noteGate records the gate as Flush sees it, on Flush's clock: how long it
// has been shut, and whether it ever opened. The caller holds a.mu.
func (a *Accumulator) noteGate(shut bool, nowMs int64) {
	switch {
	case shut && a.lin.gateSince == 0:
		a.lin.gateSince = nowMs
	case !shut:
		a.lin.gateSince = 0
		a.lin.everOpen = true
	}
}

// writeClaims writes the claim records registered and not yet written, stamped
// at the claim time, then re-writes the written ones stamped at nowMs if that is
// later than their last sample. The two go in separate requests, so a refused
// re-write never holds a first write, and with it the gate. The caller holds
// a.mu.
func (a *Accumulator) writeClaims(ctx context.Context, nowMs int64) error {
	var first, again []claimWrite
	a.lin.regMu.Lock()
	for k, p := range a.lin.parts {
		switch {
		case !p.registered:
		case !p.claimWritten:
			first = append(first, claimWrite{k, *p, p.atMs})
		case nowMs > p.lastMs:
			again = append(again, claimWrite{k, *p, nowMs})
		}
	}
	a.lin.regMu.Unlock()
	return errors.Join(a.writeClaimBatch(ctx, first), a.writeClaimBatch(ctx, again))
}

// claimWrite is one claim sample to write: the value is the claim time, the
// timestamp at.
type claimWrite struct {
	k  partKey
	p  lpart
	at int64
}

// writeClaimBatch writes one request of claim samples. The caller holds a.mu.
func (a *Accumulator) writeClaimBatch(ctx context.Context, todo []claimWrite) error {
	if len(todo) == 0 {
		return nil
	}
	req := &prompb.WriteRequest{}
	for _, t := range todo {
		labels := []prompb.Label{
			{Name: "__name__", Value: a.MetricClaim()},
			{Name: "topic", Value: t.k.topic},
			{Name: "partition", Value: strconv.Itoa(int(t.k.partition))},
			{Name: "predecessor", Value: t.p.predecessor},
		}
		req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
			Labels:  a.writerLabels(labels),
			Samples: []prompb.Sample{{Value: float64(t.p.atMs) / 1000, Timestamp: t.at}},
		})
	}
	if err := a.write(ctx, req); err != nil {
		a.statsMu.Lock()
		a.counters.claimErrors++
		a.statsMu.Unlock()
		return fmt.Errorf("event-time claim records: %w", err)
	}
	a.lin.regMu.Lock()
	for _, t := range todo {
		if p := a.lin.parts[t.k]; p != nil {
			p.claimWritten = true
			p.lastMs = t.at
		}
	}
	a.lin.regMu.Unlock()
	return nil
}

// foldPair keeps a window's terminal pair once its cells leave memory. The
// caller holds a.mu.
func (a *Accumulator) foldPair(start int64, win map[*series]cell) {
	for sr, c := range win {
		if sr.kind != kindCounter || !a.lin.isTerm[sr.name] {
			continue
		}
		byFam := a.lin.pairs[start]
		if byFam == nil {
			byFam = make(map[string]*[2]uint64)
			a.lin.pairs[start] = byFam
		}
		p := byFam[sr.name]
		if p == nil {
			p = &[2]uint64{}
			byFam[sr.name] = p
		}
		p[0] += c.written
		p[1] += c.total
	}
}

// writeTerminals writes the terminal records of every window from the first
// without one up to through, zeros included, a whole window per request batch
// so a failure keeps what was written. The caller holds a.mu, and every window
// before through has left memory (its pair is folded).
func (a *Accumulator) writeTerminals(ctx context.Context, through int64) error {
	if len(a.lin.terminal) == 0 {
		a.lin.terminatedThrough = max(a.lin.terminatedThrough, through)
		return nil
	}
	perWindow := 2 * len(a.lin.terminal)
	windows := a.writeBatch / perWindow
	if windows < 1 {
		windows = 1
	}
	for a.lin.terminatedThrough < through {
		req := &prompb.WriteRequest{}
		last := a.lin.terminatedThrough
		for n := 0; n < windows && last < through; n++ {
			for _, fam := range a.lin.terminal {
				var p [2]uint64
				if q := a.lin.pairs[last][fam]; q != nil {
					p = *q
				}
				for i, stage := range [2]string{"written", "folded"} {
					labels := []prompb.Label{
						{Name: "__name__", Value: a.MetricTerminal()},
						{Name: "family", Value: a.windowName(fam)},
						{Name: "stage", Value: stage},
					}
					req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
						Labels:  a.writerLabels(labels),
						Samples: []prompb.Sample{{Value: float64(p[i]), Timestamp: last}},
					})
				}
			}
			last += a.winMs
		}
		if err := a.write(ctx, req); err != nil {
			a.statsMu.Lock()
			a.counters.terminalErrors++
			a.statsMu.Unlock()
			return fmt.Errorf("event-time terminal records: %w", err)
		}
		for start := a.lin.terminatedThrough; start < last; start += a.winMs {
			delete(a.lin.pairs, start)
		}
		a.lin.terminatedThrough = last
	}
	return nil
}

// errGateHeld is EndGeneration's report of a writer that ended with the gate
// shut: nothing more of what it counted is written. A writer whose gate never
// opened writes nothing at all; one that wrote while it was open still writes
// its terminal records.
var errGateHeld = errors.New("event-time writer ended with the write gate held: nothing written")

// endLineage closes a writer under Lineage: claims, then every window, then
// every terminal record it owes. The caller holds a.mu, and the writer is no
// longer current.
func (a *Accumulator) endLineage(ctx context.Context) error {
	nowMs := a.lin.now().UnixMilli()
	claimErr := a.writeClaims(ctx, nowMs)
	// Not noteGate: a writer's end moves no gauge, and the gauge is on
	// Flush's clock.
	a.lin.gateSince = 0
	var err error
	if a.gateShut() {
		// No new data: a partition it drew from does not name it.
		a.dropDirty()
		if !a.lin.everOpen {
			// It wrote nothing, so a reader that skips it loses nothing.
			a.lin.pairs = nil
			return errors.Join(claimErr, errGateHeld)
		}
		// It did write while the gate was open, so it still closes its
		// books: the records carry what is visible, and folded - written is
		// what it dropped. Its data holds no unregistered partition's count,
		// since nothing was written while the gate was shut.
		err = errGateHeld
	} else {
		err = a.writeAllOrDrop(ctx)
	}
	through := a.floorWindow(nowMs+a.futureMs) + a.winMs
	for start, win := range a.windows {
		a.foldPair(start, win)
		if start+a.winMs > through {
			through = start + a.winMs
		}
	}
	err = errors.Join(claimErr, err, a.writeTerminals(ctx, through))
	a.lin.pairs = nil
	return err
}

// publishLineageStats hands what the flusher counted to the collector. The
// caller holds a.mu.
func (a *Accumulator) publishLineageStats(nowMs int64) {
	held := int64(0)
	if a.lin.gateSince > 0 {
		held = nowMs - a.lin.gateSince
	}
	a.statsMu.Lock()
	for k, v := range a.lin.folded {
		a.lin.foldedTotal[k] += v
	}
	a.lin.gateHeldMs = held
	a.statsMu.Unlock()
	clear(a.lin.folded)
}

type lineageDescs struct {
	observations, afterEnd, gateHeld *prometheus.Desc
}

func newLineageDescs(n Names) lineageDescs {
	p := n.Prefix + "_"
	return lineageDescs{
		observations: prometheus.NewDesc(p+"observations_total",
			"Counter and histogram observations, by family and stage: intake is every one handed in, folded every "+
				"one that reached a window. intake = folded + late + after_end once the shards are flushed. "+
				"Distinct members are not observations: an unstored one is on distinct_late_total.",
			[]string{"family", "stage"}, nil),
		afterEnd: prometheus.NewDesc(p+"after_end_total",
			"Counter and histogram observations counted for a writer that had already ended, or between writers: "+
				"stored nowhere.",
			[]string{"family"}, nil),
		gateHeld: prometheus.NewDesc(p+"gate_held_seconds",
			"How long the write gate has been shut: a partition that handed messages is not yet registered "+
				"with its claim record written. 0 while open.", nil, nil),
	}
}

// collectLineage reports the lineage self-metrics. The caller holds
// a.statsMu.
func (a *Accumulator) collectLineage(ch chan<- prometheus.Metric) {
	d := a.linDescs
	for family, v := range a.total.intake {
		ch <- prometheus.MustNewConstMetric(d.observations, prometheus.CounterValue, float64(v), family, "intake")
	}
	for family, v := range a.lin.foldedTotal {
		ch <- prometheus.MustNewConstMetric(d.observations, prometheus.CounterValue, float64(v), family, "folded")
	}
	for family, v := range a.total.afterEnd {
		ch <- prometheus.MustNewConstMetric(d.afterEnd, prometheus.CounterValue, float64(v), family)
	}
	ch <- prometheus.MustNewConstMetric(d.gateHeld, prometheus.GaugeValue, float64(a.lin.gateHeldMs)/1000)
}
