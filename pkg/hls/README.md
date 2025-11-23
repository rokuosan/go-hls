# pkg/hls

Lightweight HLS helper for parsing playlists, downloading and combining `.ts` segments.

This package provides a small, configurable `Client` and a few convenience wrappers
for common HLS tasks. The parser is reader-based (`io.Reader`) to make testing
and composition easy; a convenience method on `Client` opens `http`/`file://`
URLs and local filesystem paths when you want a one-shot call.

## Quick API

Create a client with `NewClient(opts ...Option)`. Common options:

- `WithHTTPClient(h *http.Client)`
- `WithConcurrency(n int)` — default `DefaultConcurrency` (8).
- `WithLogger(l *slog.Logger)` — pass a logger or discard logs in tests.
- `WithRetry(r int)` — number of retry attempts (default 2).
- `WithBackoff(d time.Duration)` — base backoff (default 200ms).
- `WithJitter(j float64)` — fractional jitter (default 0.1 = ±10%).

### Main functions / methods

- `ParseM3U8(r io.Reader) ([]string, error)`

    Parse an M3U8 playlist from an `io.Reader` and return the listed `.ts`
    entries in order. The reader is not closed by the function — callers should
    close it when appropriate. A package-level thin wrapper `ParseM3U8` uses the
    default client.

- `(c *Client) ParseM3U8FromURL(m3u8URL string) ([]string, *url.URL, error)`

    Convenience method that accepts an `http`/`https` URL, a `file://` URL,
    or a local filesystem path. It opens the source using the client's HTTP
    settings (for remote URLs) or `os.Open` (for files/paths), parses the
    playlist, and returns a `base *url.URL` suitable for resolving relative
    segment references.

- `(c *Client) DownloadSegments(ctx, tsUrls []string, baseURL *url.URL, progress chan<- int) ([][]byte, error)`

    Downloads `tsUrls` in parallel and returns a `[][]byte` with each segment
    in the original order. If `progress` is non-nil, the function sends `1`
    on the channel for each completed segment. The function does not close the
    progress channel.

- `(c *Client) CombineSegments(segments [][]byte, out string) error`

    Write ordered segments to `out`. Write/close errors are returned.

There are also package-level convenience wrappers that use a default client:
`DownloadSegments` and `CombineSegments`.

## Error behavior

The package returns IO and close errors to callers (instead of logging-only).
Always check returned errors — a failed `Close()` or a failed drain may be
reported as an error.

## Examples

Parse from a remote playlist (convenience):

```go
logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
client := hls.NewClient(hls.WithConcurrency(4), hls.WithLogger(logger))

ts, base, err := client.ParseM3U8FromURL("https://example.com/playlist.m3u8")
if err != nil {
    // handle error
}

segments, err := client.DownloadSegments(context.Background(), ts, base, nil)
if err != nil {
    // handle error
}

if err := client.CombineSegments(segments, "out.ts"); err != nil {
    // handle error
}
```

Reader-based parsing (good for tests or custom sources):

```go
r := strings.NewReader("#EXTM3U\nseg1.ts\nseg2.ts\n")
ts, err := client.ParseM3U8(r)
if err != nil {
    // handle
}
```

## Tests

Run `go test ./pkg/hls`. Tests live in `pkg/hls/hls_test.go`.

## Caveats

- Current implementation keeps all segments in memory. For large streams you
  may want to stream segments to temporary files instead.
- The `ParseM3U8FromURL` helper accepts `file://<path>` and local paths; it
  normalizes paths using `filepath` but does not attempt to fetch remote
  `file://` hosts (only `localhost`/empty host are accepted).
- Check `go.mod` for the required Go toolchain version; CI pins that version to
  avoid `go mod` differences.

If you want streaming-to-disk, richer progress events, or different retry
policies I can add them — tell me which feature to prioritize.
