// ninelives reports how much of your Claude usage allowance is left and writes
// it as a RunCat Neo custom metrics card. `ninelives codex` does the same for
// OpenAI Codex, as a separate card and a separate launchd agent.
//
// Claude data source: GET https://api.anthropic.com/api/oauth/usage — the
// endpoint Claude Code's /usage command calls. It is undocumented and may
// change without notice. Authentication reuses the OAuth token Claude Code
// already stores (macOS Keychain "Claude Code-credentials", or
// ~/.claude/.credentials.json).
//
// Codex data source: GET https://chatgpt.com/backend-api/wham/usage and
// GET .../wham/rate-limit-reset-credits — the endpoints the Codex CLI polls
// for its rate-limit display. Also undocumented. Authentication reuses the
// ChatGPT tokens Codex CLI already stores in ~/.codex/auth.json.
//
// Limits are shared per account, so these numbers match what Claude Desktop
// shows under Settings > Usage and what the Codex app shows for rate limits.
//
//	ninelives              write the Claude metrics file once
//	ninelives codex        write the Codex metrics file once
//	ninelives install      register a launchd agent that keeps it fresh
//	ninelives uninstall    undo that
//	ninelives status       show what is registered and what it last wrote
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// version is stamped by the release build with -ldflags "-X main.version=...".
// A plain `go install module@vX` leaves it alone, so fall back to the module
// version the toolchain records instead of reporting "dev".
var version = "dev"

func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	return resolveVersion(version, info.Main.Version)
}

// resolveVersion prefers the stamped value, then the module version. "(devel)"
// is what the toolchain records for a local build, which tells us nothing.
func resolveVersion(stamped, module string) string {
	if stamped != "dev" && stamped != "" {
		return stamped
	}
	if module != "" && module != "(devel)" {
		return module
	}
	return "dev"
}

type options struct {
	provider string
	out      string
	title    string
	symbol   string
	bar      string
	lives    bool
	extra    bool
	credits  bool
	raw      bool
	stdout   bool
	timeout  time.Duration
	bin      string
	interval int
	dryRun   bool
	keep     bool
}

func defaultOut(provider string) string {
	return filepath.Join(homeDir(), ".config", "runcat-neo-metrics", metricsFile(provider))
}

// metricsFile is the file RunCat reads for each provider. Claude keeps its
// historical name; codex is a sibling card in the same directory.
func metricsFile(provider string) string {
	if provider == "codex" {
		return "codex.json"
	}
	return "claude.json"
}

// providerTitle and providerSymbol are the per-provider card defaults. Only
// the card's look changes; every other flag means the same thing.
func providerTitle(provider string) string {
	if provider == "codex" {
		return "Codex"
	}
	return "Claude"
}

func providerSymbol(provider string) string {
	if provider == "codex" {
		return "bolt"
	}
	return "staroflife"
}

// bind registers the flags a subcommand accepts. Every subcommand shares the
// card-shaping flags so `install -lives` and `run -lives` mean the same thing.
func (o *options) bind(fs *flag.FlagSet, sub, provider string) {
	o.provider = provider
	fs.StringVar(&o.out, "out", defaultOut(provider), "path to the metrics JSON RunCat Neo reads")
	fs.StringVar(&o.title, "title", providerTitle(provider), "card title")
	fs.StringVar(&o.symbol, "symbol", providerSymbol(provider), "SF Symbol name for the card")
	fs.StringVar(&o.bar, "bar", "5h", "which window drives the menu bar value: 5h, 7d or min")
	fs.BoolVar(&o.lives, "lives", false, `show "6/9" instead of "65%"`)
	fs.BoolVar(&o.extra, "extra", false, "also show allowance windows the tool does not recognise")
	fs.BoolVar(&o.credits, "credits", false, "also show extra-usage credits spent")
	fs.DurationVar(&o.timeout, "timeout", 15*time.Second, "HTTP timeout")

	switch sub {
	case "run":
		fs.BoolVar(&o.raw, "raw", false, "print the raw API response to stdout and exit")
		fs.BoolVar(&o.stdout, "stdout", false, "print the card to stdout instead of writing -out")
	case "install":
		fs.StringVar(&o.bin, "bin", "", "install the binary here first (default: launch it where it already is)")
		fs.IntVar(&o.interval, "interval", defaultInterval, "seconds between refreshes; 60 is the floor")
		fs.BoolVar(&o.dryRun, "dry-run", false, "print the launchd plist that would be installed and stop")
	case "uninstall":
		fs.BoolVar(&o.keep, "keep-metrics", false, "leave the metrics file in place")
	}
}

func main() {
	prov, sub, args := splitCommand(os.Args[1:])

	switch sub {
	case "help", "-h", "--help":
		usageText(os.Stdout)
		return
	case "version":
		fmt.Println("ninelives " + versionString())
		return
	case "run", "install", "uninstall", "status":
	default:
		fmt.Fprintf(os.Stderr, "ninelives: unknown command %q\n\n", sub)
		usageText(os.Stderr)
		os.Exit(2)
	}

	fs := flag.NewFlagSet("ninelives "+sub, flag.ExitOnError)
	fs.Usage = func() { usageText(os.Stderr) }
	var o options
	o.bind(fs, sub, prov)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := dispatch(sub, o, fs); err != nil {
		fmt.Fprintln(os.Stderr, "ninelives: "+err.Error())
		os.Exit(1)
	}
}

