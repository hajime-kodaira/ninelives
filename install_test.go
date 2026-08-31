package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistXMLEscapesPaths(t *testing.T) {
	// A path with & and < is legal on macOS and would otherwise break the XML.
	got := string(plistXML([]string{"/Users/a&b/bin/ninelives", "-out", "/tmp/<x>.json"}, "/tmp/log", 300))

	for _, want := range []string{
		"<string>io.local.ninelives</string>",
		"<string>/Users/a&amp;b/bin/ninelives</string>",
		"<string>/tmp/&lt;x&gt;.json</string>",
		"<integer>300</integer>",
		"<key>RunAtLoad</key>\n\t<true/>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "<string>") != strings.Count(got, "</string>") {
		t.Error("unbalanced <string> tags")
	}
}

// agentArgs must replay what the user typed and nothing else, or `install`
// would bake defaults into the plist and drift from the flag definitions.
func TestAgentArgsOnlyCarriesExplicitFlags(t *testing.T) {
	parse := func(args ...string) []string {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		var o options
		o.bind(fs, "install")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return agentArgs(fs)
	}

	if got := parse(); len(got) != 0 {
		t.Errorf("no flags given, got %v", got)
	}
	if got := strings.Join(parse("-lives"), " "); got != "-lives=true" {
		t.Errorf("-lives => %q", got)
	}
	if got := strings.Join(parse("-bar", "min", "-lives"), " "); got != "-bar min -lives=true" {
		t.Errorf("-bar min -lives => %q", got)
	}
	// -out, -bin, -interval and -dry-run configure the install itself and must
	// not leak into the agent's argument list.
	if got := parse("-out", "/tmp/x.json", "-bin", "/tmp/b", "-interval", "600", "-dry-run"); len(got) != 0 {
		t.Errorf("install-only flags leaked into the agent args: %v", got)
	}
}

// The floor is not a style preference: 5 requests per 5 minutes is measured, so
// below 60s every run past the fifth is throttled.
func TestValidateInterval(t *testing.T) {
	for _, n := range []int{59, 30, 1, 0, -5} {
		if err := validateInterval(n); err == nil {
			t.Errorf("-interval %d was accepted", n)
		}
	}
	for _, n := range []int{60, 120, 300, 3600} {
		if err := validateInterval(n); err != nil {
			t.Errorf("-interval %d rejected: %v", n, err)
		}
	}
	if intervalNote(300) != "" || intervalNote(defaultInterval) != "" {
		t.Error("no note expected at or above the default interval")
	}
	if !strings.Contains(intervalNote(60), "5 of the 5 requests") {
		t.Errorf("note at the floor = %q", intervalNote(60))
	}
}

func TestEphemeral(t *testing.T) {
	if !ephemeral(filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "ninelives")) {
		t.Error("a go run build directory should count as ephemeral")
	}
	if ephemeral("/Users/someone/bin/ninelives") {
		t.Error("an installed path should not count as ephemeral")
	}
}

func TestValidateBar(t *testing.T) {
	for _, bar := range []string{"5h", "7d", "min"} {
		if err := validateBar(bar); err != nil {
			t.Errorf("-bar %s rejected: %v", bar, err)
		}
	}
	if err := validateBar("nope"); err == nil || !strings.Contains(err.Error(), "want 5h, 7d or min") {
		t.Errorf("bad -bar gave %v", err)
	}
}
