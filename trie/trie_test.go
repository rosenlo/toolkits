package trie

import (
	"testing"
)

var (
	tests = []string{
		"/api/v1/user",
		"/home/user",
		"/not_in_whitelist",
	}
)

func TestTrieTreeStartsWith(t *testing.T) {
	whiteList := New()
	whiteList.Insert("/api/v1")
	whiteList.Insert("/home")
	result := []bool{
		true,
		true,
		false,
	}

	for i := range tests {
		if ret := whiteList.StartsWith(tests[i]); ret != result[i] {
			t.Fatalf("Wrong Answer, ret: %v result: %v; test case: %v", ret, result[i], tests[i])
		}
	}
}

func TestTrieTreeSearch(t *testing.T) {
	whiteList := New()
	whiteList.Insert("/api/v1/user")
	whiteList.Insert("/home")
	whiteList.Insert("/not_in")

	result := []bool{
		true,
		false,
		false,
	}

	for i := range tests {
		if ret := whiteList.Search(tests[i]); ret != result[i] {
			t.Fatalf("Wrong Answer, ret: %v result: %v; test case: %v", ret, result[i], tests[i])
		}
	}
}
