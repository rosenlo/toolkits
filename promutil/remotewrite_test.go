package promutil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

// testBatchRequest returns a WriteRequest with one series so the batch fan-out
// has something to send.
func testBatchRequest() *prompb.WriteRequest {
	return &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: []prompb.Label{{Name: "__name__", Value: "test_metric"}},
				Samples: []prompb.Sample{
					{Value: 1, Timestamp: time.Now().UnixMilli()},
				},
			},
		},
	}
}

func TestWriteErrorRetryableClassification(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"transport error", 0, true},
		{"429 too many requests", http.StatusTooManyRequests, true},
		{"500 internal", http.StatusInternalServerError, true},
		{"503 unavailable", http.StatusServiceUnavailable, true},
		{"400 bad request", http.StatusBadRequest, false},
		{"401 unauthorized", http.StatusUnauthorized, false},
		{"404 not found", http.StatusNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &WriteError{StatusCode: tc.statusCode}
			if got := retryableWriteError(e); got != tc.want {
				t.Fatalf("retryableWriteError(%d) = %v, want %v", tc.statusCode, got, tc.want)
			}
		})
	}
}

func TestBatchRemoteWriteWithRetrySucceedsOnTransientFailure(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			// First attempt: connection-level failure (simulate by closing
			// without a response via a panic-style handler is unreliable; use a
			// 503 instead, which is also retryable).
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(Config{InsertAddress: server.URL})
	err := BatchRemoteWriteWithRetry(
		context.Background(),
		client,
		testBatchRequest(),
		100,
		RetryConfig{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
	)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (initial + one retry)", got)
	}
}

func TestBatchRemoteWriteWithRetryExhaustsRetries(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(Config{InsertAddress: server.URL})
	err := BatchRemoteWriteWithRetry(
		context.Background(),
		client,
		testBatchRequest(),
		100,
		RetryConfig{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
	)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (MaxRetries=2 means 3 total attempts)", got)
	}
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("expected *WriteError, got %T", err)
	}
}

func TestBatchRemoteWriteWithRetryDoesNotRetryClientError(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client := NewClient(Config{InsertAddress: server.URL})
	err := BatchRemoteWriteWithRetry(
		context.Background(),
		client,
		testBatchRequest(),
		100,
		RetryConfig{MaxRetries: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
	)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (400 is not retryable)", got)
	}
}

func TestBatchRemoteWriteWithRetryRetriesTransportError(t *testing.T) {
	// Use a server that accepts the connection but resets it on the first
	// request, then serves normally. A server that closes without responding
	// yields a transport-level error (read on a reset connection).
	var attempts atomic.Int64
	var mu sync.Mutex
	var resetOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		resetOnce.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		})
		if attempts.Load() > 1 {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := NewClient(Config{InsertAddress: server.URL})
	err := BatchRemoteWriteWithRetry(
		context.Background(),
		client,
		testBatchRequest(),
		100,
		RetryConfig{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
	)
	if err != nil {
		t.Fatalf("expected transport-error retry to succeed, got: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestBatchRemoteWriteWithRetryHonorsContextCancel(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	client := NewClient(Config{InsertAddress: server.URL})
	err := BatchRemoteWriteWithRetry(
		ctx,
		client,
		testBatchRequest(),
		100,
		RetryConfig{MaxRetries: 3, InitialBackoff: time.Second, MaxBackoff: time.Second},
	)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context cancellation, got: %v", err)
	}
}

func TestBatchRemoteWriteBackwardCompatibleNoRetry(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(Config{InsertAddress: server.URL})
	err := BatchRemoteWrite(context.Background(), client, testBatchRequest(), 100)
	if err == nil {
		t.Fatal("expected error from BatchRemoteWrite without retry")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (legacy call does not retry)", got)
	}
}

func TestBatchRemoteWriteProgressReportsWhereItStopped(t *testing.T) {
	var batches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if batches.Add(1) == 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req := &prompb.WriteRequest{}
	for i := 0; i < 10; i++ {
		req.Timeseries = append(req.Timeseries, testBatchRequest().Timeseries...)
	}
	client := NewClient(Config{InsertAddress: server.URL})
	written, err := BatchRemoteWriteProgress(context.Background(), client, req, 3, RetryConfig{})
	if err == nil {
		t.Fatal("expected the third batch to fail")
	}
	if written != 6 {
		t.Fatalf("written = %d, want 6 (two batches of 3 before the failure)", written)
	}

	written, err = BatchRemoteWriteProgress(context.Background(), client,
		&prompb.WriteRequest{Timeseries: req.Timeseries[written:]}, 3, RetryConfig{})
	if err != nil || written != 4 {
		t.Fatalf("resume: written = %d, err = %v, want the remaining 4", written, err)
	}
}

func TestBatchRemoteWriteProgressSendsAllAtOnceWithoutABatchSize(t *testing.T) {
	var batches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		batches.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req := &prompb.WriteRequest{}
	for i := 0; i < 5; i++ {
		req.Timeseries = append(req.Timeseries, testBatchRequest().Timeseries...)
	}
	written, err := BatchRemoteWriteProgress(context.Background(), NewClient(Config{InsertAddress: server.URL}), req, 0, RetryConfig{})
	if err != nil || written != 5 || batches.Load() != 1 {
		t.Fatalf("written = %d, err = %v, batches = %d", written, err, batches.Load())
	}
}
