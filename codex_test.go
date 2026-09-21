package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// The payload the live endpoint returned on a Plus plan, trimmed to what we
// read. reset_at arrives as Unix seconds; the window lengths arrive as data.
const sampleCodexPayload = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window":   {"used_percent": 3,  "limit_window_seconds": 18000,  "reset_after_seconds": 10558,  "reset_at": 1789671690},
    "secondary_window": {"used_percent": 1,  "limit_window_seconds": 604800, "reset_after_seconds": 597358, "reset_at": 1790258490}
  },
  "code_review_rate_limit": null,
  "credits": {"has_credits": false, "unlimited": false, "balance": "0"},
  "rate_limit_reset_credits": {"available_count": 3, "applicable_available_count": 0}
}`

// The dedicated reset-credits payload. Its available_count is what the Codex
// app displays, and it disagrees with the count embedded in wham/usage above
// (3), which is why the card only ever reads this one.
const sampleCodexResets = `{
  "credits": [
    {
      "reset_type": "codex_rate_limits",
      "status": "available",
      "granted_at": "2026-09-04T21:48:05Z",
      "expires_at": "2026-10-04T21:48:05Z",
      "title": "Full reset (Weekly + 5 hr)"
    }
  ],
  "available_count": 1,
  "total_earned_count": 0
}`

func parseCodexSample(t *testing.T) codexUsage {
	t.Helper()
	var u codexUsage
	if err := json.Unmarshal([]byte(sampleCodexPayload), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// sampleCodexNow is reset_at minus reset_after_seconds, so the sample payload
// says "2h55m" and "6d22h" for the same reason the live one did.
func sampleCodexNow() time.Time { return time.Unix(1789661132, 0).UTC() }

func TestCodexRowsFromSample(t *testing.T) {
	rows := parseCodexSample(t).rows()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	for i, want := range []struct {
		label, key string
		used       float64
		resets     int64
	}{
		{"5h", "5h", 3, 1789671690},
		{"7d", "7d", 1, 1790258490},
	} {
		if rows[i].label != want.label || rows[i].key != want.key || rows[i].used != want.used {
			t.Errorf("row %d = %+v, want %s/%s/%v", i, rows[i], want.label, want.key, want.used)
		}
		if !rows[i].resets.Equal(time.Unix(want.resets, 0)) {
			t.Errorf("row %d resets = %v, want %v", i, rows[i].resets, time.Unix(want.resets, 0))
		}
	}
}

// Some Plus accounts only ever see the weekly window; a single row must still
// render, and -bar must fall back to it instead of finding nothing.
func TestCodexWeeklyOnlyAccount(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"used_percent":31,"limit_window_seconds":604800,"reset_at":100}}}`
	var u codexUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	rows := u.rows()
	if len(rows) != 1 || rows[0].label != "7d" || rows[0].key != "7d" {
		t.Fatalf("rows = %+v, want a single 7d row", rows)
	}
	if got := barValue(rows, "5h"); got != 69 {
		t.Errorf("-bar 5h with no 5h window = %v, want the 69 of the weekly window", got)
	}
}

func TestCodexWindowLabelFallback(t *testing.T) {
	for _, c := range []struct {
		secs       int64
		label, key string
	}{
		{18000, "5h", "5h"},
		{604800, "7d", "7d"},
		{3600, "1h", ""},
		{172800, "2d", ""},
		{2592000, "30d", ""},
		{2700, "45m", ""},
		{0, "window", ""},
	} {
		if label, key := windowLabel(c.secs); label != c.label || key != c.key {
			t.Errorf("windowLabel(%d) = %q/%q, want %q/%q", c.secs, label, key, c.label, c.key)
		}
	}
}

