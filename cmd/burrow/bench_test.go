package main

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"10485760", 10485760},
		{"512B", 512},
		{"4kb", 4 << 10},
		{" 50MB ", 50 << 20},
		{"1GB", 1 << 30},
		{strconv.FormatInt(math.MaxInt64>>30, 10) + "GB", (math.MaxInt64 >> 30) << 30},
		{strconv.FormatInt(math.MaxInt64, 10), math.MaxInt64},
	}
	for _, test := range tests {
		got, err := parseSize(test.in)
		if err != nil {
			t.Fatalf("parseSize(%q) error: %v", test.in, err)
		}
		if got != test.want {
			t.Fatalf("parseSize(%q) = %d, want %d", test.in, got, test.want)
		}
	}
}

func TestParseSizeRejectsOverflow(t *testing.T) {
	for _, in := range []string{
		strconv.FormatInt(math.MaxInt64>>30+1, 10) + "GB",
		"9223372036854775807GB",
		"9223372036854775807KB",
		strconv.FormatInt(math.MaxInt64>>20+1, 10) + "MB",
	} {
		got, err := parseSize(in)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("parseSize(%q) = %d, %v; want a too-large error", in, got, err)
		}
	}
}

func TestParseSizeRejectsInvalid(t *testing.T) {
	for _, in := range []string{"", "MB", "0", "-1KB", "1.5GB", "abc"} {
		if got, err := parseSize(in); err == nil {
			t.Fatalf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}
