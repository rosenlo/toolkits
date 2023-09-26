package httpclient

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	DefaultDialTimeout         = 5 * time.Second
	DefaultKeepAliveTimeout    = 30 * time.Second
	DefaultMaxIdleConnsPerHost = 10
)

var DefaultTransport = &http.Transport{
	MaxIdleConnsPerHost: DefaultMaxIdleConnsPerHost,
	DialContext: (&net.Dialer{
		Timeout:   DefaultDialTimeout,
		KeepAlive: DefaultKeepAliveTimeout,
	}).DialContext,
}

type Client struct {
	client *http.Client
	user   string
	pwd    string
}

func New(transport *http.Transport) *Client {
	if transport == nil {
		transport = DefaultTransport
	}
	return &Client{
		client: &http.Client{
			Transport: transport,
		},
	}
}

func (s *Client) WithTimeout(timeout time.Duration) {
	s.client.Timeout = timeout
}

func (s *Client) WithBasicAuth(user, pwd string) {
	s.user = user
	s.pwd = pwd
}

func (s *Client) Request(method, url string, header map[string]string, body []byte) (rsp *http.Response, respBody []byte, err error) {
	return s.RequestWithContext(context.Background(), method, url, header, body)
}

func (s *Client) RequestWithContext(ctx context.Context, method, url string, header map[string]string, body []byte) (rsp *http.Response, respBody []byte, err error) {
	var req *http.Request
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}

	if err != nil {
		return
	}

	if len(s.user) != 0 && len(s.pwd) != 0 {
		req.SetBasicAuth(s.user, s.pwd)
	}

	for key, value := range header {
		req.Header.Set(key, value)
	}

	if value, exists := header["Host"]; exists {
		req.Host = value
	}

	rsp, err = s.client.Do(req)
	if err != nil {
		return
	}

	defer rsp.Body.Close()

	respBody, err = io.ReadAll(rsp.Body)

	return
}
