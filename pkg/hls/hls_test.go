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
	"path/filepath"
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

func TestParseM3U8Reader(t *testing.T) {
	playlist := "#EXTM3U\nsegA.ts\n../rel/segB.ts\nlast.TS\n"
	r := strings.NewReader(playlist)

	urls, err := ParseM3U8(r)
	if err != nil {
		t.Fatalf("ParseM3U8 reader error: %v", err)
	}
	if len(urls) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(urls))
	}
	if urls[0] != "segA.ts" {
		t.Fatalf("unexpected first entry: %s", urls[0])
	}
	if urls[1] != "../rel/segB.ts" {
		t.Fatalf("unexpected second entry: %s", urls[1])
	}
	if urls[2] != "last.TS" {
		t.Fatalf("unexpected third entry: %s", urls[2])
	}
}

func TestClientParseM3U8LocalFile(t *testing.T) {
	playlist := "#EXTM3U\nlocal1.ts\nlocal2.ts\n"
	f, err := os.CreateTemp("", "playlist-*.m3u8")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	name := f.Name()
	if _, err := f.WriteString(playlist); err != nil {
		_ = f.Close()
		t.Fatalf("write temp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	defer func() { _ = os.Remove(name) }()

	c := NewClient(WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	f2, err := os.Open(name)
	if err != nil {
		t.Fatalf("open temp file: %v", err)
	}
	defer func() {
		if cerr := f2.Close(); cerr != nil {
			t.Fatalf("close temp file: %v", cerr)
		}
	}()
	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	baseURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Dir(abs)) + "/"}
	ts, err := c.ParseM3U8(f2)
	if err != nil {
		t.Fatalf("Client.ParseM3U8 local file error: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(ts))
	}
	if baseURL.Scheme != "file" {
		t.Fatalf("expected file scheme base url, got %v", baseURL)
	}
}

func TestClientParseM3U8FileURL(t *testing.T) {
	playlist := "#EXTM3U\nfile1.ts\nfile2.ts\n"
	f, err := os.CreateTemp("", "playlist-*.m3u8")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	name := f.Name()
	if _, err := f.WriteString(playlist); err != nil {
		_ = f.Close()
		t.Fatalf("write temp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	defer func() { _ = os.Remove(name) }()

	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	c := NewClient(WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	f2, err := os.Open(name)
	if err != nil {
		t.Fatalf("open temp file: %v", err)
	}
	defer func() {
		if cerr := f2.Close(); cerr != nil {
			t.Fatalf("close temp file: %v", cerr)
		}
	}()
	baseURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Dir(abs)) + "/"}
	ts, err := c.ParseM3U8(f2)
	if err != nil {
		t.Fatalf("Client.ParseM3U8 file:// error: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(ts))
	}
	if baseURL.Scheme != "file" {
		t.Fatalf("expected file scheme base url, got %v", baseURL)
	}
}

func TestClientParseM3U8FromURL(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		playlist := "#EXTM3U\nseg1.ts\nseg2.ts\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write([]byte(playlist)); err != nil {
				t.Fatalf("write playlist: %v", err)
			}
		}))
		defer srv.Close()

		c := NewClient(WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		ts, base, err := c.ParseM3U8FromURL(srv.URL + "/playlist.m3u8")
		if err != nil {
			t.Fatalf("ParseM3U8FromURL HTTP error: %v", err)
		}
		if len(ts) != 2 {
			t.Fatalf("expected 2 segments, got %d", len(ts))
		}
		if base == nil || base.Scheme == "" {
			t.Fatalf("expected base url, got %v", base)
		}
	})

	t.Run("file_path", func(t *testing.T) {
		playlist := "#EXTM3U\nfileA.ts\nfileB.ts\n"
		f, err := os.CreateTemp("", "pl-*.m3u8")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		name := f.Name()
		if _, err := f.WriteString(playlist); err != nil {
			_ = f.Close()
			t.Fatalf("write temp: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close temp: %v", err)
		}
		defer func() { _ = os.Remove(name) }()

		c := NewClient(WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		ts, base, err := c.ParseM3U8FromURL(name)
		if err != nil {
			t.Fatalf("ParseM3U8FromURL local path error: %v", err)
		}
		if len(ts) != 2 {
			t.Fatalf("expected 2 segments, got %d", len(ts))
		}
		if base == nil || base.Scheme != "file" {
			t.Fatalf("expected file scheme base url, got %v", base)
		}
	})

	t.Run("file_url", func(t *testing.T) {
		playlist := "#EXTM3U\nfileX.ts\nfileY.ts\n"
		f, err := os.CreateTemp("", "pl-*.m3u8")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		name := f.Name()
		if _, err := f.WriteString(playlist); err != nil {
			_ = f.Close()
			t.Fatalf("write temp: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close temp: %v", err)
		}
		defer func() { _ = os.Remove(name) }()

		abs, err := filepath.Abs(name)
		if err != nil {
			t.Fatalf("abs: %v", err)
		}
		fileURL := "file://" + abs

		c := NewClient(WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		ts, base, err := c.ParseM3U8FromURL(fileURL)
		if err != nil {
			t.Fatalf("ParseM3U8FromURL file:// error: %v", err)
		}
		if len(ts) != 2 {
			t.Fatalf("expected 2 segments, got %d", len(ts))
		}
		if base == nil || base.Scheme != "file" {
			t.Fatalf("expected file scheme base url, got %v", base)
		}
	})
}

func TestFetchSegment(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write([]byte("HELLO")); err != nil {
				t.Fatalf("write hello: %v", err)
			}
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
			if _, err := w.Write([]byte("a.ts\nb.ts\n")); err != nil {
				t.Fatalf("write playlist body: %v", err)
			}
		case "/a.ts":
			if _, err := w.Write([]byte("AAA")); err != nil {
				t.Fatalf("write a.ts: %v", err)
			}
		case "/b.ts":
			if _, err := w.Write([]byte("BBB")); err != nil {
				t.Fatalf("write b.ts: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewClient(WithConcurrency(2), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	// fetch playlist and parse via reader-based API
	resp, err := http.Get(srv.URL + "/playlist.m3u8")
	if err != nil {
		t.Fatalf("http get playlist: %v", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Fatalf("close resp body: %v", cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	base, err := url.Parse(srv.URL + "/playlist.m3u8")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	tsUrls, err := client.ParseM3U8(resp.Body)
	if err != nil {
		t.Fatalf("ParseM3U8: %v", err)
	}

	segments, err := client.DownloadSegments(context.Background(), tsUrls, base, nil)
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
	if err := f.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	defer func() {
		if err := os.Remove(fname); err != nil {
			t.Logf("remove temp file: %v", err)
		}
	}()

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

func TestParseM3U8(t *testing.T) {
	t.Run("reader_basic", func(t *testing.T) {
		playlist := "#EXTM3U\nsegA.ts\n../rel/segB.ts\nlast.TS\n"
		r := strings.NewReader(playlist)

		urls, err := ParseM3U8(r)
		if err != nil {
			t.Fatalf("ParseM3U8 reader error: %v", err)
		}
		if len(urls) != 3 {
			t.Fatalf("expected 3 entries, got %d", len(urls))
		}
		if urls[0] != "segA.ts" {
			t.Fatalf("unexpected first entry: %s", urls[0])
		}
	})

	t.Run("http_reader", func(t *testing.T) {
		playlist := "#EXTM3U\nsegment1.ts\nhttp://example.com/abs/segment2.ts\nsegment3.TS\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write([]byte(playlist)); err != nil {
				t.Fatalf("write playlist: %v", err)
			}
		}))
		defer srv.Close()

		client := NewClient(WithConcurrency(DefaultConcurrency), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		resp, err := http.Get(srv.URL + "/playlist.m3u8")
		if err != nil {
			t.Fatalf("http get playlist: %v", err)
		}
		defer func() {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Fatalf("close resp body: %v", cerr)
			}
		}()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status: %d", resp.StatusCode)
		}
		tsUrls, err := client.ParseM3U8(resp.Body)
		if err != nil {
			t.Fatalf("ParseM3U8 error: %v", err)
		}
		if len(tsUrls) != 3 {
			t.Fatalf("expected 3 ts urls, got %d", len(tsUrls))
		}
	})

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

	segments, err := c.DownloadSegments(ctx, ts, base, nil)
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
