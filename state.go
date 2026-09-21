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
	// Seen records the allowance windows the API has reported, so a newly
	// appearing one can be announced once instead of every single run.
	Seen []string `json:"seen,omitempty"`
	// AgentVersion is the version that wrote the current plist. Replacing the
	// binary is enough to update, but a plist written by an older version can
	// be missing keys a newer one expects, and nothing else would say so.
	AgentVersion string `json:"agentVersion,omitempty"`
	// ResetCreditsNote records the last reset-credit sidecar failure, so a
	// permanent one (a plan without reset credits answers the same 404
	// forever) is announced once instead of once per run.
	ResetCreditsNote string `json:"resetCreditsNote,omitempty"`
}

func (s *state) clearBackoff() {
	s.BackoffUntil = time.Time{}
	s.Strikes = 0
}

// noteWindows records the current window names and returns the ones that were
// not there before.
func (s *state) noteWindows(names []string) []string {
	was := make(map[string]bool, len(s.Seen))
	for _, n := range s.Seen {
		was[n] = true
	}
	var added []string
	for _, n := range names {
		if !was[n] {
			added = append(added, n)
		}
	}
	s.Seen = names
	return added
}

// statePath keeps the state beside the metrics file. The leading dot keeps it
// out of the way of RunCat's file picker. The name is keyed by provider, not
// by the -out file: the claude card keeps its historical name whatever -out
// says (so an existing install pointed at a custom file keeps its backoff
// record), and the codex card gets one of its own, so two cards sharing a
// directory do not fight over a single backoff.
func statePath(o options) string {
	name := ".ninelives-state.json"
	if o.provider == "codex" {
		name = ".ninelives-codex-state.json"
	}
	return filepath.Join(filepath.Dir(o.out), name)
}

func loadState(o options) state {
	var s state
	data, err := os.ReadFile(statePath(o))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s) // a corrupt state file just means no backoff
	return s
}

func saveState(o options, s state) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	// Best effort: failing to record a backoff must not fail the run.
	_ = writeAtomic(statePath(o), append(data, '\n'))
}

func clearState(o options) {
	_ = os.Remove(statePath(o))
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
