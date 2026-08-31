package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	for _, c := range []struct {
		header string
		want   time.Duration
	}{
		{"300", 300 * time.Second},
		{" 42 ", 42 * time.Second},
		{"", defaultRetryAfter},
		{"tomorrow", defaultRetryAfter},
		{"0", defaultRetryAfter},
	} {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		if got := retryAfter(h); got != c.want {
			t.Errorf("Retry-After %q => %v, want %v", c.header, got, c.want)
		}
	}
	if got := retryAfter(nil); got != defaultRetryAfter {
		t.Errorf("nil header => %v", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	out := filepath.Join(t.TempDir(), "claude.json")
	now := time.Now()

	if _, ok := loadState(out).waiting(now); ok {
		t.Error("a missing state file should not report a backoff")
	}

	saveState(out, state{BackoffUntil: now.Add(90 * time.Second), Strikes: 2})
	d, ok := loadState(out).waiting(now)
	if !ok || d < 89*time.Second || d > 90*time.Second {
		t.Errorf("waiting = %v, %v; want ~90s, true", d, ok)
	}

	// An elapsed backoff is over, not merely shorter.
	if _, ok := loadState(out).waiting(now.Add(2 * time.Minute)); ok {
		t.Error("an elapsed backoff should not still be waiting")
	}

	clearState(out)
	if _, err := os.Stat(statePath(out)); !os.IsNotExist(err) {
		t.Errorf("clearState left %s behind", statePath(out))
	}
}

// A corrupt state file must not wedge the tool into a permanent backoff.
func TestCorruptStateIsIgnored(t *testing.T) {
	out := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(statePath(out), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadState(out).waiting(time.Now()); ok {
		t.Error("a corrupt state file should not report a backoff")
	}
}

// While backed off, writeMetrics must return without touching the network or
// the metrics file. The unreachable timeout proves no request was attempted.
func TestWriteMetricsSkipsWhileBackedOff(t *testing.T) {
	out := filepath.Join(t.TempDir(), "claude.json")
	saveState(out, state{BackoffUntil: time.Now().Add(time.Hour), Strikes: 1})

	if err := writeMetrics(options{out: out, timeout: time.Nanosecond}); err != nil {
		t.Fatalf("a backed-off run should succeed quietly, got %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("a backed-off run must not write the metrics file")
	}
}

func TestRateLimitErrorMessage(t *testing.T) {
	err := &rateLimitError{RetryAfter: 300 * time.Second}
	if got := err.Error(); got != "429 from the usage endpoint: the window resets in 5m0s" {
		t.Errorf("Error() = %q", got)
	}
}