// splitCommand reads an optional provider prefix (claude, codex) and then the
// subcommand off the front of argv. The prefix defaults to claude, so every
// existing invocation keeps meaning exactly what it meant before. It is a
// function of its own so the parsing can be tested without running main.
func splitCommand(argv []string) (provider, sub string, args []string) {
	provider, sub, args = "claude", "run", argv
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return provider, sub, args
	}
	switch args[0] {
	case "claude", "codex":
		provider, args = args[0], args[1:]
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			sub, args = args[0], args[1:]
		}
	default:
		sub, args = args[0], args[1:]
	}
	return provider, sub, args
}

func dispatch(sub string, o options, fs *flag.FlagSet) error {
	if err := validateBar(o.bar); err != nil {
		return err
	}
	switch sub {
	case "run":
		return runOnce(o)
	case "install":
		return installAgent(o, agentArgs(fs))
	case "uninstall":
		return uninstallAgent(o, o.keep)
	case "status":
		return showStatus(o)
	}
	return fmt.Errorf("unreachable subcommand %q", sub)
}

func validateBar(bar string) error {
	switch bar {
	case "5h", "7d", "min":
		return nil
	}
	return fmt.Errorf("-bar %q: want 5h, 7d or min", bar)
}

// agentArgs turns the flags the user actually typed into the argument list the
// launchd agent should replay. Flags left at their default are omitted so the
// generated plist stays readable.
func agentArgs(fs *flag.FlagSet) []string {
	skip := map[string]bool{"out": true, "interval": true, "bin": true, "dry-run": true, "keep-metrics": true}
	var out []string
	fs.Visit(func(f *flag.Flag) {
		if skip[f.Name] {
			return
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			out = append(out, "-"+f.Name+"="+f.Value.String())
			return
		}
		out = append(out, "-"+f.Name, f.Value.String())
	})
	return out
}

func runOnce(o options) error {
	if o.raw {
		return printRaw(o)
	}
	if o.stdout {
		enc, err := encodeCard(o)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(enc)
		return err
	}
	return writeMetrics(o)
}

// printRaw dumps the provider's usage endpoints to stdout, headers to stderr,
// so an undocumented endpoint changing shape can be inspected without curl.
// Codex prints both endpoints it reads: the reset-credit count is the very
// thing whose disagreement with wham/usage made a second endpoint necessary,
// so -raw has to be able to see it too.
func printRaw(o options) error {
	if o.provider == "codex" {
		token, accountID, src, err := findCodexToken()
		if err != nil {
			return err
		}
		for _, url := range []string{codexUsageURL, codexResetCreditsURL} {
			fmt.Fprintln(os.Stderr, "GET "+url)
			body, hdr, err := codexGet(url, token, accountID, o.timeout)
			for _, h := range responseHeaders(hdr) {
				fmt.Fprintln(os.Stderr, h)
			}
			if err != nil {
				return fmt.Errorf("%w (token from %s)", err, src)
			}
			if _, err := os.Stdout.Write(pretty(body)); err != nil {
				return err
			}
		}
		return nil
	}
	token, src, err := findToken()
	if err != nil {
		return err
	}
	body, hdr, err := fetchUsage(token, o.timeout)
	for _, h := range responseHeaders(hdr) {
		fmt.Fprintln(os.Stderr, h)
	}
	if err != nil {
		return fmt.Errorf("%w (token from %s)", err, src)
	}
	_, err = os.Stdout.Write(pretty(body))
	return err
}

// writeMetrics is the whole point of the tool: fetch, format, replace the file.
// On failure it deliberately leaves the existing file untouched, so RunCat keeps
// showing the last good numbers with a stale "N minutes ago" instead of
// silently zeroing the bars.
func writeMetrics(o options) error {
	// A recorded 429 means the server already told us when to come back. Skip
	// the request rather than spending one of the window's few slots, and exit
	// successfully so the log does not fill with the same complaint.
	if d, ok := loadState(o).waiting(time.Now()); ok {
		fmt.Fprintf(os.Stderr, "ninelives: rate limited, skipping for another %s\n", d)
		return nil
	}
	enc, err := encodeCard(o)
	if err != nil {
		return err
	}
	return writeAtomic(o.out, enc)
}

// encodeCard fetches and encodes the card for the provider in the options.
func encodeCard(o options) ([]byte, error) {
	if o.provider == "codex" {
		return encodeCodexCard(o)
	}
	return encodeClaudeCard(o)
}

