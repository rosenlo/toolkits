package requests

import (
	"encoding/json"
	"errors"
	"io/ioutil"

	"github.com/RosenLo/toolkits/common"
	log "github.com/Sirupsen/logrus"

	"github.com/astaxie/beego/httplib"
)

func Call(method, url string, headers, params map[string]string, body map[string]interface{}, logger *log.Entry) (respData []byte, err error) {
	var req *httplib.BeegoHTTPRequest
	whileCode := []int{200, 201}
	fields := log.Fields{
		"url":     url,
		"method":  method,
		"headers": headers,
		"params":  params,
		"body":    body,
	}
	if logger == nil {
		logger = log.WithFields(fields)
	} else {
		logger = logger.WithFields(fields)
	}

	if method == "GET" {
		req = httplib.Get(url)
	} else if method == "POST" {
		req = httplib.Post(url)
	} else if method == "PUT" {
		req = httplib.Put(url)
	} else if method == "DELETE" {
		req = httplib.Delete(url)
	} else if method == "HEAD" {
		req = httplib.Head(url)
	} else {
		err = errors.New("invalid http method")
		return
	}

	req.Header("Content-type", "application/json")

	for hk, hv := range headers {
		req.Header(hk, hv)
	}

	for pk, pv := range params {
		req.Param(pk, pv)
	}

	data, _ := json.Marshal(body)
	req.Body(data)

	resp, err := req.Response()
	if err != nil {
		logger.Error("request failed, due to ", err)
		return
	}
	defer resp.Body.Close()
	respData, err = ioutil.ReadAll(resp.Body)
	logger.WithField("result", string(respData)).Debug()

	if !common.Contains(whileCode, resp.StatusCode) {
		logger.Error("error code:", resp.StatusCode)
		logger.Error("fail reason: ", err)
		err = errors.New(string(respData))
		return
	}
	if resp.Body == nil {
		return
	}

	return
}
