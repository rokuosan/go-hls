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
	"path/filepath"

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

// ParseM3U8 parses the playlist from the provided io.Reader using the
// reader-based parser. Callers pass an optional baseURL used to resolve
// relative segment paths; the method returns the parsed segment list and the
// same baseURL.
func (c *Client) ParseM3U8(r io.Reader) ([]string, error) {
	// Pure parser: return the listed .ts entries from the provided reader.
	scanner := bufio.NewScanner(r)
	var tsUrls []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#") && strings.HasSuffix(strings.ToLower(line), ".ts") {
			tsUrls = append(tsUrls, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("m3u8 parse error: %w", err)
	}
	return tsUrls, nil
}

// ParseM3U8FromURL opens the given playlist source which may be an HTTP/HTTPS
// URL, a file:// URL, or a local filesystem path. It parses the playlist and
// returns the segment list and a base URL for resolving relative segment
// references.
// ParseM3U8FromURL opens the given playlist source with the client's HTTP
// settings and parses it. This is provided as a method so callers can use a
// configured `Client` (custom http.Client, logger, etc.).
func (c *Client) ParseM3U8FromURL(m3u8URL string) ([]string, *url.URL, error) {
	// First try parsing as a URL. If parsing succeeds and a scheme is
	// present, handle according to the scheme. Otherwise treat the input
	// as a local filesystem path. The file handling for file:// and plain
	// paths is unified via a single code path (label `fileOpen`).

	var pathToOpen string
	var baseURL *url.URL

	u, err := url.Parse(m3u8URL)
	if err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "http", "https":
			resp, err := c.HTTP.Get(m3u8URL)
			if err != nil {
				return nil, nil, fmt.Errorf("HTTP request error: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				if cerr := resp.Body.Close(); cerr != nil {
					return nil, nil, fmt.Errorf("server error: status code %d; close error: %w", resp.StatusCode, cerr)
				}
				return nil, nil, fmt.Errorf("server error: status code %d", resp.StatusCode)
			}
			defer func() {
				if cerr := resp.Body.Close(); cerr != nil {
					if c != nil && c.Logger != nil {
						c.Logger.Warn("failed to close response body", "err", cerr)
					}
				}
			}()

			parsedURL, err := url.Parse(m3u8URL)
			if err != nil {
				return nil, nil, fmt.Errorf("url parse error: %w", err)
			}
			ts, err := c.ParseM3U8(resp.Body)
			return ts, parsedURL, err
		case "file":
			// Reject file:// with a non-local host (except localhost).
			if u.Host != "" && u.Host != "localhost" {
				return nil, nil, fmt.Errorf("unsupported file host: %s", u.Host)
			}

			p, err := url.PathUnescape(u.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid file path escape: %w", err)
			}

			pathToOpen = filepath.FromSlash(p)
		default:
		}
	}

	if pathToOpen == "" {
		pathToOpen = filepath.Clean(m3u8URL)
	}

	abs, err := filepath.Abs(pathToOpen)
	if err != nil {
		return nil, nil, fmt.Errorf("abs path error: %w", err)
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("open file error: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			if c != nil && c.Logger != nil {
				c.Logger.Warn("failed to close file", "path", abs, "err", cerr)
			}
		}
	}()

	dir := filepath.Dir(abs)
	baseURL = &url.URL{Scheme: "file", Path: filepath.ToSlash(dir) + "/"}
	ts, err := c.ParseM3U8(f)
	return ts, baseURL, err
}

// DownloadSegments downloads all segments in parallel and returns the byte slices.
// If progress is non-nil, the function will send `1` on the channel for each
// successfully completed segment. The channel is not closed by this function.
func (c *Client) DownloadSegments(parentCtx context.Context, tsUrls []string, baseURL *url.URL, progress chan<- int) ([][]byte, error) {
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
			if progress != nil {
				select {
				case progress <- 1:
				case <-ctx.Done():
				}
			}
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
			if _, cerr := io.Copy(io.Discard, resp.Body); cerr != nil {
				// if drain fails and it's a 5xx we can retry, otherwise return the drain error
				if resp.StatusCode >= 500 && resp.StatusCode < 600 && attempt < attempts-1 {
					if c != nil && c.Logger != nil {
						c.Logger.Warn("failed to drain response body, will retry", "err", cerr)
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(applyJitter(c.Backoff*(1<<attempt), c.Jitter)):
					}
					continue
				}
				return nil, fmt.Errorf("drain error: %w", cerr)
			}

			if cerr := resp.Body.Close(); cerr != nil {
				if resp.StatusCode >= 500 && resp.StatusCode < 600 && attempt < attempts-1 {
					if c != nil && c.Logger != nil {
						c.Logger.Warn("failed to close response body, will retry", "err", cerr)
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(applyJitter(c.Backoff*(1<<attempt), c.Jitter)):
					}
					continue
				}
				return nil, fmt.Errorf("close error: %w", cerr)
			}

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
		if cerr := resp.Body.Close(); cerr != nil {
			if rerr != nil {
				rerr = fmt.Errorf("%v; close error: %w", rerr, cerr)
			} else {
				// non-nil close error and no read error
				rerr = fmt.Errorf("close error: %w", cerr)
			}
		}
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
	defer func() {
		if cerr := outFile.Close(); cerr != nil {
			if c != nil && c.Logger != nil {
				c.Logger.Warn("failed to close output file", "err", cerr)
			}
		}
	}()

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

// Note: package-level `ParseM3U8` now parses from an `io.Reader`.

// ParseM3U8 parses an M3U8 playlist from an io.Reader and returns the listed
// .ts segment paths in order. This package-level function is a thin wrapper
// around the default client's parser and returns only the segment list.
func ParseM3U8(r io.Reader) ([]string, error) {
	ts, err := defaultClient.ParseM3U8(r)
	return ts, err
}

// DownloadSegments is a convenience wrapper using default client.
func DownloadSegments(ctx context.Context, tsUrls []string, baseURL *url.URL, concurrency int) ([][]byte, error) {
	// ignore provided concurrency; keep compatibility by creating a temp client
	c := NewClient(WithConcurrency(concurrency))
	return c.DownloadSegments(ctx, tsUrls, baseURL, nil)
}

// CombineSegments wrapper
func CombineSegments(segments [][]byte, outputFileName string) error {
	return defaultClient.CombineSegments(segments, outputFileName)
}
