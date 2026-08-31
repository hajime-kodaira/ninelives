package main

// LaunchAgent management. Everything install.sh used to do lives here so the
// tool installs itself: `ninelives install`, `ninelives uninstall`.

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	agentLabel = "io.local.ninelives"

	// Measured against the live endpoint: 5 requests succeed inside a 5 minute
	// window and the 6th returns 429 with Retry-After: 300. So one request per
	// 60s is exactly the whole budget, and anything faster is guaranteed to be
	// throttled. Clock drift can still push a 6th request into a window at the
	// floor, which is why 120s — roughly half the budget — is the default: it
	// leaves room for Claude Code's own /usage, which shares the same limit.
	minInterval     = 60
	defaultInterval = 120
)

func plistPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", agentLabel+".plist")
}

func logPath() string {
	return filepath.Join(homeDir(), "Library", "Logs", "ninelives.log")
}

func defaultBin() string {
	return filepath.Join(homeDir(), "bin", "ninelives")
}

func validateInterval(n int) error {
	if n < minInterval {
		return fmt.Errorf("-interval %d is below the %ds floor: the endpoint allows 5 requests per 5 minutes, so anything faster is throttled outright", n, minInterval)
	}
	return nil
}

// intervalNote warns when the agent would eat most of the shared budget.
func intervalNote(n int) string {
	if n >= defaultInterval {
		return ""
	}
	return fmt.Sprintf("note: at %ds this uses %d of the 5 requests each 5 minute window, leaving little for Claude Code's own /usage", n, 300/n)
}

// --- install -----------------------------------------------------------

func installAgent(o options, extraArgs []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("install manages a launchd agent, which only exists on macOS")
	}
	if err := validateInterval(o.interval); err != nil {
		return err
	}
	if note := intervalNote(o.interval); note != "" {
		fmt.Println(note)
	}

	bin, copyNeeded, err := targetBin(o.bin)
	if err != nil {
		return err
	}
	if o.dryRun {
		args := append([]string{bin, "-out", o.out}, extraArgs...)
		_, err := os.Stdout.Write(plistXML(args, logPath(), o.interval))
		return err
	}
	if copyNeeded {
		fmt.Printf("==> installing %s\n", bin)
		if err := copySelf(bin); err != nil {
			return err
		}
	}

	// Fetch once before registering anything, so a missing token fails here
	// rather than silently in a background job.
	fmt.Printf("==> writing %s\n", o.out)
	if err := writeMetrics(o); err != nil {
		return err
	}

	args := append([]string{bin, "-out", o.out}, extraArgs...)
	plist := plistPath()
	fmt.Printf("==> writing %s\n", plist)
	if err := os.MkdirAll(filepath.Dir(logPath()), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(plist, plistXML(args, logPath(), o.interval)); err != nil {
		return err
	}

	st := loadState(o.out)
	st.AgentVersion = versionString()
	saveState(o.out, st)

	fmt.Println("==> loading the launchd agent")
	// bootout first so a re-run replaces the previous registration.
	_ = launchctl("bootout", domain()+"/"+agentLabel)
	if err := launchctl("bootstrap", domain(), plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}

	fmt.Printf(`
Done. %s now refreshes every %d seconds.

Register the file in RunCat Neo:
  Settings > Metrics > Custom Metrics > +
  ~/.config is hidden, so press Cmd+Shift+G and paste the path above.

Errors from the scheduled runs land in %s
`, o.out, o.interval, logPath())
	return nil
}

// targetBin decides which binary path the agent should launch, and whether we
// have to put a copy there first. A binary built by `go run`, or one just
// downloaded into a temp directory, will not survive, so it cannot be the thing
// launchd points at.
func targetBin(want string) (path string, copyNeeded bool, err error) {
	self, err := selfPath()
	if err != nil {
		return "", false, err
	}
	if want == "" {
		if !ephemeral(self) {
			return self, false, nil
		}
		return defaultBin(), true, nil
	}
	want, err = filepath.Abs(want)
	if err != nil {
		return "", false, err
	}
	return want, want != self, nil
}

func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

func copySelf(dst string) error {
	self, err := selfPath()
	if err != nil {
		return err
	}
	return copyExecutable(self, dst)
}

// ephemeral reports whether a binary sits somewhere that will not survive:
// a `go run` build directory, or a temp dir someone downloaded a release into.
func ephemeral(path string) bool {
	if strings.Contains(path, "/go-build") {
		return true
	}
	// The /private forms are listed explicitly rather than left to under()'s
	// symlink resolution, which only finds them when running on macOS.
	for _, dir := range []string{
		os.TempDir(),
		"/tmp", "/private/tmp",
		"/var/tmp", "/private/var/tmp",
		"/var/folders", "/private/var/folders",
	} {
		if under(path, dir) {
			return true
		}
	}
	return false
}

