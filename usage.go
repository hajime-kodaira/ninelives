package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	usageURL      = "https://api.anthropic.com/api/oauth/usage"
	anthropicBeta = "oauth-2025-04-20"
	anthropicVers = "2023-06-01"
	keychainSvc   = "Claude Code-credentials"
	userAgent     = "ninelives/1.0"
)

// --- response shape ----------------------------------------------------

type window struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    resetTime `json:"resets_at"`
}

// limit is one entry of the newer "limits" array. It carries the same numbers
// as five_hour/seven_day plus the per-model weekly limits, which the older
// seven_day_opus field reports as null.
type limit struct {
	Kind     string    `json:"kind"`
	Group    string    `json:"group"`
	Percent  float64   `json:"percent"`
	ResetsAt resetTime `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type usage struct {
	FiveHour     *window `json:"five_hour"`
	SevenDay     *window `json:"seven_day"`
	SevenDayOpus *window `json:"seven_day_opus"`
	Limits       []limit `json:"limits"`
}

// resetTime accepts either an RFC3339 string or a Unix timestamp, since the
// endpoint is undocumented and has changed shape before.
type resetTime struct{ time.Time }

func (r *resetTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		// An unparseable timestamp costs us the "resets in" suffix, not the run.
		if t, err := time.Parse(time.RFC3339, str); err == nil {
			r.Time = t
		}
		return nil
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		r.Time = time.Unix(int64(secs), 0)
	}
	return nil
}

// row is one line of the RunCat card.
type row struct {
	label  string
	used   float64 // percent of the allowance consumed
	resets time.Time
	key    string // "5h" or "7d" for the windows -bar can name; empty otherwise
}

// rows prefers the limits array and falls back to the flat fields, so the tool
// keeps working whichever shape the undocumented endpoint returns.
func (u usage) rows() []row {
	var out []row
	for _, l := range u.Limits {
		r := row{used: l.Percent, resets: l.ResetsAt.Time}
		switch l.Kind {
		case "session":
			r.label, r.key = "5h", "5h"
		case "weekly_all":
			r.label, r.key = "7d", "7d"
		case "weekly_scoped":
			r.label = "7d " + modelName(l)
		default:
			r.label = l.Kind
		}
		out = append(out, r)
	}
	if len(out) > 0 {
		return out
	}
	for _, w := range []struct {
		label, key string
		w          *window
	}{
		{"5h", "5h", u.FiveHour},
		{"7d", "7d", u.SevenDay},
		{"7d Opus", "", u.SevenDayOpus},
	} {
		if w.w == nil {
			continue
		}
		out = append(out, row{label: w.label, used: w.w.Utilization, resets: w.w.ResetsAt.Time, key: w.key})
	}
	return out
}

func modelName(l limit) string {
	if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
		return l.Scope.Model.DisplayName
	}
	return "scoped"
}

// --- fetching ----------------------------------------------------------

func fetchUsage(token string, timeout time.Duration) ([]byte, http.Header, error) {
	req, err := http.NewRequest(http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicBeta)
	req.Header.Set("anthropic-version", anthropicVers)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.Header, err
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, resp.Header, errors.New("401 from the usage endpoint: the OAuth token expired; start `claude` once to let it refresh")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, resp.Header, &rateLimitError{RetryAfter: retryAfter(resp.Header)}
	case resp.StatusCode != http.StatusOK:
		return nil, resp.Header, fmt.Errorf("usage endpoint returned %s: %s", resp.Status, snippet(body))
	}
	return body, resp.Header, nil
}

// rateLimitError is a 429. The endpoint sends no rate-limit headers on a
// success, but a 429 does carry Retry-After, which is exactly when the window
// resets — so it is worth threading through rather than flattening to text.
type rateLimitError struct{ RetryAfter time.Duration }

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("429 from the usage endpoint: the window resets in %s", e.RetryAfter)
}

// defaultRetryAfter is used when a 429 arrives without the header. The observed
// window is 5 minutes, so waiting that long is the safe assumption.
const defaultRetryAfter = 5 * time.Minute

func retryAfter(h http.Header) time.Duration {
	if h != nil {
		if secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultRetryAfter
}

// responseHeaders renders the response headers for -raw. Rate-limit headers in
// particular are the only way to tell how often this endpoint can be polled.
func responseHeaders(h http.Header) []string {
	var out []string
	for name, vals := range h {
		out = append(out, name+": "+strings.Join(vals, ", "))
	}
	sort.Strings(out)
	return out
}

// --- credentials -------------------------------------------------------

type credentials struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// findToken returns the OAuth access token and a short description of where it
// came from, for error messages.
func findToken() (token, source string, err error) {
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			return v, "$" + name, nil
		}
	}
	if runtime.GOOS == "darwin" {
		if blob, err := keychainBlob(); err == nil {
			if tok, err := tokenFromBlob(blob); err == nil {
				return tok, "Keychain", nil
			}
		}
	}
	path := filepath.Join(homeDir(), ".claude", ".credentials.json")
	blob, ferr := os.ReadFile(path)
	if ferr != nil {
		return "", "", fmt.Errorf("no Claude Code credentials found (checked $CLAUDE_CODE_OAUTH_TOKEN, the macOS Keychain item %q and %s)", keychainSvc, path)
	}
	tok, err := tokenFromBlob(blob)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", path, err)
	}
	return tok, path, nil
}

func keychainBlob() ([]byte, error) {
	return exec.Command("security", "find-generic-password", "-s", keychainSvc, "-w").Output()
}

func tokenFromBlob(blob []byte) (string, error) {
	var c credentials
	if err := json.Unmarshal(blob, &c); err != nil {
		return "", fmt.Errorf("credentials were not the expected JSON: %w", err)
	}
	if c.ClaudeAiOauth.AccessToken == "" {
		return "", errors.New("credentials had no claudeAiOauth.accessToken")
	}
	return c.ClaudeAiOauth.AccessToken, nil
}
