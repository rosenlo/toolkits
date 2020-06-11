package rest

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/rosenlo/toolkits/log"

	"github.com/parnurzeal/gorequest"
)

type Request struct {
	baseURL string
	req     *gorequest.SuperAgent
}

func NewRequest(base string) *Request {
	return &Request{
		req:     gorequest.New().TLSClientConfig(&tls.Config{InsecureSkipVerify: true}),
		baseURL: base,
	}
}

func printResponse(rsp gorequest.Response, body []byte, errs []error) {
	for _, err := range errs {
		if err != nil {
			log.Error(err)
		}
	}
	log.WithFields(map[string]interface{}{
		"method":      rsp.Request.Method,
		"url":         rsp.Request.URL,
		"status":      rsp.Status,
		"status_code": rsp.StatusCode,
		// "response":    string(body),
	}).Debug()
}

func (r *Request) AddHeader(header map[string]string) *Request {
	for param, value := range header {
		r.req.Header.Set(param, value)
	}
	return r
}

func (r *Request) AddParams(params map[string]string) *Request {
	for key, value := range params {
		r.req.Param(key, value)
	}
	return r
}

func (r *Request) Get(uri string) *Request {
	url := fmt.Sprintf("%s%s", r.baseURL, uri)
	r.req.Get(url)
	return r
}

func (r *Request) Post(uri string, body interface{}) *Request {
	url := fmt.Sprintf("%s%s", r.baseURL, uri)
	r.req.Post(url).Send(body)
	return r
}
func (r *Request) Put(uri string, body interface{}) *Request {
	url := fmt.Sprintf("%s%s", r.baseURL, uri)
	r.req.Put(url).Send(body)
	return r
}
func (r *Request) Delete(uri string) *Request {
	url := fmt.Sprintf("%s%s", r.baseURL, uri)
	r.req.Delete(url)
	return r
}

func (r *Request) EndBytes(callback ...func(response gorequest.Response, body []byte, errs []error)) (gorequest.Response, []byte, []error) {
	callback = append(callback, printResponse)
	return r.req.EndBytes(callback...)
}

func (r *Request) End(callback ...func(response gorequest.Response, body []byte, errs []error)) (*http.Response, string, []error) {
	resp, body, errs := r.EndBytes(callback...)
	bodyString := string(body)
	return resp, bodyString, errs
}
