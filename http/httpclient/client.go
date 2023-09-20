package httpclient

import (
	"bytes"
	"io/ioutil"
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
	header map[string]string
}

func New(transport *http.Transport) *Client {
	if transport == nil {
		transport = DefaultTransport
	}
	return &Client{
		client: &http.Client{
			Transport: transport,
		},
		header: make(map[string]string),
	}
}

func (s *Client) Request(method, url string, header map[string]string, body []byte) (rsp *http.Response, respBody []byte, err error) {
	var req *http.Request
	if body != nil {
		req, err = http.NewRequest(method, url, bytes.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}

	if err != nil {
		return
	}

	req.Close = true

	for key, value := range header {
		req.Header.Set(key, value)
	}

	rsp, err = s.client.Do(req)
	if err != nil {
		return
	}

	defer rsp.Body.Close()

	respBody, err = ioutil.ReadAll(rsp.Body)

	return
}
