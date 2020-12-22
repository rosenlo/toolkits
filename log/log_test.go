package log

import "testing"

func TestInit(t *testing.T) {
	Init("debug", nil, nil)
	SetField("AppID", "unittest")
	Info("info")
	Warn("warn")
	Error("error")
}
