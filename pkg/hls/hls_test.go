package hls

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRT is a simple RoundTripper used for testing injection of http.Client.
type fakeRT struct {
	Called bool
	Body   string
	Status int
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.Called = true
	return &http.Response{
		StatusCode: f.Status,
		Body:       io.NopCloser(strings.NewReader(f.Body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestParseM3U8(t *testing.T) {
	playlist := "#EXTM3U\nsegment1.ts\nhttp://example.com/abs/segment2.ts\nsegment3.TS\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(playlist))
	}))
	defer srv.Close()

	client := NewClient(WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	tsUrls, base, err := client.ParseM3U8(srv.URL + "/playlist.m3u8")
	if err != nil {
		t.Fatalf("ParseM3U8 error: %v", err)
	}

	if len(tsUrls) != 3 {
		t.Fatalf("expected 3 ts urls, got %d", len(tsUrls))
	}

	if tsUrls[0] != "segment1.ts" {
		t.Fatalf("unexpected first entry: %s", tsUrls[0])
	}

	if tsUrls[1] != "http://example.com/abs/segment2.ts" {
		t.Fatalf("unexpected second entry: %s", tsUrls[1])
	}

	if tsUrls[2] != "segment3.TS" {
		t.Fatalf("unexpected third entry: %s", tsUrls[2])
	}

	if base == nil {
		t.Fatalf("expected non-nil base URL")
	}

	// Ensure base resolves relative correctly
	resolved := base.ResolveReference(&url.URL{Path: "segment1.ts"}).String()
	if resolved == "" {
		t.Fatalf("resolve returned empty string")
	}
}

// NOTE: success case is included as a subtest in TestFetchSegment below.

func TestFetchSegment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("HELLO"))
		}))
		defer srv.Close()

		c := NewClient(WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		data, err := c.fetchSegment(context.Background(), srv.URL+"/a.ts")
		if err != nil {
			t.Fatalf("fetchSegment error: %v", err)
		}
		if string(data) != "HELLO" {
			t.Fatalf("unexpected body: %s", string(data))
		}
	})

	t.Run("not_found", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()

		c := NewClient(WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		_, err := c.fetchSegment(context.Background(), srv.URL+"/missing.ts")
		if err == nil {
			t.Fatalf("expected error for 404 response")
		}
	})

	t.Run("uses_injected_client", func(t *testing.T) {
		t.Parallel()
		frt := &fakeRT{Body: "OK", Status: 200}
		httpc := &http.Client{Transport: frt}
		c := NewClient(WithHTTPClient(httpc), WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

		data, err := c.fetchSegment(context.Background(), "http://example.local/test")
		if err != nil {
			t.Fatalf("fetchSegment with injected client error: %v", err)
		}
		if string(data) != "OK" {
			t.Fatalf("unexpected body: %s", string(data))
		}
		if !frt.Called {
			t.Fatalf("expected injected RoundTripper to be called")
		}
	})
}

