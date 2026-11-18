package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// benchPath is the HTTP path shared by the bench client and server.
const benchPath = "/bench"

// parseSize parses a human-readable size such as "50MB", "1GB", "1024KB" or
// "10485760" and returns the number of bytes. A value without a unit is
// interpreted as bytes.
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "GB"):
		mult, t = 1<<30, strings.TrimSuffix(t, "GB")
	case strings.HasSuffix(t, "MB"):
		mult, t = 1<<20, strings.TrimSuffix(t, "MB")
	case strings.HasSuffix(t, "KB"):
		mult, t = 1<<10, strings.TrimSuffix(t, "KB")
	case strings.HasSuffix(t, "B"):
		t = strings.TrimSuffix(t, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse size %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive: %q", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size is too large: %q", s)
	}
	return n * mult, nil
}

// formatBytes formats a byte count in human-readable form.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// formatRate formats a transfer rate in both MB/s and Mbps.
func formatRate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "n/a"
	}
	mbps := float64(bytes) * 8 / d.Seconds() / 1e6
	mBps := float64(bytes) / d.Seconds() / (1 << 20)
	return fmt.Sprintf("%.2f MB/s (%.1f Mbps)", mBps, mbps)
}

// randReader produces a fixed amount of pseudo-random data for benchmarks.
// It generates data on the fly, so memory usage stays constant.
type randReader struct {
	remaining int64
	seed      uint64
	onRead    func(n int) // called after every read; used for progress
}

// newPayload returns an io.Reader that yields exactly size bytes. onRead may
// be nil; otherwise it is called once per chunk.
func newPayload(size int64, onRead func(n int)) io.Reader {
	return &randReader{
		remaining: size,
		seed:      0x9E3779B97F4A7C15,
		onRead:    onRead,
	}
}

func (r *randReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	// xorshift keeps the data incompressible so link-level compression
	// cannot skew the measurement.
	for i := range n {
		r.seed ^= r.seed << 13
		r.seed ^= r.seed >> 7
		r.seed ^= r.seed << 17
		p[i] = byte(r.seed)
	}
	r.remaining -= int64(n)
	if r.onRead != nil {
		r.onRead(n)
	}
	return n, nil
}

// countingSink discards everything written to it and counts the bytes.
type countingSink struct {
	total atomic.Int64
}

func (c *countingSink) Write(p []byte) (int, error) {
	c.total.Add(int64(len(p)))
	return len(p), nil
}

// progress periodically prints transfer progress until Stop is called.
type progress struct {
	label  string
	total  int64
	output io.Writer
	done   atomic.Int64
	start  time.Time
	stop   chan struct{}
}

// newProgress starts a reporter that writes one line per second to output.
func newProgress(output io.Writer, label string, total int64) *progress {
	if output == nil {
		output = io.Discard
	}
	p := &progress{label: label, total: total, output: output, start: time.Now(), stop: make(chan struct{})}
	go p.loop()
	return p
}

func (p *progress) Add(n int) { p.done.Add(int64(n)) }

func (p *progress) loop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			done := p.done.Load()
			elapsed := time.Since(p.start)
			pct := ""
			if p.total > 0 {
				pct = fmt.Sprintf(" (%.1f%%)", float64(done)*100/float64(p.total))
			}
			fmt.Fprintf(p.output, "[bench] %s: %s%s, %s\n",
				p.label, formatBytes(done), pct, formatRate(done, elapsed))
		}
	}
}

// Stop stops the reporter and returns the bytes transferred and elapsed time.
func (p *progress) Stop() (int64, time.Duration) {
	close(p.stop)
	return p.done.Load(), time.Since(p.start)
}
