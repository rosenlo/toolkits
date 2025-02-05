package promutil

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/rosenlo/toolkits/log"
)

const (
	MetricWriteRows     = "remote_write_rows"
	MetricWriteDuration = "remote_write_duration_seconds"
)

var (
	writeRows, _     = NewGaugeVec(MetricWriteRows, "Total data points of remote write", []string{})
	writeDuration, _ = NewGaugeVec(MetricWriteDuration, "The total time spent writing data points", []string{})
)

var (
	fqNameRe = regexp.MustCompile(`fqName: "([^"]*)"`)
)

func BuildWriteRequest(ch chan prometheus.Metric, jobName, instance string) *prompb.WriteRequest {
	req := &prompb.WriteRequest{Timeseries: []prompb.TimeSeries{}}
	cch := ch
	start := time.Now()

	for {
		select {
		case inst, ok := <-cch:
			if !ok {
				cch = nil
				break
			}
			metric := new(dto.Metric)
			err := inst.Write(metric)
			if err != nil {
				log.Warnf(err.Error())
				continue
			}

			desc := inst.Desc().String()
			submatch := fqNameRe.FindStringSubmatch(desc)
			if len(submatch) <= 1 {
				continue
			}

			var commonLabel []prompb.Label
			fqName := submatch[1]
			for _, l := range metric.Label {
				commonLabel = append(commonLabel, prompb.Label{
					Name:  l.GetName(),
					Value: l.GetValue(),
				})
			}
			commonLabel = append(commonLabel, prompb.Label{
				Name:  model.JobLabel,
				Value: jobName,
			}, prompb.Label{
				Name:  model.InstanceLabel,
				Value: instance,
			})
			timestamp := metric.GetTimestampMs()
			if timestamp == 0 {
				timestamp = start.UnixMilli()
			}

			if metric.Histogram != nil {
				FillHistogram(req, metric.Histogram, commonLabel, fqName, timestamp)
			}
			if metric.Gauge != nil {
				FillGauge(req, metric.Gauge, commonLabel, fqName, timestamp)
			}
			if metric.Counter != nil {
				FillCounter(req, metric.Counter, commonLabel, fqName, timestamp)
			}

		}

		if cch == nil {
			break
		}
	}

	return req
}

func BatchRemoteWrite(promClient *Client, req *prompb.WriteRequest, batch int) {
	start := time.Now()
	ts := req.Timeseries

	for i := 0; i < len(ts); i += batch {
		if i+batch > len(ts) {
			req.Timeseries = ts[i:]
		} else {
			req.Timeseries = ts[i : i+batch]
		}

		data, err := proto.Marshal(req)
		if err != nil {
			log.Warnf(err.Error())
			continue
		}
		err = promClient.Write(context.TODO(), data)
		if err != nil {
			log.Warnf(err.Error())
			continue
		}
	}
	writeRows.WithLabelValues().Set(float64(len(ts)))
	writeDuration.WithLabelValues().Set(time.Since(start).Seconds())
}

func FillCounter(
	req *prompb.WriteRequest,
	counter *dto.Counter,
	commonLabel []prompb.Label,
	fqName string,
	timestamp int64,
) {
	label := []prompb.Label{
		{
			Name:  model.MetricNameLabel,
			Value: fqName,
		},
	}
	label = append(label, commonLabel...)
	req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
		Labels: label,
		Samples: []prompb.Sample{
			{
				Value:     counter.GetValue(),
				Timestamp: timestamp,
			},
		},
	})
}

func FillGauge(
	req *prompb.WriteRequest,
	gauge *dto.Gauge,
	commonLabel []prompb.Label,
	fqName string,
	timestamp int64,
) {
	label := []prompb.Label{
		{
			Name:  model.MetricNameLabel,
			Value: fqName,
		},
	}
	label = append(label, commonLabel...)
	req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
		Labels: label,
		Samples: []prompb.Sample{
			{
				Value:     gauge.GetValue(),
				Timestamp: timestamp,
			},
		},
	})
}

func FillHistogram(
	req *prompb.WriteRequest,
	histogram *dto.Histogram,
	commonLabel []prompb.Label,
	fqName string,
	timestamp int64,
) {
	countName := fmt.Sprintf("%s_count", fqName)
	sumName := fmt.Sprintf("%s_sum", fqName)

	countLabel := []prompb.Label{
		{
			Name:  model.MetricNameLabel,
			Value: countName,
		},
	}
	countLabel = append(countLabel, commonLabel...)
	req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
		Labels: countLabel,
		Samples: []prompb.Sample{
			{
				Value:     float64(histogram.GetSampleCount()),
				Timestamp: timestamp,
			},
		},
	})

	sumLabel := []prompb.Label{
		{
			Name:  model.MetricNameLabel,
			Value: sumName,
		},
	}
	sumLabel = append(sumLabel, commonLabel...)
	req.Timeseries = append(req.Timeseries, prompb.TimeSeries{
		Labels: sumLabel,
		Samples: []prompb.Sample{
			{
				Value:     histogram.GetSampleSum(),
				Timestamp: timestamp,
			},
		},
	})

	var bucketLabel []prompb.Label
	bucketLabel = append(bucketLabel, commonLabel...)

	for _, b := range histogram.Bucket {
		t := prompb.TimeSeries{}
		metricLabel := append(bucketLabel, prompb.Label{
			Name:  model.MetricNameLabel,
			Value: fmt.Sprintf("%s_bucket", fqName),
		}, prompb.Label{
			Name:  model.BucketLabel,
			Value: fmt.Sprintf("%v", b.GetUpperBound()),
		})
		t.Labels = metricLabel
		sample := prompb.Sample{
			Value:     float64(b.GetCumulativeCount()),
			Timestamp: timestamp,
		}
		t.Samples = append(t.Samples, sample)
		req.Timeseries = append(req.Timeseries, t)
	}
}
