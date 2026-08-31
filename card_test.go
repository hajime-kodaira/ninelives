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
	c := buildCard(parseSample(t), "Claude", "staroflife", "5h", false, now)

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
		if got := buildCard(u, "Claude", "staroflife", bar, false, now).MetricsBarValue; got != want {
			t.Errorf("-bar %s = %q, want %q", bar, got, want)
		}
	}
	if got := buildCard(u, "Claude", "staroflife", "5h", true, now).MetricsBarValue; got != "6/9" {
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
