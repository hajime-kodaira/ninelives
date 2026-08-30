// ninelives reports how much of your Claude usage allowance is left and writes
// it as a RunCat Neo custom metrics card.
//
// Data source: GET https://api.anthropic.com/api/oauth/usage — the endpoint
// Claude Code's /usage command calls. It is undocumented and may change without
// notice. Authentication reuses the OAuth token Claude Code already stores
// (macOS Keychain "Claude Code-credentials", or ~/.claude/.credentials.json).
//
// Limits are shared per account, so these numbers match what Claude Desktop
// shows under Settings > Usage.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	maxLives      = 9
)

func main() {
	var (
		out     = flag.String("out", defaultOut(), "path to the metrics JSON RunCat Neo reads")
		title   = flag.String("title", "Claude", "card title")
		symbol  = flag.String("symbol", "staroflife", "SF Symbol name for the card")
		bar     = flag.String("bar", "5h", "which window drives the menu bar value: 5h, 7d or min")
		lives   = flag.Bool("lives", false, `show "6/9" instead of "65%"`)
		raw     = flag.Bool("raw", false, "print the raw API response to stdout and exit")
		stdout  = flag.Bool("stdout", false, "print the card to stdout instead of writing -out")
		timeout = flag.Duration("timeout", 15*time.Second, "HTTP timeout")
	)
	flag.Parse()

	if err := run(*out, *title, *symbol, *bar, *lives, *raw, *stdout, *timeout); err != nil {
		// Deliberately leave the existing file untouched on failure: RunCat keeps
		// showing the last good numbers with a stale "N minutes ago", which makes
		// the breakage visible instead of silently zeroing the bars.
		fmt.Fprintln(os.Stderr, "ninelives: "+err.Error())
		os.Exit(1)
	}
}

func run(out, title, symbol, bar string, lives, raw, toStdout bool, timeout time.Duration) error {
	token, src, err := findToken()
	if err != nil {
		return err
	}

	body, err := fetchUsage(token, timeout)
	if err != nil {
		return fmt.Errorf("%w (token from %s)", err, src)
	}
	if raw {
		os.Stdout.Write(pretty(body))
		return nil
	}

	var u usage
	if err := json.Unmarshal(body, &u); err != nil {
		return fmt.Errorf("parsing usage response: %w", err)
	}
	if len(u.rows()) == 0 {
		return errors.New("usage response carried no limit windows (run with -raw to inspect)")
	}

	c := buildCard(u, title, symbol, bar, lives, time.Now())
	enc, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	enc = append(enc, '\n')

	if toStdout {
		_, err := os.Stdout.Write(enc)
		return err
	}
	return writeAtomic(out, enc)
}

// --- API ---------------------------------------------------------------

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

func fetchUsage(token string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicBeta)
	req.Header.Set("anthropic-version", anthropicVers)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("401 from the usage endpoint: the OAuth token expired; start `claude` once to let it refresh")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("429 from the usage endpoint: polling too often; keep the interval at 5 minutes or more")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("usage endpoint returned %s: %s", resp.Status, snippet(body))
	}
	return body, nil
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
	outBytes, err := exec.Command("security", "find-generic-password", "-s", keychainSvc, "-w").Output()
	if err != nil {
		return nil, err
	}
	return outBytes, nil
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

// --- RunCat Neo card ---------------------------------------------------

type metric struct {
	Title           string   `json:"title"`
	FormattedValue  string   `json:"formattedValue"`
	NormalizedValue *float64 `json:"normalizedValue,omitempty"`
}

type card struct {
	Title           string   `json:"title"`
	Symbol          string   `json:"symbol"`
	MetricsBarValue string   `json:"metricsBarValue"`
	Metrics         []metric `json:"metrics"`
	LastUpdatedDate string   `json:"lastUpdatedDate"`
}

func buildCard(u usage, title, symbol, bar string, lives bool, now time.Time) card {
	c := card{
		Title:           title,
		Symbol:          symbol,
		LastUpdatedDate: now.UTC().Format(time.RFC3339),
	}
	rows := u.rows()
	for _, r := range rows {
		left := clamp(100-r.used, 0, 100)
		norm := left / 100
		c.Metrics = append(c.Metrics, metric{
			Title:           r.label,
			FormattedValue:  shortValue(left, lives) + " left" + until(now, r.resets),
			NormalizedValue: &norm,
		})
	}
	c.MetricsBarValue = shortValue(barValue(rows, bar), lives)
	return c
}

// barValue picks the remaining percentage shown in the menu bar itself.
func barValue(rows []row, bar string) float64 {
	if len(rows) == 0 {
		return 0
	}
	if bar == "min" {
		worst := math.Inf(1)
		for _, r := range rows {
			worst = math.Min(worst, clamp(100-r.used, 0, 100))
		}
		return worst
	}
	for _, r := range rows {
		if r.key == bar {
			return clamp(100-r.used, 0, 100)
		}
	}
	return clamp(100-rows[0].used, 0, 100)
}

// shortValue renders a remaining percentage as "65%" or, with -lives, "6/9".
func shortValue(remaining float64, lives bool) string {
	if !lives {
		return strconv.Itoa(int(math.Round(remaining))) + "%"
	}
	return strconv.Itoa(livesLeft(remaining)) + "/" + strconv.Itoa(maxLives)
}

// livesLeft maps a percentage onto the cat's nine lives. Anything above zero
// keeps at least one life, so 0/9 means genuinely used up.
func livesLeft(remaining float64) int {
	if remaining <= 0 {
		return 0
	}
	n := int(math.Ceil(remaining / 100 * maxLives))
	if n < 1 {
		return 1
	}
	if n > maxLives {
		return maxLives
	}
	return n
}

// until renders " · 2h13m" for the time left before a window resets.
func until(now, resets time.Time) string {
	if resets.IsZero() {
		return ""
	}
	d := resets.Sub(now)
	if d <= 0 {
		return " · resetting"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf(" · %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf(" · %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf(" · %dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// --- helpers -----------------------------------------------------------

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Same directory, so the rename is atomic and RunCat never observes a
	// half-written file.
	tmp, err := os.CreateTemp(dir, ".ninelives-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func defaultOut() string {
	return filepath.Join(homeDir(), ".config", "runcat-neo-metrics", "claude.json")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(math.Max(v, lo), hi)
}

func pretty(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return append(b, '\n')
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return append(b, '\n')
	}
	return append(out, '\n')
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
