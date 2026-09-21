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
	// Measured against the live endpoint: 5 requests succeed inside a 5 minute
	// window and the 6th returns 429 with Retry-After: 300. So one request per
	// 60s is exactly the whole budget, and anything faster is guaranteed to be
	// throttled. Clock drift can still push a 6th request into a window at the
	// floor, which is why 120s — roughly half the budget — is the default: it
	// leaves room for Claude Code's own /usage, which shares the same limit.
	minInterval     = 60
	defaultInterval = 120
)

// agentLabelFor names the launchd agent per provider. Codex gets its own so
// both cards can run side by side without one booting out the other.
func agentLabelFor(provider string) string {
	if provider == "codex" {
		return "io.local.ninelives.codex"
	}
	return "io.local.ninelives"
}

func plistPath(o options) string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", agentLabelFor(o.provider)+".plist")
}

func logPath(o options) string {
	name := "ninelives.log"
	if o.provider == "codex" {
		name = "ninelives-codex.log"
	}
	return filepath.Join(homeDir(), "Library", "Logs", name)
}

func defaultBin() string {
	return filepath.Join(homeDir(), "bin", "ninelives")
}

func validateInterval(n int, provider string) error {
	if n >= minInterval {
		return nil
	}
	if provider == "codex" {
		return fmt.Errorf("-interval %d is below the %ds floor", n, minInterval)
	}
	return fmt.Errorf("-interval %d is below the %ds floor: the endpoint allows 5 requests per 5 minutes, so anything faster is throttled outright", n, minInterval)
}

// intervalNote warns when the agent would eat most of the shared budget. The
// budget is a measured fact about the Claude endpoint; the codex endpoints are
// unmeasured, so there is no equivalent note to make there.
func intervalNote(n int, provider string) string {
	if n >= defaultInterval || provider == "codex" {
		return ""
	}
	return fmt.Sprintf("note: at %ds this uses %d of the 5 requests each 5 minute window, leaving little for Claude Code's own /usage", n, 300/n)
}

// --- install -----------------------------------------------------------

func installAgent(o options, extraArgs []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("install manages a launchd agent, which only exists on macOS")
	}
	if err := validateInterval(o.interval, o.provider); err != nil {
		return err
	}
	if note := intervalNote(o.interval, o.provider); note != "" {
		fmt.Println(note)
	}

	bin, copyNeeded, err := targetBin(o.bin)
	if err != nil {
		return err
	}
	if o.dryRun {
		args := agentCommand(o, bin, extraArgs)
		_, err := os.Stdout.Write(plistXML(agentLabelFor(o.provider), args, logPath(o), o.interval))
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

	args := agentCommand(o, bin, extraArgs)
	plist := plistPath(o)
	fmt.Printf("==> writing %s\n", plist)
	if err := os.MkdirAll(filepath.Dir(logPath(o)), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(plist, plistXML(agentLabelFor(o.provider), args, logPath(o), o.interval)); err != nil {
		return err
	}

	st := loadState(o)
	st.AgentVersion = versionString()
	saveState(o, st)

	fmt.Println("==> loading the launchd agent")
	// bootout first so a re-run replaces the previous registration.
	_ = launchctl("bootout", domain()+"/"+agentLabelFor(o.provider))
	if err := launchctl("bootstrap", domain(), plist); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}

	if note := pathNote(bin); note != "" {
		fmt.Print(note)
	}

	fmt.Printf(`
Done. %s now refreshes every %d seconds.

Register the file in RunCat Neo:
  Settings > Metrics > Custom Metrics > +
  ~/.config is hidden, so press Cmd+Shift+G and paste the path above.

Errors from the scheduled runs land in %s
`, o.out, o.interval, logPath(o))
	return nil
}

// agentCommand is the argv the launchd agent replays: the binary, the provider
// prefix when this card is not the claude one, then the user's flags.
func agentCommand(o options, bin string, extra []string) []string {
	args := []string{bin}
	if o.provider == "codex" {
		args = append(args, "codex")
	}
	args = append(args, "-out", o.out)
	return append(args, extra...)
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
// pathNote warns when the installed binary sits outside PATH. The agent itself
// is fine — the plist carries the absolute path — but typing `ninelives` later
// will not resolve, which reads as a failed install.
func pathNote(bin string) string {
	dir := filepath.Dir(bin)
	if onPath(dir) {
		return ""
	}
	return fmt.Sprintf(`
note: %s is not on your PATH, so typing `+"`ninelives`"+` will not work.
      The agent is unaffected; it launches the absolute path above.
      To fix it:  echo 'export PATH="$PATH:%s"' >> ~/.zshrc && exec zsh
`, dir, dir)
}

// onPath reports whether dir is one of the PATH entries.
func onPath(dir string) bool {
	dir = filepath.Clean(dir)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry != "" && filepath.Clean(entry) == dir {
			return true
		}
	}
	return false
}

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
	_ = launchctl("bootout", domain()+"/"+agentLabelFor(o.provider))
	fmt.Println("==> unloaded the launchd agent")

	for _, p := range []string{plistPath(o), logPath(o)} {
		if err := os.Remove(p); err == nil {
			fmt.Printf("==> removed %s\n", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if !keepData {
		for _, p := range []string{o.out, statePath(o)} {
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
	st := loadState(o)

	fmt.Printf("agent    %s\n", agentState(o))
	fmt.Printf("plist    %s\n", exists(plistPath(o)))
	reportBinaries(st, o)
	fmt.Printf("log      %s\n", exists(logPath(o)))
	fmt.Printf("metrics  %s\n", o.out)
	fmt.Printf("backoff  %s\n", loadState(o).describe(time.Now()))

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
func reportBinaries(st state, o options) {
	mine := versionString()
	fmt.Printf("this     %s\n", mine)

	bin, err := registeredBin(o)
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
func registeredBin(o options) (string, error) {
	out, err := exec.Command("plutil", "-extract", "ProgramArguments.0", "raw", "-o", "-", plistPath(o)).Output()
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

func agentState(o options) string {
	if runtime.GOOS != "darwin" {
		return "n/a (not macOS)"
	}
	if err := launchctl("print", domain()+"/"+agentLabelFor(o.provider)); err != nil {
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

func plistXML(label string, args []string, logFile string, interval int) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	// Three WriteStrings rather than one concatenated argument: gopls flags
	// the concatenation, and the plist bytes must stay exactly as the golden
	// test has them.
	b.WriteString("\t<key>Label</key>\n\t<string>")
	b.WriteString(esc(label))
	b.WriteString("</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		b.WriteString("\t\t<string>")
		b.WriteString(esc(a))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
	fmt.Fprintf(&b, "\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", interval)
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>")
	b.WriteString(esc(logFile))
	b.WriteString("</string>\n")
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
