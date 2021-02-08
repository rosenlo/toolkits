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
	Dial: (&net.Dialer{
		Timeout:   DefaultDialTimeout,
		KeepAlive: DefaultKeepAliveTimeout,
	}).Dial,
}

type HttpClient struct {
	client *http.Client
	header map[string]string
}

func New(transport *http.Transport) *HttpClient {
	if transport == nil {
		transport = DefaultTransport
	}
	return &HttpClient{
		client: &http.Client{
			Transport: transport,
		},
		header: make(map[string]string),
	}
}

func (s *HttpClient) Request(method, url string, header map[string]string, body []byte) (rsp *http.Response, respBody []byte, err error) {
	var req *http.Request
	if body != nil {
		req, err = http.NewRequest(method, url, bytes.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}

	req.Close = true

	if err != nil {
		return
	}

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
