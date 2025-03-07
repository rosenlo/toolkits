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

func BatchRemoteWrite(ctx context.Context, promClient *Client, req *prompb.WriteRequest, batch int) error {
	ts := req.Timeseries

	r := &prompb.WriteRequest{}
	defer r.Reset()
	for i := 0; i < len(ts); i += batch {
		if i+batch > len(ts) {
			r.Timeseries = ts[i:]
		} else {
			r.Timeseries = ts[i : i+batch]
		}

		data, err := proto.Marshal(r)
		if err != nil {
			return err
		}
		err = promClient.Write(ctx, data)
		if err != nil {
			return err
		}
	}

	return nil
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
