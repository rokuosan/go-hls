package hls

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

const DefaultConcurrency = 8

// Client encapsulates HTTP client and options for HLS operations.
type Client struct {
	HTTP        *http.Client
	Concurrency int
	Logger      *slog.Logger
	// Retry controls how many retries are attempted (on top of the first attempt).
	// If Retry == 0, no retries are performed.
	Retry int
	// Backoff is the base backoff duration between retries; it is multiplied
	// exponentially for subsequent attempts (Backoff * 2^attempt).
	Backoff time.Duration
	// Jitter is the fractional jitter applied to computed backoff durations.
	// The effective sleep is Backoff*(2^attempt) * factor where factor is in
	// [1-Jitter, 1+Jitter]. For example, Jitter=0.1 produces factors in [0.9,1.1].
	// A value of 0 disables jitter.
	Jitter float64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets custom http.Client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.HTTP = h }
}

// WithConcurrency sets concurrency.
func WithConcurrency(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.Concurrency = n
		}
	}
}

// WithLogger sets slog logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.Logger = l
		}
	}
}

// WithRetry sets retry count.
func WithRetry(r int) Option {
	return func(c *Client) {
		if r >= 0 {
			c.Retry = r
		}
	}
}

// WithBackoff sets base backoff duration.
func WithBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.Backoff = d
		}
	}
}

// WithJitter sets the fractional jitter for backoff sleeps (0.0 means disabled).
func WithJitter(j float64) Option {
	return func(c *Client) {
		if j >= 0 {
			c.Jitter = j
		}
	}
}

// NewClient creates a configured Client using functional options.
// Defaults: HTTP=http.DefaultClient, Concurrency=DefaultConcurrency, Logger=slog.Default(), Retry=2, Backoff=200ms
func NewClient(opts ...Option) *Client {
	c := &Client{
		HTTP:        http.DefaultClient,
		Concurrency: DefaultConcurrency,
		Logger:      slog.Default(),
		Retry:       2,
		Backoff:     200 * time.Millisecond,
		Jitter:      0.1,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}
func (c *Client) ParseM3U8(m3u8URL string) ([]string, *url.URL, error) {
	resp, err := c.HTTP.Get(m3u8URL)
	if err != nil {
		return nil, nil, fmt.Errorf("HTTP request error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("server error: status code %d", resp.StatusCode)
	}

	baseURL, err := url.Parse(m3u8URL)
	if err != nil {
		return nil, nil, fmt.Errorf("url parse error: %w", err)
	}

	var tsUrls []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#") && strings.HasSuffix(strings.ToLower(line), ".ts") {
			tsUrls = append(tsUrls, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("m3u8 scan error: %w", err)
	}

	return tsUrls, baseURL, nil
}

// DownloadSegments downloads all segments in parallel and returns the byte slices.
func (c *Client) DownloadSegments(parentCtx context.Context, tsUrls []string, baseURL *url.URL) ([][]byte, error) {
	segments := make([][]byte, len(tsUrls))
	errs := make(chan error, len(tsUrls))

	var wg sync.WaitGroup
	limiter := make(chan struct{}, c.Concurrency)

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// http client is accessed via c.HTTP inside fetchSegment

	for i, relPath := range tsUrls {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		limiter <- struct{}{}

		go func(index int, relativeURL string) {
			defer wg.Done()
			defer func() { <-limiter }()

			fullURL := resolveURL(baseURL, relativeURL)

			c.Logger.Info("downloading segment", "file", path.Base(fullURL), "index", index+1, "total", len(tsUrls))

			data, err := c.fetchSegment(ctx, fullURL)
			if err != nil {
				errs <- fmt.Errorf("segment %d download error: %w", index+1, err)
				cancel()
				return
			}

			segments[index] = data
		}(i, relPath)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return nil, err
		}
	}

	return segments, nil
}

// resolveURL combines base and relative URLs; kept unexported.
func resolveURL(baseURL *url.URL, relativeURL string) string {
	u, err := baseURL.Parse(relativeURL)
	if err != nil {
		return baseURL.String() + relativeURL
	}
	return u.String()
}

// fetchSegment performs a single HTTP GET for the given full URL and
// returns the response body bytes. It uses the client's configured HTTP client.
func (c *Client) fetchSegment(ctx context.Context, fullURL string) ([]byte, error) {
	attempts := c.Retry + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, fmt.Errorf("request creation error: %w", err)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt == attempts-1 {
				return nil, fmt.Errorf("http do error: %w", err)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(applyJitter(c.Backoff*(1<<attempt), c.Jitter)):
			}
			continue
		}

		// handle non-200
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 500 && resp.StatusCode < 600 && attempt < attempts-1 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(applyJitter(c.Backoff*(1<<attempt), c.Jitter)):
				}
				continue
			}
			return nil, fmt.Errorf("server error: status code %d", resp.StatusCode)
		}

		data, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			if attempt == attempts-1 {
				return nil, fmt.Errorf("read error: %w", rerr)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(applyJitter(c.Backoff*(1<<attempt), c.Jitter)):
			}
			continue
		}

		return data, nil
	}

	return nil, fmt.Errorf("exhausted attempts")
}

// CombineSegments writes the downloaded segments into the output file in order.
func (c *Client) CombineSegments(segments [][]byte, outputFileName string) error {
	outFile, err := os.Create(outputFileName)
	if err != nil {
		return fmt.Errorf("output file create error: %w", err)
	}
	defer outFile.Close()

	c.Logger.Info("combining segments", "count", len(segments), "out", outputFileName)

	for i, data := range segments {
		if data == nil {
			return fmt.Errorf("segment %d data missing", i+1)
		}

		if _, err := outFile.Write(data); err != nil {
			return fmt.Errorf("segment %d write error: %w", i+1, err)
		}
	}

	return nil
}

// applyJitter applies fractional jitter to a duration. If jitter <= 0 it
// returns the original duration. jitter is treated as a fraction (e.g. 0.1).
func applyJitter(d time.Duration, jitter float64) time.Duration {
	if jitter <= 0 {
		return d
	}
	// factor in [1-jitter, 1+jitter]
	f := 1 - jitter + rand.Float64()*(2*jitter)
	if f < 0 {
		f = 0
	}
	return time.Duration(float64(d) * f)
}

// Backwards-compatible package-level defaults
var defaultClient = NewClient()

// ParseM3U8 is a convenience wrapper using default client.
func ParseM3U8(m3u8URL string) ([]string, *url.URL, error) { return defaultClient.ParseM3U8(m3u8URL) }

// DownloadSegments is a convenience wrapper using default client.
func DownloadSegments(ctx context.Context, tsUrls []string, baseURL *url.URL, concurrency int) ([][]byte, error) {
	// ignore provided concurrency; keep compatibility by creating a temp client
	c := NewClient(WithConcurrency(concurrency))
	return c.DownloadSegments(ctx, tsUrls, baseURL)
}

// CombineSegments wrapper
func CombineSegments(segments [][]byte, outputFileName string) error {
	return defaultClient.CombineSegments(segments, outputFileName)
}
