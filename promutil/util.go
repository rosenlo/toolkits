package promutil

import (
	"fmt"

	"github.com/rosenlo/toolkits/log"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	ErrorMetricNameSuffix = "errors_total"
)

var DefBuckets = []float64{.001, .0015, .002, .003, .005, .01, .025, .05, .1, .25, .5, .75, 1, 1.5, 2.5, 3, 3.5, 5, 7.5, 10}

func NewHistogramVec(name, help string, buckets []float64, labels []string) (*prometheus.HistogramVec, error) {
	log.Debugf("[%s] register with labels: %v", name, labels)
	if len(buckets) != 0 {
		buckets = DefBuckets
	}
	metric := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    name,
			Help:    help,
			Buckets: buckets,
		},
		labels,
	)

	if err := prometheus.Register(metric); err != nil {
		return nil, fmt.Errorf("[%s] failed to register: %w", name, err)
	}

	return metric, nil
}

func NewGaugeVec(name, help string, labels []string) (*prometheus.GaugeVec, error) {
	log.Debugf("[%s] register with labels: %v", name, labels)
	metric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: name,
			Help: help,
		},
		labels,
	)

	if err := prometheus.Register(metric); err != nil {
		return nil, fmt.Errorf("[%s] failed to register: %w", name, err)
	}

	return metric, nil
}

func NewErrorCounterVec(name, help string, labels []string) (*prometheus.CounterVec, error) {
	metricName := fmt.Sprintf("%s_%s", name, ErrorMetricNameSuffix)
	log.Debugf("[%s] register with labels: %v", metricName, labels)
	metric := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricName,
			Help: help,
		},
		labels,
	)

	if err := prometheus.Register(metric); err != nil {
		return nil, fmt.Errorf("[%s] failed to register: %w", metricName, err)
	}

	return metric, nil
}
