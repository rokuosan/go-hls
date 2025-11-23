package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rokuosan/go-hls/pkg/hls"
)

func main() {
	var (
		playlist    = flag.String("playlist", "", "URL to the .m3u8 playlist to download")
		out         = flag.String("out", "out.ts", "Output filename for combined segments")
		concurrency = flag.Int("concurrency", hls.DefaultConcurrency, "Number of parallel downloads")
		retry       = flag.Int("retry", -1, "Number of retries (negative to use client default)")
		backoffStr  = flag.String("backoff", "", "Base backoff duration (e.g. 200ms). Empty to use client default")
		jitter      = flag.Float64("jitter", -1.0, "Fractional jitter (0.1 means ±10%). Negative to use client default")
		timeoutStr  = flag.String("timeout", "0", "Overall timeout for download (e.g. 30s). 0 means no timeout")
		verbose     = flag.Bool("verbose", false, "Enable verbose logging")
	)
	flag.Parse()

	if *playlist == "" {
		fmt.Fprintln(os.Stderr, "-playlist is required")
		flag.Usage()
		os.Exit(2)
	}

	// create a logger depending on verbosity
	var logger *slog.Logger
	if *verbose {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	} else {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	// build options
	opts := []hls.Option{
		hls.WithConcurrency(*concurrency),
		hls.WithLogger(logger),
	}

	if *retry >= 0 {
		opts = append(opts, hls.WithRetry(*retry))
	}

	if *backoffStr != "" {
		if d, err := time.ParseDuration(*backoffStr); err == nil {
			opts = append(opts, hls.WithBackoff(d))
		} else {
			fmt.Fprintf(os.Stderr, "invalid backoff: %v\n", err)
			os.Exit(2)
		}
	}

	if *jitter >= 0 {
		opts = append(opts, hls.WithJitter(*jitter))
	}

	client := hls.NewClient(opts...)

	// context with optional timeout and signal handling
	var ctx context.Context
	var cancel context.CancelFunc
	if *timeoutStr != "0" {
		if d, err := time.ParseDuration(*timeoutStr); err == nil {
			ctx, cancel = context.WithTimeout(context.Background(), d)
		} else {
			fmt.Fprintf(os.Stderr, "invalid timeout: %v\n", err)
			os.Exit(2)
		}
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	// handle signals
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		logger.Info("received signal, cancelling")
		cancel()
	}()

	logger.Info("parsing playlist", "url", *playlist)
	tsUrls, base, err := client.ParseM3U8FromURL(*playlist)
	if err != nil {
		logger.Error("failed to parse playlist", "err", err)
		os.Exit(1)
	}

	logger.Info("found segments", "count", len(tsUrls))

	// progress display
	progressCh := make(chan int)
	total := len(tsUrls)
	go func() {
		done := 0
		for d := range progressCh {
			done += d
			// simple progress bar
			pct := float64(done) / float64(total)
			bars := int(pct * 40)
			fmt.Printf("\r[%s%s] %d/%d", repeat('#', bars), repeat('-', 40-bars), done, total)
			if done >= total {
				fmt.Println()
			}
		}
	}()

	segments, err := client.DownloadSegments(ctx, tsUrls, base, progressCh)
	close(progressCh)
	if err != nil {
		logger.Error("download failed", "err", err)
		os.Exit(1)
	}

	if err := client.CombineSegments(segments, *out); err != nil {
		logger.Error("combine failed", "err", err)
		os.Exit(1)
	}

	logger.Info("download complete", "out", *out)
}

// helper: repeat a rune N times and return as string
func repeat(ch rune, count int) string {
	if count <= 0 {
		return ""
	}
	runes := make([]rune, count)
	for i := 0; i < count; i++ {
		runes[i] = ch
	}
	return string(runes)
}
