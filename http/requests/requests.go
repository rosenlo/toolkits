package requests

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/parnurzeal/gorequest"
)

var (
	request = NewRequest(false)
)

type Requests struct {
	req     *gorequest.SuperAgent
	verbose bool
}

func NewRequest(verbose bool) *Requests {
	return &Requests{req: gorequest.New().TLSClientConfig(&tls.Config{InsecureSkipVerify: true}), verbose: verbose}
}

func Request() *Requests {
	return request
}

func printResponse(resp gorequest.Response, body []byte, errs []error) {
	fmt.Println("===>", resp.Request.Method, resp.Request.URL)
	for _, err := range errs {
		if err != nil {
			fmt.Println(err)
		}
	}
	fmt.Println("<=== Resp Status:", resp.Status)
	fmt.Printf("<=== Resp Body: %s\n", string(body))
	fmt.Println("<=== Done")
}

func (r *Requests) Get(url string) *Requests {
	r.req.Get(url)
	return r
}

func (r *Requests) AddHeader(header map[string]string) *Requests {
	for param, value := range header {
		r.req.Header.Set(param, value)
	}
	return r
}

func (r *Requests) Post(url string, body interface{}) *Requests {
	r.req.Post(url).Send(body)
	return r
}
func (r *Requests) Put(url string, body interface{}) *Requests {
	r.req.Put(url).Send(body)
	return r
}
func (r *Requests) Delete(url string) *Requests {
	r.req.Delete(url)
	return r
}

func (r *Requests) EndBytes(callback ...func(response gorequest.Response, body []byte, errs []error)) (*http.Response, []byte, []error) {
	if r.verbose {
		callback = append(callback, printResponse)
	}
	return r.req.EndBytes(callback...)
}

func (r *Requests) End(callback ...func(response gorequest.Response, body []byte, errs []error)) (*http.Response, string, []error) {
	resp, body, errs := r.EndBytes(callback...)
	bodyString := string(body)
	return resp, bodyString, errs
}