// The code-review limit is the same shape under another name. Its rows carry
// a prefix, and must not hijack -bar even when their window length matches.
func TestCodexReviewLimitRows(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":100}},
	          "code_review_rate_limit":{"primary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_at":200}}}`
	var u codexUsage
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	rows := u.rows()
	if len(rows) != 2 || rows[1].label != "Review 7d" || rows[1].key != "" {
		t.Fatalf("rows = %+v, want a plain and a prefixed review row", rows)
	}
	if got := barValue(rows, "7d"); got != 90 {
		t.Errorf("-bar 7d = %v, want 90 from the plain weekly window", got)
	}
}

func TestBuildCodexCard(t *testing.T) {
	u := parseCodexSample(t)
	now := sampleCodexNow()

	var resets codexResets
	if err := json.Unmarshal([]byte(sampleCodexResets), &resets); err != nil {
		t.Fatal(err)
	}
	if resets.AvailableCount != 1 {
		t.Fatalf("available_count = %d, want 1 (the app's number)", resets.AvailableCount)
	}

	o := options{provider: "codex", title: "Codex", symbol: "bolt", bar: "5h"}
	c := buildCodexCard(u, &resets, o, now)
	if c.Title != "Codex" || c.Symbol != "bolt" {
		t.Errorf("card identity = %q/%q", c.Title, c.Symbol)
	}
	if c.MetricsBarValue != "97%" {
		t.Errorf("bar = %q, want 97%%", c.MetricsBarValue)
	}
	want := []struct{ title, formatted string }{
		{"5h", "97% left · 2h55m"},
		{"7d", "99% left · 6d21h"},
		{"Resets", "1 available"},
	}
	if len(c.Metrics) != len(want) {
		t.Fatalf("got %d rows, want %d", len(c.Metrics), len(want))
	}
	for i, w := range want {
		if c.Metrics[i].Title != w.title || c.Metrics[i].FormattedValue != w.formatted {
			t.Errorf("row %d = %q/%q, want %q/%q", i,
				c.Metrics[i].Title, c.Metrics[i].FormattedValue, w.title, w.formatted)
		}
	}
	for i, m := range c.Metrics {
		bar := m.NormalizedValue != nil
		if wantBar := i < 2; bar != wantBar {
			t.Errorf("row %d hasBar=%v, want %v (Resets is a count, not a percentage)", i, bar, wantBar)
		}
	}

	// A failed sidecar fetch drops the row without costing the card.
	dropped := buildCodexCard(u, nil, o, now)
	if len(dropped.Metrics) != 2 {
		t.Errorf("a nil resets left %d rows, want 2 (just the windows)", len(dropped.Metrics))
	}

	// -lives works on the codex bar exactly as on the claude one.
	lives := buildCodexCard(u, &resets, options{provider: "codex", bar: "5h", lives: true}, now)
	if lives.MetricsBarValue != "9/9" {
		t.Errorf("-lives bar = %q, want 9/9", lives.MetricsBarValue)
	}
}

// The two cards share one directory, so their backoff records must not share
// one file: a codex 429 would otherwise silence the claude agent too.
func TestStatePathPerProvider(t *testing.T) {
	dir := t.TempDir()
	if got := statePath(filepath.Join(dir, "claude.json")); got != filepath.Join(dir, ".ninelives-state.json") {
		t.Errorf("claude state = %q, want the legacy name", got)
	}
	if got := statePath(filepath.Join(dir, "codex.json")); got != filepath.Join(dir, ".ninelives-codex-state.json") {
		t.Errorf("codex state = %q, want a per-card name", got)
	}
}

// The agent argv must carry the codex prefix, or the scheduled run would
// silently refresh the claude card instead.
func TestAgentCommandCarriesProvider(t *testing.T) {
	if got := agentCommand(options{provider: "claude", out: "c.json"}, "/bin/ninelives", nil); len(got) != 3 || got[0] != "/bin/ninelives" || got[1] != "-out" || got[2] != "c.json" {
		t.Errorf("claude argv = %v", got)
	}
	got := agentCommand(options{provider: "codex", out: "x.json", lives: true}, "/bin/ninelives", []string{"-lives=true"})
	want := []string{"/bin/ninelives", "codex", "-out", "x.json", "-lives=true"}
	if len(got) != len(want) {
		t.Fatalf("codex argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("codex argv = %v, want %v", got, want)
		}
	}
}
