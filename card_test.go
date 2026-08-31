package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLivesLeft(t *testing.T) {
	for _, c := range []struct {
		remaining float64
		want      int
	}{
		{100, 9}, {89, 9}, {88.8, 8}, {57, 6}, {11.2, 2}, {0.4, 1}, {0, 0},
	} {
		if got := livesLeft(c.remaining); got != c.want {
			t.Errorf("livesLeft(%v) = %d, want %d", c.remaining, got, c.want)
		}
	}
}

func TestUntil(t *testing.T) {
	now := time.Date(2026, 8, 30, 16, 33, 0, 0, time.UTC)
	for _, c := range []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now.Add(-time.Minute), " · resetting"},
		{now.Add(42 * time.Minute), " · 42m"},
		{now.Add(96 * time.Minute), " · 1h36m"},
		{now.Add(90 * time.Hour), " · 3d18h"},
	} {
		if got := until(now, c.in); got != c.want {
			t.Errorf("until(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The payload the live endpoint actually returns, trimmed to what we read.
const samplePayload = `{
  "five_hour": {"utilization": 43, "resets_at": "2026-08-30T18:10:00.269765+00:00"},
  "seven_day": {"utilization": 7, "resets_at": "2026-09-03T11:00:00.269818+00:00"},
  "seven_day_opus": null,
  "limits": [
    {"kind":"session","group":"session","percent":43,"resets_at":"2026-08-30T18:10:00.269765+00:00","scope":null},
    {"kind":"weekly_all","group":"weekly","percent":7,"resets_at":"2026-09-03T11:00:00.269818+00:00","scope":null},
    {"kind":"weekly_scoped","group":"weekly","percent":11,"resets_at":"2026-09-03T11:00:00.270228+00:00",
     "scope":{"model":{"display_name":"Fable","id":null},"surface":null}}
  ]
}`

func parseSample(t *testing.T) usage {
	t.Helper()
	var u usage
	if err := json.Unmarshal([]byte(samplePayload), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBuildCardFromLimits(t *testing.T) {
	now := time.Date(2026, 8, 30, 16, 34, 0, 0, time.UTC)
	c := buildCard(parseSample(t), options{title: "Claude", symbol: "staroflife", bar: "5h"}, now)

	if c.MetricsBarValue != "57%" {
		t.Errorf("bar = %q, want %q", c.MetricsBarValue, "57%")
	}
	want := []struct{ title, formatted string }{
		{"5h", "57% left · 1h36m"},
		{"7d", "93% left · 3d18h"},
		{"7d Fable", "89% left · 3d18h"},
	}
	if len(c.Metrics) != len(want) {
		t.Fatalf("got %d rows, want %d", len(c.Metrics), len(want))
	}
	for i, w := range want {
		if c.Metrics[i].Title != w.title || c.Metrics[i].FormattedValue != w.formatted {
			t.Errorf("row %d = %q/%q, want %q/%q", i,
				c.Metrics[i].Title, c.Metrics[i].FormattedValue, w.title, w.formatted)
		}
		if c.Metrics[i].NormalizedValue == nil {
			t.Errorf("row %d has no normalizedValue, so RunCat draws no bar", i)
		}
	}
}

func TestBarSelection(t *testing.T) {
	u := parseSample(t)
	now := time.Date(2026, 8, 30, 16, 34, 0, 0, time.UTC)
	for bar, want := range map[string]string{"5h": "57%", "7d": "93%", "min": "57%"} {
		if got := buildCard(u, options{title: "Claude", symbol: "staroflife", bar: bar}, now).MetricsBarValue; got != want {
			t.Errorf("-bar %s = %q, want %q", bar, got, want)
		}
	}
	if got := buildCard(u, options{title: "Claude", symbol: "staroflife", bar: "5h", lives: true}, now).MetricsBarValue; got != "6/9" {
		t.Errorf("-lives bar = %q, want %q", got, "6/9")
	}
}

// Falling back to the flat fields matters if the limits array ever disappears.
func TestRowsFallback(t *testing.T) {
	u := parseSample(t)
	u.Limits = nil
	rows := u.rows()
	if len(rows) != 2 || rows[0].label != "5h" || rows[1].label != "7d" {
		t.Fatalf("fallback rows = %+v", rows)
	}
	if rows[0].used != 43 {
		t.Errorf("five_hour utilization = %v, want 43", rows[0].used)
	}
}

func TestResolveVersion(t *testing.T) {
	for _, c := range []struct{ stamped, module, want string }{
		{"v1.2.3", "v9.9.9", "v1.2.3"}, // a release build wins
		{"dev", "v1.0.0", "v1.0.0"},    // go install module@v1.0.0
		{"dev", "(devel)", "dev"},      // local go build
		{"dev", "", "dev"},
	} {
		if got := resolveVersion(c.stamped, c.module); got != c.want {
			t.Errorf("resolveVersion(%q, %q) = %q, want %q", c.stamped, c.module, got, c.want)
		}
	}
}

// A temporary capacity grant would most plausibly arrive either as a new kind
// inside limits, or as a codenamed top-level field. Neither may be dropped.
const grantPayload = `{
  "five_hour": {"utilization": 51, "resets_at": "2026-08-31T16:19:59+00:00"},
  "seven_day": {"utilization": 13, "resets_at": "2026-09-03T10:59:59+00:00"},
  "omelette_promotional": {"utilization": 25, "resets_at": "2026-09-07T00:00:00+00:00"},
  "nimbus_quill": {"utilization": 0, "resets_at": null},
  "tangelo": null,
  "member_dashboard_available": false,
  "extra_usage": {"is_enabled": true, "currency": "USD", "decimal_places": 2.0, "used_credits": 735.0},
  "limits": [
    {"kind":"session","group":"session","percent":51,"resets_at":"2026-08-31T16:19:59+00:00","scope":null},
    {"kind":"weekly_all","group":"weekly","percent":13,"resets_at":"2026-09-03T10:59:59+00:00","scope":null},
    {"kind":"promotional_boost","group":"promo","percent":25,"resets_at":"2026-09-07T00:00:00+00:00","scope":null}
  ]
}`

func parseGrant(t *testing.T) usage {
	t.Helper()
	var u usage
	if err := json.Unmarshal([]byte(grantPayload), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// An unrecognised limits kind must show up on its own, with no code change.
func TestUnknownLimitKindBecomesARow(t *testing.T) {
	rows := parseGrant(t).rows()
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	if rows[2].label != "promotional_boost" || rows[2].used != 25 {
		t.Errorf("unknown kind became %+v", rows[2])
	}
	// It is not one of the windows -bar can name, so it must not hijack the bar.
	if got := barValue(rows, "5h"); got != 49 {
		t.Errorf("bar = %v, want 49 (the 5h window)", got)
	}
}

func TestUnknownTopLevelWindows(t *testing.T) {
	u := parseGrant(t)
	var got []string
	for _, r := range u.unknownWindows() {
		got = append(got, r.label)
	}
	want := []string{"nimbus_quill", "omelette_promotional"} // sorted, nulls dropped
	if len(got) != len(want) {
		t.Fatalf("unknownWindows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unknownWindows = %v, want %v", got, want)
		}
	}

	// Off by default: the card stays about the plan windows.
	base := buildCard(u, options{bar: "5h"}, time.Now())
	if len(base.Metrics) != 3 {
		t.Errorf("default card had %d rows, want 3", len(base.Metrics))
	}

	c := buildCard(u, options{bar: "5h", extra: true, credits: true}, time.Now())
	titles := []string{}
	for _, m := range c.Metrics {
		titles = append(titles, m.Title)
	}
	if len(titles) != 6 || titles[3] != "nimbus_quill" || titles[4] != "omelette_promotional" || titles[5] != "Credits" {
		t.Errorf("-extra -credits card rows = %v", titles)
	}
	if c.Metrics[5].NormalizedValue != nil {
		t.Error("credits are money, so the row must not draw a bar")
	}
	if c.Metrics[5].FormattedValue != "$7.35 used" {
		t.Errorf("credits row = %q", c.Metrics[5].FormattedValue)
	}
	// nimbus_quill sits at 0% used, which is 100% left; the bar must ignore it.
	if c.MetricsBarValue != "49%" {
		t.Errorf("bar = %q, want 49%%", c.MetricsBarValue)
	}
}

func TestNewWindowsAreAnnouncedOnce(t *testing.T) {
	var s state
	if added := s.noteWindows([]string{"nimbus_quill"}); len(added) != 1 || added[0] != "nimbus_quill" {
		t.Fatalf("first sighting = %v", added)
	}
	if added := s.noteWindows([]string{"nimbus_quill"}); len(added) != 0 {
		t.Errorf("the same window was announced twice: %v", added)
	}
	if added := s.noteWindows([]string{"nimbus_quill", "omelette_promotional"}); len(added) != 1 || added[0] != "omelette_promotional" {
		t.Errorf("a newly granted window was missed: %v", added)
	}
}

// A malformed optional field must cost only that field, never the whole card.
func TestBrokenExtraUsageDoesNotBreakTheCard(t *testing.T) {
	var u usage
	body := `{"limits":[{"kind":"session","percent":51,"resets_at":"2026-08-31T16:19:59+00:00"}],
	          "extra_usage":{"used_credits":"lots","is_enabled":true}}`
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("a broken extra_usage failed the whole parse: %v", err)
	}
	if u.ExtraUsage != nil {
		t.Error("a broken extra_usage should be dropped, not half-decoded")
	}
	if len(u.rows()) != 1 {
		t.Errorf("the plan windows were lost: %+v", u.rows())
	}
}

func TestExtraUsageSpent(t *testing.T) {
	for _, c := range []struct {
		e    extraUsage
		want string
	}{
		{extraUsage{Currency: "USD", DecimalPlaces: 2, UsedCredits: 735}, "$7.35"},
		{extraUsage{Currency: "JPY", DecimalPlaces: 0, UsedCredits: 1200}, "¥1200"},
		{extraUsage{Currency: "XYZ", DecimalPlaces: 2, UsedCredits: 5}, "XYZ 0.05"},
	} {
		if got := c.e.spent(); got != c.want {
			t.Errorf("spent() = %q, want %q", got, c.want)
		}
	}
}
