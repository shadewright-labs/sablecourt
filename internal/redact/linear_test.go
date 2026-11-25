package redact_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/redact"
	"github.com/shadewright-labs/sablecourt/internal/redact/redacttest"
)

// TestTextTimeGrowsLinearly feeds inputs built to defeat a backtracking or
// rescanning redactor and checks that four times the input costs well under
// sixteen times the time.
func TestTextTimeGrowsLinearly(t *testing.T) {
	var corpus strings.Builder
	for _, tc := range redacttest.Corpus() {
		corpus.WriteString(tc.Text)
	}
	for _, tc := range []struct{ name, unit string }{
		{"unterminated key headers", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n"},
		{"key headers with stray footers", "-----BEGIN EC PRIVATE KEY-----\n-----END RSA PRIVATE KEY-----\n"},
		{"open JSON secret fields", `{"password":"\\` + `"`},
		{"assignment labels without values", "password= api_key: secret="},
		{"bearer headers without tokens", "Authorization: Bearer \n"},
		{"token prefixes", "sk- ghp_ AIza sc_task_ op_ "},
		{"corpus", corpus.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ratio := growthRatio(func(n int) {
				redact.Text(strings.Repeat(tc.unit, n/len(tc.unit)+1))
			}, 32*1024, 128*1024)
			if ratio > 10 {
				t.Fatalf("4x input took %.1fx the time", ratio)
			}
		})
	}
}

func growthRatio(run func(int), small, large int) float64 {
	measure := func(size int) time.Duration {
		samples := make([]time.Duration, 7)
		for i := range samples {
			start := time.Now()
			run(size)
			samples[i] = time.Since(start)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[len(samples)/2]
	}
	s, l := measure(small), measure(large)
	if s <= 0 {
		s = time.Nanosecond
	}
	return float64(l) / float64(s)
}