// under compares against both the literal and the symlink-resolved directory.
// On macOS /tmp and /var are symlinks into /private, and os.Executable is
// resolved, so a literal prefix test alone silently misses every temp path.
func under(path, dir string) bool {
	if dir == "" {
		return false
	}
	for _, d := range []string{dir, resolve(dir)} {
		if d != "" && strings.HasPrefix(path, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

func resolve(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return ""
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Write via a temp file: replacing a binary that is currently executing
	// fails on some systems, but renaming over it does not.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".ninelives-*")
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
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// --- uninstall ---------------------------------------------------------

func uninstallAgent(o options, keepData bool) error {
	if runtime.GOOS != "darwin" {
		return errors.New("uninstall manages a launchd agent, which only exists on macOS")
	}
	_ = launchctl("bootout", domain()+"/"+agentLabel)
	fmt.Println("==> unloaded the launchd agent")

	for _, p := range []string{plistPath(), logPath()} {
		if err := os.Remove(p); err == nil {
			fmt.Printf("==> removed %s\n", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if !keepData {
		for _, p := range []string{o.out, statePath(o.out)} {
			if err := os.Remove(p); err == nil {
				fmt.Printf("==> removed %s\n", p)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	fmt.Println("\nThe binary itself is left in place. Remove the card from RunCat Neo's settings by hand.")
	return nil
}

// --- status ------------------------------------------------------------

func showStatus(o options) error {
	st := loadState(o.out)

	fmt.Printf("agent    %s\n", agentState())
	fmt.Printf("plist    %s\n", exists(plistPath()))
	reportBinaries(st)
	fmt.Printf("log      %s\n", exists(logPath()))
	fmt.Printf("metrics  %s\n", o.out)
	fmt.Printf("backoff  %s\n", loadState(o.out).describe(time.Now()))

	data, err := os.ReadFile(o.out)
	if err != nil {
		fmt.Printf("         not written yet (%v)\n", err)
		return nil
	}
	var c card
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Printf("         unreadable: %v\n", err)
		return nil
	}
	age := "unknown age"
	if t, err := time.Parse(time.RFC3339, c.LastUpdatedDate); err == nil {
		age = fmt.Sprintf("updated %s ago", time.Since(t).Round(time.Second))
	}
	fmt.Printf("         %s · bar %s\n", age, c.MetricsBarValue)
	for _, m := range c.Metrics {
		fmt.Printf("         %-10s %s\n", m.Title, m.FormattedValue)
	}
	return nil
}

// reportBinaries shows which binary the agent actually launches and how its
// version compares to the one being run right now. Updating is just replacing
// the binary, so these two are expected to match; a mismatch means either the
// update did not land where the agent looks, or the plist predates it.
func reportBinaries(st state) {
	mine := versionString()
	fmt.Printf("this     %s\n", mine)

	bin, err := registeredBin()
	if err != nil {
		fmt.Println("runs     (no agent installed)")
		return
	}
	theirs := binVersion(bin)
	fmt.Printf("runs     %s (%s)\n", bin, theirs)

	if theirs != mine && theirs != "unknown" {
		fmt.Printf("         note: the agent runs a different build than this one\n")
	}
	if st.AgentVersion != "" && st.AgentVersion != mine {
		fmt.Printf("         note: the plist was written by %s; re-run `ninelives install` if flags changed\n", st.AgentVersion)
	}
}

// registeredBin reads the binary path out of the installed plist rather than
// trusting a recorded copy, so a hand-edited plist still reports the truth.
func registeredBin() (string, error) {
	out, err := exec.Command("plutil", "-extract", "ProgramArguments.0", "raw", "-o", "-", plistPath()).Output()
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errors.New("plist has no ProgramArguments")
	}
	return path, nil
}

func binVersion(path string) string {
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "ninelives "))
}

func agentState() string {
	if runtime.GOOS != "darwin" {
		return "n/a (not macOS)"
	}
	if err := launchctl("print", domain()+"/"+agentLabel); err != nil {
		return "not loaded"
	}
	return "loaded"
}

func exists(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path + " (missing)"
	}
	return path
}

// --- launchd plumbing --------------------------------------------------

func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	return nil
}

func plistXML(args []string, logFile string, interval int) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + esc(agentLabel) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	fmt.Fprintf(&b, "\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", interval)
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + esc(logFile) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func esc(s string) string {
	var b bytes.Buffer
	// Paths can legally contain & and <, which would otherwise break the plist.
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