func encodeClaudeCard(o options) ([]byte, error) {
	token, src, err := findToken()
	if err != nil {
		return nil, err
	}
	body, _, err := fetchUsage(token, o.timeout)
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			st := loadState(o)
			st.Strikes++
			st.BackoffUntil = time.Now().Add(rl.RetryAfter)
			saveState(o, st)
		}
		return nil, fmt.Errorf("%w (token from %s)", err, src)
	}
	st := loadState(o)
	st.clearBackoff()
	saveState(o, st)
	var u usage
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("parsing usage response: %w", err)
	}
	if len(u.rows()) == 0 {
		return nil, errors.New("usage response carried no limit windows (run with -raw to inspect)")
	}
	noteNewWindows(o, u)
	enc, err := json.MarshalIndent(buildCard(u, o, time.Now()), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(enc, '\n'), nil
}

// encodeCodexCard fetches both codex endpoints and encodes the card. The
// windows decide whether the card is written; the reset count is a sidecar —
// if only it fails, the card still goes out without the Resets row.
func encodeCodexCard(o options) ([]byte, error) {
	token, accountID, src, err := findCodexToken()
	if err != nil {
		return nil, err
	}
	body, _, err := codexGet(codexUsageURL, token, accountID, o.timeout)
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			st := loadState(o)
			st.Strikes++
			st.BackoffUntil = time.Now().Add(rl.RetryAfter)
			saveState(o, st)
		}
		return nil, fmt.Errorf("%w (token from %s)", err, src)
	}
	st := loadState(o)
	st.clearBackoff()
	saveState(o, st)
	var u codexUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("parsing codex usage response: %w", err)
	}
	if len(u.rows()) == 0 {
		return nil, errors.New("codex usage response carried no limit windows (run with -raw to inspect)")
	}
	// Reset credits are a sidecar: their absence costs the Resets row, not the
	// card. But a 429 here means the usage endpoint would say the same next
	// round, so record the backoff now instead of paying for the lesson twice.
	resets, rerr := fetchCodexResets(token, accountID, o.timeout)
	st = loadState(o)
	if rerr != nil {
		var rl *rateLimitError
		if errors.As(rerr, &rl) {
			st.Strikes++
			st.BackoffUntil = time.Now().Add(rl.RetryAfter)
		}
		// Announce only when the reason changes: a plan without reset
		// credits answers the same 404 forever, and one line per run would
		// bury the log.
		if note := rerr.Error(); st.ResetCreditsNote != note {
			fmt.Fprintf(os.Stderr, "ninelives: dropping the Resets row: %v\n", rerr)
			st.ResetCreditsNote = note
		}
	} else {
		st.ResetCreditsNote = ""
	}
	saveState(o, st)
	enc, err := json.MarshalIndent(buildCodexCard(u, resets, o, time.Now()), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(enc, '\n'), nil
}

// noteNewWindows announces an allowance the card does not know about, once,
// the first time the API mentions it. A temporary capacity grant would arrive
// this way, and going unnoticed is the failure worth avoiding.
func noteNewWindows(o options, u usage) {
	unknown := u.unknownWindows()
	names := make([]string, 0, len(unknown))
	for _, r := range unknown {
		names = append(names, r.label)
	}
	st := loadState(o)
	if added := st.noteWindows(names); len(added) > 0 && !o.extra {
		fmt.Fprintf(os.Stderr, "ninelives: the API reported an allowance this card does not show: %s (add -extra)\n",
			strings.Join(added, ", "))
	}
	saveState(o, st)
}

func usageText(w *os.File) {
	fmt.Fprint(w, `ninelives — how much Claude and Codex allowance is left, in the RunCat Neo menu bar

usage:
  ninelives [flags]              fetch Claude usage once and write the metrics file
  ninelives codex [flags]        same, for OpenAI Codex (a separate card)
  ninelives install [flags]      install a launchd agent that refreshes it
  ninelives codex install [flags]
  ninelives uninstall            unload the agent and remove what it wrote
  ninelives codex uninstall
  ninelives status               show the agent and the last written card
  ninelives codex status
  ninelives version

The claude/codex prefix picks which card a command works on; it defaults to
claude. Codex only changes the defaults of -out, -title and -symbol.

flags (all subcommands):
  -out PATH        metrics file RunCat Neo reads
                    claude: ~/.config/runcat-neo-metrics/claude.json
                    codex:  ~/.config/runcat-neo-metrics/codex.json
  -lives           show "6/9" instead of "65%"
  -extra           also show allowance windows the tool does not recognise (claude)
  -credits         also show extra-usage credits spent (claude)
  -bar 5h|7d|min   which window drives the menu bar value (default 5h)
  -title NAME      card title (claude: Claude, codex: Codex)
  -symbol NAME     SF Symbol for the card (claude: staroflife, codex: bolt)
  -timeout D       HTTP timeout (default 15s)

run only:
  -raw             print the raw API response and exit
  -stdout          print the card instead of writing -out

install only:
  -bin PATH        copy the binary here and launch that copy
  -interval N      seconds between refreshes (default 120, floor 60)
  -dry-run         print the plist that would be installed and stop

uninstall only:
  -keep-metrics    leave the metrics file in place

examples:
  ninelives -stdout
  ninelives install -lives -bar min
  ninelives codex install
  ninelives status
`)
}
