package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxLives = 9

// metric is one row of a RunCat Neo custom metrics card. A row with no
// normalizedValue renders as text without a bar.
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

func buildCard(u usage, o options, now time.Time) card {
	c := card{
		Title:           o.title,
		Symbol:          o.symbol,
		LastUpdatedDate: now.UTC().Format(time.RFC3339),
	}
	rows := u.rows()
	if o.extra {
		rows = append(rows, u.unknownWindows()...)
	}
	for _, r := range rows {
		left := clamp(100-r.used, 0, 100)
		norm := left / 100
		c.Metrics = append(c.Metrics, metric{
			Title:           r.label,
			FormattedValue:  shortValue(left, o.lives) + " left" + until(now, r.resets),
			NormalizedValue: &norm,
		})
	}
	if o.credits && u.ExtraUsage != nil && u.ExtraUsage.IsEnabled {
		// Money, not a percentage: no normalizedValue means no bar.
		c.Metrics = append(c.Metrics, metric{
			Title:          "Credits",
			FormattedValue: u.ExtraUsage.spent() + " used",
		})
	}
	// The menu bar only ever reflects the plan windows; an unknown allowance
	// sitting at 0% must not make the bar look full.
	c.MetricsBarValue = shortValue(barValue(u.rows(), o.bar), o.lives)
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
