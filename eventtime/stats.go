package eventtime

import (
	"sort"

	"github.com/prometheus/client_golang/prometheus"
)

// Self metrics. Each shard keeps plain counts that the flusher folds in, so the
// hot path shares no atomic across workers; the collector reports the folded
// totals, which trail the messages by at most one flush interval.

type histogram struct {
	bounds []float64
	counts []uint64 // per bucket, not cumulative; the last is +Inf
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) histogram {
	return histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v)
	h.counts[i]++
	h.sum += v
	h.count++
}

func (h *histogram) add(o *histogram) {
	for i := range o.counts {
		h.counts[i] += o.counts[i]
	}
	h.sum += o.sum
	h.count += o.count
}

func (h *histogram) metric(desc *prometheus.Desc, labels ...string) prometheus.Metric {
	buckets := make(map[float64]uint64, len(h.bounds))
	var cum uint64
	for i, b := range h.bounds {
		cum += h.counts[i]
		buckets[b] = cum
	}
	return prometheus.MustNewConstHistogram(desc, h.count, h.sum, buckets, labels...)
}

type stats struct {
	delay      []histogram // by class
	arrivalLag histogram
	stamps     [numEventSources][numRefSources]uint64
	late       map[string]uint64
	// distinctLate counts distinct members whose window was already
	// released or sealed.
	distinctLate map[string]uint64
	// rejected counts histogram observations refused for being negative or
	// not finite.
	rejected map[string]uint64
}

func (a *Accumulator) newStats() *stats {
	s := &stats{
		delay:        make([]histogram, len(a.names.Classes)),
		arrivalLag:   newHistogram(a.lagBuckets),
		late:         make(map[string]uint64),
		distinctLate: make(map[string]uint64),
		rejected:     make(map[string]uint64),
	}
	for i := range s.delay {
		s.delay[i] = newHistogram(a.delayBuckets)
	}
	return s
}

func (s *stats) add(o *stats) {
	for i := range s.delay {
		s.delay[i].add(&o.delay[i])
	}
	s.arrivalLag.add(&o.arrivalLag)
	for i := range s.stamps {
		for j := range s.stamps[i] {
			s.stamps[i][j] += o.stamps[i][j]
		}
	}
	for k, v := range o.late {
		s.late[k] += v
	}
	for k, v := range o.distinctLate {
		s.distinctLate[k] += v
	}
	for k, v := range o.rejected {
		s.rejected[k] += v
	}
}

type descs struct {
	delay, arrivalLag, stamps, late, dropped, rows, rewrites, writeErrors, openWindows, openSeries,
	openCells, podWatermark, sealedBefore, distinctLate, openMembers, rejected, holdReleases,
	heldPartitions, cappedFlushes *prometheus.Desc
}

func (d descs) all() []*prometheus.Desc {
	return []*prometheus.Desc{d.delay, d.arrivalLag, d.stamps, d.late, d.dropped, d.rows, d.rewrites,
		d.writeErrors, d.openWindows, d.openSeries, d.openCells, d.podWatermark, d.sealedBefore,
		d.distinctLate, d.openMembers, d.rejected, d.holdReleases, d.heldPartitions, d.cappedFlushes}
}

