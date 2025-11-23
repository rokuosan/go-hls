# go-hls

go-hls is a small, dependency-light HLS (HTTP Live Streaming) helper and
command-line downloader written in Go. It provides a reusable `pkg/hls` library
for parsing M3U8 playlists, downloading `.ts` segments in parallel with
configurable retries/backoff, and combining segments into a single file. A
minimal CLI is included under `cmd/client` for simple downloads.

## Features

- Parse M3U8 playlists and resolve relative segment URLs
- Parallel segment downloading with concurrency limit
- Retry with exponential backoff and optional jitter
- Structured logging via `slog` (injectable logger)
- Simple CLI for downloading and combining segments

## Run Example

Prerequisites: Go toolchain (see `go.mod` for tested Go version).

Clone and build:

```bash
git clone https://github.com/rokuosan/go-hls-client.git
```

Run the CLI (example):

```bash
# Download a playlist and produce out.ts
go run ./cmd/client -playlist "https://example.com/playlist.m3u8" -out out.ts
```

CLI flags include `-playlist`, `-out`, `-concurrency`, `-retry`, `-backoff`,
`-jitter`, `-timeout`, and `-verbose`. See `cmd/client/main.go` for details.

## Library usage (`pkg/hls`)

The `pkg/hls` package provides a `Client` type configured via functional
options. Typical usage:

```go
client := hls.NewClient(
    hls.WithConcurrency(4),
    hls.WithRetry(3),
)

ts, base, err := client.ParseM3U8FromURL("https://example.com/playlist.m3u8")
// handle err

progress := make(chan int)
ctx := context.Background()
segments, err := client.DownloadSegments(ctx, ts, base, progress)
// handle err

if err := client.CombineSegments(segments, "out.ts"); err != nil {
    // handle err
}
```

See `pkg/hls/README.md` for more implementation details and examples.

## Contributing

Contributions are welcome. For small fixes or documentation updates, open a
pull request. If you plan to add larger changes (streaming-to-disk support,
advanced retry policies, richer progress events), open an issue first to
discuss design and backward compatibility.

## License

This project is licensed under the MIT License. See the `LICENSE` file for details.

## Notes

- Current implementation keeps segments in memory; consider streaming to
  temporary files for large playlists.
- `pkg/hls/README.md` contains focused API notes and examples for library
  users.
