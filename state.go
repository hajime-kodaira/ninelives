package main

// Backoff state kept between runs. launchd fires us on a fixed interval and
// cannot be told to wait, so a 429 has to be remembered on disk: the next few
// runs skip the request entirely instead of hammering an endpoint that has
// already said no.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type state struct {
	// BackoffUntil is when the server said the window resets.
	BackoffUntil time.Time `json:"backoffUntil,omitempty"`
	// Strikes counts consecutive 429s, for status output only.
	Strikes int `json:"strikes,omitempty"`
}

// statePath keeps the state beside the metrics file. The leading dot keeps it
// out of the way of RunCat's file picker.
func statePath(out string) string {
	return filepath.Join(filepath.Dir(out), ".ninelives-state.json")
}

func loadState(out string) state {
	var s state
	data, err := os.ReadFile(statePath(out))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s) // a corrupt state file just means no backoff
	return s
}

func saveState(out string, s state) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	// Best effort: failing to record a backoff must not fail the run.
	_ = writeAtomic(statePath(out), append(data, '\n'))
}

func clearState(out string) {
	_ = os.Remove(statePath(out))
}

// waiting reports how long is left on a recorded backoff, if any.
func (s state) waiting(now time.Time) (time.Duration, bool) {
	if s.BackoffUntil.IsZero() || !now.Before(s.BackoffUntil) {
		return 0, false
	}
	return s.BackoffUntil.Sub(now).Round(time.Second), true
}

func (s state) describe(now time.Time) string {
	if d, ok := s.waiting(now); ok {
		return fmt.Sprintf("backing off for another %s (%d consecutive 429s)", d, s.Strikes)
	}
	return "none"
}