func TestDownloadAndCombineSegments(t *testing.T) {
	// Serve two segments
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlist.m3u8":
			w.Write([]byte("a.ts\nb.ts\n"))
		case "/a.ts":
			w.Write([]byte("AAA"))
		case "/b.ts":
			w.Write([]byte("BBB"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(WithConcurrency(2), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	tsUrls, base, err := client.ParseM3U8(srv.URL + "/playlist.m3u8")
	if err != nil {
		t.Fatalf("ParseM3U8: %v", err)
	}

	segments, err := client.DownloadSegments(context.Background(), tsUrls, base)
	if err != nil {
		t.Fatalf("DownloadSegments error: %v", err)
	}

	if len(segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segments))
	}

	// Write combined output to temp file
	f, err := os.CreateTemp("", "combined-*.ts")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	fname := f.Name()
	f.Close()
	defer os.Remove(fname)

	if err := client.CombineSegments(segments, fname); err != nil {
		t.Fatalf("CombineSegments error: %v", err)
	}

	data, err := os.ReadFile(fname)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(data) != "AAABBB" {
		t.Fatalf("unexpected combined content: %s", string(data))
	}
}

func TestCombineSegmentsOnly(t *testing.T) {
	// Simple combine without download
	segments := [][]byte{[]byte("X"), []byte("Y")}
	f, err := os.CreateTemp("", "combine-*.ts")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	fname := f.Name()
	f.Close()
	defer os.Remove(fname)

	c := NewClient(WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err := c.CombineSegments(segments, fname); err != nil {
		t.Fatalf("CombineSegments: %v", err)
	}

	data, err := os.ReadFile(fname)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(data) != "XY" {
		t.Fatalf("unexpected data: %s", string(data))
	}
}

// flakyRT simulates transient errors for the first N calls, then succeeds.
type flakyRT struct {
	Calls     int
	FailUntil int
	Body      string
	Status    int
}

func (f *flakyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.Calls++
	if f.Calls <= f.FailUntil {
		return nil, fmt.Errorf("transient error")
	}
	return &http.Response{
		StatusCode: f.Status,
		Body:       io.NopCloser(strings.NewReader(f.Body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestFetchSegmentRetry(t *testing.T) {
	t.Run("retry_success", func(t *testing.T) {
		frt := &flakyRT{FailUntil: 1, Body: "OK", Status: 200}
		httpc := &http.Client{Transport: frt}
		c := NewClient(WithHTTPClient(httpc), WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		c.Retry = 2
		c.Backoff = 5 * time.Millisecond

		data, err := c.fetchSegment(context.Background(), "http://example.local/test")
		if err != nil {
			t.Fatalf("expected success after retry, got error: %v", err)
		}
		if string(data) != "OK" {
			t.Fatalf("unexpected body: %s", string(data))
		}
		if frt.Calls != 2 {
			t.Fatalf("expected 2 calls, got %d", frt.Calls)
		}
	})

	t.Run("retry_exhausted", func(t *testing.T) {
		frt := &flakyRT{FailUntil: 3, Body: "OK", Status: 200}
		httpc := &http.Client{Transport: frt}
		c := NewClient(WithHTTPClient(httpc), WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		c.Retry = 1
		c.Backoff = 5 * time.Millisecond

		_, err := c.fetchSegment(context.Background(), "http://example.local/test")
		if err == nil {
			t.Fatalf("expected error after retries exhausted")
		}
		if frt.Calls != 2 { // initial attempt + 1 retry
			t.Fatalf("expected 2 calls, got %d", frt.Calls)
		}
	})
}

// TestDownloadSegmentsConcurrency ensures DownloadSegments does not exceed the
// configured concurrency limit by observing the maximum number of concurrent
// handler invocations on the server.
func TestDownloadSegmentsConcurrency(t *testing.T) {
	const concurrency = 3
	const total = 12

	var cur int32
	var max int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// increment current concurrent count
		c := atomic.AddInt32(&cur, 1)
		// update max if needed
		for {
			prev := atomic.LoadInt32(&max)
			if c <= prev || atomic.CompareAndSwapInt32(&max, prev, c) {
				break
			}
		}

		// keep the handler busy a little to create overlap
		time.Sleep(30 * time.Millisecond)

		atomic.AddInt32(&cur, -1)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()

	// create tsUrls
	ts := make([]string, total)
	for i := 0; i < total; i++ {
		ts[i] = fmt.Sprintf("seg%d.ts", i)
	}

	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}

	// client with limited concurrency; disable retries to avoid interfering
	c := NewClient(WithConcurrency(concurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithRetry(0))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	segments, err := c.DownloadSegments(ctx, ts, base)
	if err != nil {
		t.Fatalf("DownloadSegments error: %v", err)
	}

	if len(segments) != total {
		t.Fatalf("expected %d segments, got %d", total, len(segments))
	}

	if atomic.LoadInt32(&max) > concurrency {
		t.Fatalf("expected max concurrent <= %d, got %d", concurrency, atomic.LoadInt32(&max))
	}
}
