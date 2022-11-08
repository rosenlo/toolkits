package safemap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type param struct {
	key        string
	value      interface{}
	expiration time.Duration
	result     bool
}

func TestSafeMap(t *testing.T) {
	assert := assert.New(t)

	tests := []param{
		{
			"111",
			"111",
			time.Minute,
			true,
		},
		{
			"222",
			222,
			time.Second,
			false,
		},
	}
	m := New(Options{CleanDuration: time.Second})
	for i := range tests {
		m.Set(tests[i].key, tests[i].value, tests[i].expiration)
	}
	time.Sleep(time.Second)
	for i := range tests {
		value, result := m.Get(tests[i].key)
		assert.Equal(tests[i].result, result, "Actual value:%v", value)
	}
}
