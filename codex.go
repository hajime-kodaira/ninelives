package main

// Codex CLI usage. The numbers come from the same undocumented endpoints the
// Codex CLI polls for its rate-limit display: GET /wham/usage for the windows
// and GET /wham/rate-limit-reset-credits for the reset-credit count, both
// under https://chatgpt.com/backend-api. Authentication reuses the ChatGPT
// tokens Codex CLI already stores: the access token goes in Authorization,
// the account id in ChatGPT-Account-Id.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

// --- response shape -----------------------------------------------------

// codexWindow is one rate-limit window. used_percent is percent consumed —
// the same convention as Claude's utilization — and reset_at arrives as Unix
// seconds (resetTime also accepts an RFC3339 string, should the shape drift).
type codexWindow struct {
	UsedPercent        float64   `json:"used_percent"`
	LimitWindowSeconds int64     `json:"limit_window_seconds"`
	ResetsAt           resetTime `json:"reset_at"`
}

// codexLimits is one pair of windows. The CLI calls them primary (usually the
// 5-hour bucket) and secondary (usually the weekly one), but the lengths
// arrive as data and some accounts only ever see one window, so the labels
// are derived per window.
type codexLimits struct {
	Primary   *codexWindow `json:"primary_window"`
	Secondary *codexWindow `json:"secondary_window"`
}

// codexUsage is the wham/usage payload, trimmed to what the card shows. The
// rate_limit_reset_credits count in here disagrees with both the Codex app
// and the dedicated endpoint, so it is deliberately not decoded; the card
// reads the dedicated one instead.
type codexUsage struct {
	PlanType   string       `json:"plan_type"`
	RateLimit  *codexLimits `json:"rate_limit"`
	CodeReview *codexLimits `json:"code_review_rate_limit"`
}

// codexResets is the wham/rate-limit-reset-credits payload, trimmed to the
// count the card shows. This is the number the Codex app itself displays.
type codexResets struct {
	AvailableCount int `json:"available_count"`
}

// windowLabel names a window from its length: 18000s is the 5-hour bucket,
// 604800s the weekly one, and only those two are what -bar can single out.
// Any other length still gets a row, labelled by its duration, but never
// drives the bar.
func windowLabel(secs int64) (label, key string) {
	switch secs {
	case 5 * 60 * 60:
		return "5h", "5h"
	case 7 * 24 * 60 * 60:
		return "7d", "7d"
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d <= 0:
		return "window", ""
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m", ""
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h", ""
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d", ""
	}
}

// rows turns the windows into card rows. Unknown window lengths still get a
// row — they are the same kind of allowance, just not one -bar can name.
func (u codexUsage) rows() []row {
	var out []row
	add := func(l *codexLimits, prefix string) {
		if l == nil {
			return
		}
		for _, w := range []*codexWindow{l.Primary, l.Secondary} {
			if w == nil {
				continue
			}
			label, key := windowLabel(w.LimitWindowSeconds)
			if prefix != "" {
				key = "" // a "Review 5h" row must not hijack -bar 5h
			}
			out = append(out, row{
				label:  prefix + label,
				key:    key,
				used:   w.UsedPercent,
				resets: w.ResetsAt.Time,
			})
		}
	}
	add(u.RateLimit, "")
	add(u.CodeReview, "Review ")
	return out
}

// --- credentials -------------------------------------------------------

// codexAuth is ~/.codex/auth.json, trimmed to the two fields the endpoints
// need. Codex CLI refreshes this file itself; this tool only reads it.
type codexAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".codex")
}

// findCodexToken returns the ChatGPT access token and the account id the
// backend expects alongside it, plus where they came from, for errors.
func findCodexToken() (token, accountID, source string, err error) {
	path := filepath.Join(codexHome(), "auth.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", fmt.Errorf("no Codex credentials found (checked %s; `codex login` creates them)", path)
	}
	var a codexAuth
	if err := json.Unmarshal(blob, &a); err != nil {
		return "", "", "", fmt.Errorf("reading %s: %w", path, err)
	}
	if a.Tokens.AccessToken == "" {
		return "", "", "", fmt.Errorf("%s had no tokens.access_token", path)
	}
	return a.Tokens.AccessToken, a.Tokens.AccountID, path, nil
}

// --- fetching ----------------------------------------------------------

// codexGet performs one authenticated GET against the ChatGPT backend. Both
// endpoints take the same headers, and both can answer 429 with Retry-After,
// so the existing backoff machinery applies unchanged.
func codexGet(url, token, accountID string, timeout time.Duration) ([]byte, http.Header, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
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
		return nil, resp.Header, errors.New("401 from the codex endpoint: the ChatGPT token expired; start `codex` once to let it refresh")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, resp.Header, &rateLimitError{RetryAfter: retryAfter(resp.Header)}
	case resp.StatusCode != http.StatusOK:
		return nil, resp.Header, fmt.Errorf("codex endpoint returned %s: %s", resp.Status, snippet(body))
	}
	return body, resp.Header, nil
}

// fetchCodexResets fetches the reset-credit count. Unlike usage, a failure
// here only costs the Resets row, so callers treat the error as soft.
func fetchCodexResets(token, accountID string, timeout time.Duration) (*codexResets, error) {
	body, _, err := codexGet(codexResetCreditsURL, token, accountID, timeout)
	if err != nil {
		return nil, err
	}
	var r codexResets
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parsing rate-limit-reset-credits response: %w", err)
	}
	return &r, nil
}
