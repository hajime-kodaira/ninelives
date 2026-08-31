package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

	// Decoded separately from raw, never as part of the main struct: it is a
	// cosmetic extra, and a surprise in its shape must not cost us the card.
	// used_credits arrives as 735.0, which an int field would reject outright.
	ExtraUsage *extraUsage `json:"-"`

	// raw keeps every top-level key so unrecognised allowances can still be
	// found. The account carries a row of codenamed nulls — omelette_promotional,
	// amber_ladder, tangelo and friends — which is where a temporary grant would
	// plausibly show up, and nimbus_quill is already non-null. Rather than guess
	// at each name, anything window-shaped that is not accounted for is reported.
	raw map[string]json.RawMessage
}

// knownKeys are the top-level fields the card already understands. Everything
// else is a candidate allowance.
var knownKeys = map[string]bool{
	"five_hour": true, "seven_day": true, "seven_day_opus": true,
	"limits": true, "extra_usage": true, "spend": true,
	"member_dashboard_available": true,
}

func (u *usage) UnmarshalJSON(b []byte) error {
	type plain usage // avoid recursing into this method
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*u = usage(p)
	if err := json.Unmarshal(b, &u.raw); err != nil {
		return err
	}
	if body, ok := u.raw["extra_usage"]; ok {
		var e extraUsage
		if err := json.Unmarshal(body, &e); err == nil {
			u.ExtraUsage = &e
		}
	}
	return nil
}

// unknownWindows returns the window-shaped fields the card does not show. A new
// allowance appearing under a name nobody has seen yet lands here instead of
// being silently dropped.
func (u usage) unknownWindows() []row {
	names := make([]string, 0, len(u.raw))
	for name := range u.raw {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []row
	for _, name := range names {
		if knownKeys[name] {
			continue
		}
		body := bytes.TrimSpace(u.raw[name])
		if len(body) == 0 || body[0] != '{' { // null, or not an object
			continue
		}
		var w window
		if err := json.Unmarshal(body, &w); err != nil {
			continue
		}
		if !bytes.Contains(body, []byte(`"utilization"`)) {
			continue // some other kind of object, not an allowance
		}
		out = append(out, row{label: name, used: w.Utilization, resets: w.ResetsAt.Time})
	}
	return out
}

// extraUsage is the pay-as-you-go spend that covers you past the plan limits.
// It is money rather than a percentage, so it never gets a bar.
type extraUsage struct {
	IsEnabled         bool    `json:"is_enabled"`
	Currency          string  `json:"currency"`
	DecimalPlaces     float64 `json:"decimal_places"`
	UsedCredits       float64 `json:"used_credits"`
	SpendLimitReached bool    `json:"spend_limit_reached"`
}

// spent renders used_credits as an amount, e.g. 735 with 2 decimal places and
// USD becomes "$7.35".
func (e extraUsage) spent() string {
	places := int(e.DecimalPlaces)
	if places < 0 || places > 6 {
		places = 2
	}
	amount := e.UsedCredits / math.Pow(10, float64(places))
	symbol := map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥"}[e.Currency]
	if symbol == "" {
		symbol = e.Currency + " "
	}
	return fmt.Sprintf("%s%.*f", symbol, places, amount)
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
	req.Header.Set("User-Agent", "ninelives/"+versionString())
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