func newDescs(n Names) descs {
	p := n.Prefix + "_"
	return descs{
		delay: prometheus.NewDesc(p+n.DelayMetric,
			"The primary reference clock minus the event clock, by class. Negative means the event clock is ahead.",
			[]string{"class"}, nil),
		arrivalLag: prometheus.NewDesc(p+"arrival_lag_seconds",
			"Processing time minus the reference clock (the primary, else the secondary).", nil, nil),
		stamps: prometheus.NewDesc(p+"stamps_total",
			"Tracked messages stamped, by the clock their event time came from and the reference clock used.",
			[]string{"event_source", "reference"}, nil),
		late: prometheus.NewDesc(p+"late_total",
			"Observations whose window was already sealed (past max_lateness), counted here and written nowhere.",
			[]string{"family"}, nil),
		dropped: prometheus.NewDesc(p+"dropped_unwritten_total",
			"Observations dropped because their window could not be written: a write failing past twice max_lateness, "+
				"or at the end of a session.",
			[]string{"family"}, nil),
		rows: prometheus.NewDesc(p+"write_rows_total",
			"Window samples written, first writes and re-writes alike.", nil, nil),
		rewrites: prometheus.NewDesc(p+"rewrites_total",
			"Window cells re-written with a larger accumulated value after their first write.", nil, nil),
		writeErrors: prometheus.NewDesc(p+"write_errors_total",
			"Failed event-time writes, by what they carried.", []string{"stage"}, nil),
		openWindows: prometheus.NewDesc(p+"open_windows",
			"Windows held, not yet sealed.", nil, nil),
		openSeries: prometheus.NewDesc(p+"open_series",
			"Distinct series held across the open windows.", nil, nil),
		openCells: prometheus.NewDesc(p+"open_cells",
			"Window-series counts held: what the open windows cost in memory.", nil, nil),
		podWatermark: prometheus.NewDesc(p+"pod_watermark_seconds",
			"This pod's arrival watermark, unix seconds; 0 until every assigned partition has one.", nil, nil),
		sealedBefore: prometheus.NewDesc(p+"sealed_before_seconds",
			"The first window start not yet sealed, unix seconds.", nil, nil),
		distinctLate: prometheus.NewDesc(p+"distinct_late_total",
			"Distinct-family members whose window's members were already released (past early_lateness plus the "+
				"distinct horizon) or sealed: counted here and in no window.",
			[]string{"family"}, nil),
		openMembers: prometheus.NewDesc(p+"open_members",
			"Distinct-family members held across the windows not yet released: what they cost in memory.", nil, nil),
		rejected: prometheus.NewDesc(p+"rejected_total",
			"Histogram observations refused for being negative or not finite: a re-write can only raise a sample.",
			[]string{"family"}, nil),
		holdReleases: prometheus.NewDesc(p+"hold_releases_total",
			"Partition-flushes in which max_hold, not the partition's own reference time, set its watermark.", nil, nil),
		heldPartitions: prometheus.NewDesc(p+"max_hold_partitions",
			"Partitions whose watermark max_hold set in the last flush.", nil, nil),
		cappedFlushes: prometheus.NewDesc(p+"capped_flushes_total",
			"Flushes that reached max_rows_per_flush with windows left unwritten, and so published no watermark.",
			nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (a *Accumulator) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range a.descs.all() {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (a *Accumulator) Collect(ch chan<- prometheus.Metric) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	d, n, t := a.descs, a.names, a.total
	for i := range t.delay {
		ch <- t.delay[i].metric(d.delay, n.Classes[i])
	}
	ch <- t.arrivalLag.metric(d.arrivalLag)
	refs := [numRefSources]string{n.References[0], n.References[1], "processing"}
	for i := range t.stamps {
		for j := range t.stamps[i] {
			ch <- prometheus.MustNewConstMetric(d.stamps, prometheus.CounterValue, float64(t.stamps[i][j]),
				n.EventSources[i], refs[j])
		}
	}
	for family, v := range t.late {
		ch <- prometheus.MustNewConstMetric(d.late, prometheus.CounterValue, float64(v), family)
	}
	for family, v := range t.distinctLate {
		ch <- prometheus.MustNewConstMetric(d.distinctLate, prometheus.CounterValue, float64(v), family)
	}
	for family, v := range t.rejected {
		ch <- prometheus.MustNewConstMetric(d.rejected, prometheus.CounterValue, float64(v), family)
	}
	for family, v := range a.counters.droppedUnwritten {
		ch <- prometheus.MustNewConstMetric(d.dropped, prometheus.CounterValue, float64(v), family)
	}
	c, g := a.counters, a.gauges
	ch <- prometheus.MustNewConstMetric(d.rows, prometheus.CounterValue, float64(c.rows))
	ch <- prometheus.MustNewConstMetric(d.rewrites, prometheus.CounterValue, float64(c.rewrites))
	ch <- prometheus.MustNewConstMetric(d.writeErrors, prometheus.CounterValue, float64(c.windowErrors), "windows")
	ch <- prometheus.MustNewConstMetric(d.writeErrors, prometheus.CounterValue, float64(c.watermarkErrors), "watermark")
	ch <- prometheus.MustNewConstMetric(d.holdReleases, prometheus.CounterValue, float64(c.holdReleases))
	ch <- prometheus.MustNewConstMetric(d.cappedFlushes, prometheus.CounterValue, float64(c.cappedFlushes))
	ch <- prometheus.MustNewConstMetric(d.openWindows, prometheus.GaugeValue, float64(g.openWindows))
	ch <- prometheus.MustNewConstMetric(d.openSeries, prometheus.GaugeValue, float64(g.openSeries))
	ch <- prometheus.MustNewConstMetric(d.openCells, prometheus.GaugeValue, float64(g.openCells))
	ch <- prometheus.MustNewConstMetric(d.podWatermark, prometheus.GaugeValue, float64(g.watermarkMs)/1000)
	ch <- prometheus.MustNewConstMetric(d.sealedBefore, prometheus.GaugeValue, float64(g.sealedBefore)/1000)
	ch <- prometheus.MustNewConstMetric(d.openMembers, prometheus.GaugeValue, float64(g.openMembers))
	ch <- prometheus.MustNewConstMetric(d.heldPartitions, prometheus.GaugeValue, float64(g.heldByMaxHold))
}
