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
//
//	ninelives              write the metrics file once
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
	"strings"
	"time"
)

// version is stamped by the release build with -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	out      string
	title    string
	symbol   string
	bar      string
	lives    bool
	raw      bool
	stdout   bool
	timeout  time.Duration
	bin      string
	interval int
	dryRun   bool
	keep     bool
}

func defaultOut() string {
	return filepath.Join(homeDir(), ".config", "runcat-neo-metrics", "claude.json")
}

// bind registers the flags a subcommand accepts. Every subcommand shares the
// card-shaping flags so `install -lives` and `run -lives` mean the same thing.
func (o *options) bind(fs *flag.FlagSet, sub string) {
	fs.StringVar(&o.out, "out", defaultOut(), "path to the metrics JSON RunCat Neo reads")
	fs.StringVar(&o.title, "title", "Claude", "card title")
	fs.StringVar(&o.symbol, "symbol", "staroflife", "SF Symbol name for the card")
	fs.StringVar(&o.bar, "bar", "5h", "which window drives the menu bar value: 5h, 7d or min")
	fs.BoolVar(&o.lives, "lives", false, `show "6/9" instead of "65%"`)
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
	sub, args := "run", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}

	switch sub {
	case "help", "-h", "--help":
		usageText(os.Stdout)
		return
	case "version":
		fmt.Println("ninelives " + version)
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
	o.bind(fs, sub)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := dispatch(sub, o, fs); err != nil {
		fmt.Fprintln(os.Stderr, "ninelives: "+err.Error())
		os.Exit(1)
	}
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

// writeMetrics is the whole point of the tool: fetch, format, replace the file.
// On failure it deliberately leaves the existing file untouched, so RunCat keeps
// showing the last good numbers with a stale "N minutes ago" instead of
// silently zeroing the bars.
func writeMetrics(o options) error {
	// A recorded 429 means the server already told us when to come back. Skip
	// the request rather than spending one of the window's few slots, and exit
	// successfully so the log does not fill with the same complaint.
	if d, ok := loadState(o.out).waiting(time.Now()); ok {
		fmt.Fprintf(os.Stderr, "ninelives: rate limited, skipping for another %s\n", d)
		return nil
	}
	enc, err := encodeCard(o)
	if err != nil {
		return err
	}
	return writeAtomic(o.out, enc)
}

func encodeCard(o options) ([]byte, error) {
	token, src, err := findToken()
	if err != nil {
		return nil, err
	}
	body, _, err := fetchUsage(token, o.timeout)
	if err != nil {
		var rl *rateLimitError
		if errors.As(err, &rl) {
			st := loadState(o.out)
			st.Strikes++
			st.BackoffUntil = time.Now().Add(rl.RetryAfter)
			saveState(o.out, st)
		}
		return nil, fmt.Errorf("%w (token from %s)", err, src)
	}
	clearState(o.out)
	var u usage
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("parsing usage response: %w", err)
	}
	if len(u.rows()) == 0 {
		return nil, errors.New("usage response carried no limit windows (run with -raw to inspect)")
	}
	enc, err := json.MarshalIndent(buildCard(u, o.title, o.symbol, o.bar, o.lives, time.Now()), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(enc, '\n'), nil
}

func usageText(w *os.File) {
	fmt.Fprint(w, `ninelives — how much Claude allowance is left, in the RunCat Neo menu bar

usage:
  ninelives [flags]              fetch once and write the metrics file
  ninelives install [flags]      install a launchd agent that refreshes it
  ninelives uninstall            unload the agent and remove what it wrote
  ninelives status               show the agent and the last written card
  ninelives version

flags (all subcommands):
  -out PATH        metrics file RunCat Neo reads
                   (default ~/.config/runcat-neo-metrics/claude.json)
  -lives           show "6/9" instead of "65%"
  -bar 5h|7d|min   which window drives the menu bar value (default 5h)
  -title NAME      card title (default Claude)
  -symbol NAME     SF Symbol for the card (default staroflife)
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
  ninelives status
`)
}
