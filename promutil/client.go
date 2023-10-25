package promutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rosenlo/toolkits/http/httpclient"
)

const (
	V1Export      = "/api/v1/export"
	V1QueryRange  = "/api/v1/query_range"
	V1LabelValues = "/api/v1/label/%s/values"
	V1Import      = "/api/v1/import/prometheus"
)

type Option func(*Client)

type Config struct {
	Address       string
	InsertAddress string
}

type Client struct {
	client *httpclient.Client
	cfg    Config
}

func NewClient(cfg Config, opts ...Option) *Client {
	c := &Client{
		client: httpclient.New(nil),
		cfg:    cfg,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func WithBasicAuth(user, pwd string) Option {
	return func(c *Client) {
		c.client.WithBasicAuth(user, pwd)
	}
}

func (c *Client) Export(metric string, start, end int64) ([]byte, error) {
	v := url.Values{}
	v.Add("match[]", metric)
	v.Add("start", strconv.FormatInt(start, 10))
	v.Add("end", strconv.FormatInt(end, 10))

	_url := fmt.Sprintf("%s%s?%s", c.cfg.Address, V1Export, v.Encode())

	headers := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}
	resp, respBody, err := c.client.Request("POST", _url, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to request %s; err: %v", V1Export, err)
	}
	if resp != nil && resp.StatusCode > 399 {
		log.Printf("---> request: %s", _url)
		log.Printf("<--- response status: %s", resp.Status)
		return nil, fmt.Errorf("failed to request %s resp: %s; err: %v", V1Export, string(respBody), err)
	}
	return respBody, nil
}

func (c *Client) QueryRange(metric string, start, end time.Time, step string) (*QueryRangeResponse, error) {
	var response QueryRangeResponse
	v := url.Values{}
	v.Add("query", metric)
	v.Add("start", start.Format(time.RFC3339))
	v.Add("end", end.Format(time.RFC3339))
	if len(step) != 0 {
		v.Add("step", step)
	}

	_url := fmt.Sprintf("%s%s?%s", c.cfg.Address, V1QueryRange, v.Encode())

	headers := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}
	resp, respBody, err := c.client.Request("GET", _url, headers, nil)
	if err != nil || resp.StatusCode > 399 {
		log.Printf("---> request: %s", _url)
		log.Printf("<--- response status: %s", resp.Status)
		return nil, fmt.Errorf("failed to request %s resp: %s; err: %v", V1QueryRange, string(respBody), err)
	}
	err = json.Unmarshal(respBody, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *Client) LabelValues(label string, match string) (*LabelValuesResponse, error) {
	var response LabelValuesResponse

	v := url.Values{}
	v.Add("match[]=", match)

	path := fmt.Sprintf(V1LabelValues, label)
	_url := fmt.Sprintf("%s%s?%s", c.cfg.Address, path, v.Encode())

	headers := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}
	resp, respBody, err := c.client.Request("GET", _url, headers, nil)
	if err != nil || resp.StatusCode > 399 {
		log.Printf("---> request: %s", _url)
		log.Printf("<--- response status: %s", resp.Status)
		return nil, fmt.Errorf("failed to request %s resp: %s; err: %v", path, string(respBody), err)
	}

	err = json.Unmarshal(respBody, &response)
	if err != nil {
		return nil, err
	}

	return &response, nil
}

func MetricFormatter(metric, job string, value any, timestamp int64, labels map[string]string) string {
	labels["job"] = job
	labs := make([]string, 0)
	for k := range labels {
		labs = append(labs, fmt.Sprintf("%s=\"%s\"", k, labels[k]))
	}
	return fmt.Sprintf("%s{%s} %v %d", metric, strings.Join(labs, ","), value, timestamp)
}

func (c *Client) Import(ctx context.Context, payload string) error {
	_url := fmt.Sprintf("%s%s", c.cfg.InsertAddress, V1Import)

	req, err := http.NewRequestWithContext(ctx, "POST", _url, strings.NewReader(payload))

	if err != nil {
		return fmt.Errorf("failed to new request: %v req body: %s", err, payload)
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request: %v req body: %s", err, payload)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read resp body: %v req body: %s", err, payload)
	}

	return fmt.Errorf("vmagent returned error: %s req body: %s", body, payload)
}
