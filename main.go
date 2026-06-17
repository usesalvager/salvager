// Command salvager is a filesystem-level local-history safety net for agents.
//
//	salvager init                     connect this project's agent to salvager
//	salvager watch [--root <path>]    start the watcher (runs until killed)
//	salvager service install|uninstall|status
//	                                  run the watcher as a persistent service
//	salvager history <file>           list recorded versions of a file
//	salvager show <file> <ts>         print the content of one version
//	salvager restore <file> <ts>      restore a file to a version (reversible)
//	salvager mcp                      start the MCP server (stdio)
//	salvager gc [--max-age 7d] [--max-bytes 500M]
//	                                  purge old revisions and cap store size
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/usesalvager/salvager/ignore"
	"github.com/usesalvager/salvager/mcp"
	"github.com/usesalvager/salvager/store"
	"github.com/usesalvager/salvager/version"
	"github.com/usesalvager/salvager/watch"
)

const usage = `salvager — local history for agents

Usage:
  salvager init [--no-claude-md] [--undo]
                                    connect this project's agent (MCP + CLAUDE.md)
  salvager watch [--root <path>] [--allow-partial]
                                    start the watcher (runs until killed)
  salvager service install | uninstall | status [--json]
                                    run the watcher as a persistent service
  salvager history <file>           list recorded versions of a file
  salvager show <file> <timestamp>  print the content of one version
  salvager restore <file> <ts>      restore a file to a version (reversible)
  salvager mcp                      start the MCP server (stdio)
  salvager gc [--max-age 7d] [--max-bytes 500M]
                                    purge old revisions and cap store size
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}

	args := os.Args[2:]
	switch os.Args[1] {
	case "init":
		cmdInit(root, args)
	case "watch":
		cmdWatch(root, args)
	case "service":
		cmdService(root, args)
	case "history":
		cmdHistory(root, args)
	case "show":
		cmdShow(root, args)
	case "restore":
		cmdRestore(root, args)
	case "mcp":
		cmdMCP(root)
	case "gc":
		cmdGC(root, args)
	case "--version", "-version", "version":
		fmt.Println("salvager", version.Version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "salvager: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// parseWatchFlags applies `watch`'s flags to the cwd-derived root. --root
// overrides the root with an explicit absolute path (resolved via filepath.Abs);
// absent, the root is returned unchanged — zero regression for existing usage.
// Kept pure so the flag contract is unit-testable without starting a watcher.
func parseWatchFlags(root string, args []string) (string, bool, error) {
	allowPartial := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--allow-partial":
			allowPartial = true
		case "--root":
			if i+1 >= len(args) {
				return root, false, fmt.Errorf("--root requires a path")
			}
			// Reject a following flag token, so `watch --root --allow-partial`
			// fails loudly instead of silently treating "--allow-partial" as the
			// path (and then watching a directory literally named that).
			if strings.HasPrefix(args[i+1], "-") {
				return root, false, fmt.Errorf("--root requires a path, got flag %q", args[i+1])
			}
			abs, err := filepath.Abs(args[i+1])
			if err != nil {
				return root, false, err
			}
			root = abs
			i++
		default:
			return root, false, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return root, allowPartial, nil
}

func cmdWatch(root string, args []string) {
	root, allowPartial, err := parseWatchFlags(root, args)
	if err != nil {
		fatalf("%v\nusage: salvager watch [--root <path>] [--allow-partial]", err)
	}

	s := store.New(root)
	if err := s.Init(); err != nil {
		fatal(err)
	}
	w, err := watch.New(root, s, ignore.New(root))
	if err != nil {
		fatal(err)
	}
	w.SetAllowPartial(allowPartial)
	w.ProbeRacySlop() // widen the gate's racy window on coarse-mtime filesystems (best-effort)
	defer w.Close()

	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		close(done)
	}()

	fmt.Fprintf(os.Stderr, "salvager: watching %s (Ctrl-C to stop)\n", root)
	if err := w.Run(done); err != nil {
		fatal(err)
	}
}

func cmdHistory(root string, args []string) {
	if len(args) != 1 {
		fatalf("usage: salvager history <file>")
	}
	s := store.New(root)
	revs, err := s.List(rel(root, args[0]))
	if err != nil {
		fatal(err)
	}
	if len(revs) == 0 {
		fmt.Println("no history for", args[0])
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIMESTAMP\tHASH\tLABEL\tLINES\tΔLINES\tSTART")
	for _, r := range revs {
		lines, delta, start := "-", "-", ""
		if r.HasSignal {
			lines = strconv.Itoa(r.Lines)
			delta = r.DeltaString()
			start = firstLine(r.Sig)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			time.UnixMilli(r.Timestamp).Format("2006-01-02 15:04:05"),
			shortHash(r.Hash), r.Label, lines, delta, start)
	}
	tw.Flush()
	fmt.Fprintln(os.Stderr, "\nrestore with: salvager restore", args[0], "<timestamp-ms>")
	fmt.Fprintln(os.Stderr, "(timestamps below are human-readable; raw ms:)")
	for _, r := range revs {
		fmt.Fprintf(os.Stderr, "  %d  %s\n", r.Timestamp, r.Label)
	}
}

func cmdShow(root string, args []string) {
	if len(args) != 2 {
		fatalf("usage: salvager show <file> <timestamp>")
	}
	ts := parseTS(args[1])
	s := store.New(root)
	content, err := s.Get(rel(root, args[0]), ts)
	if err != nil {
		fatal(err)
	}
	os.Stdout.Write(content)
}

func cmdRestore(root string, args []string) {
	if len(args) != 2 {
		fatalf("usage: salvager restore <file> <timestamp>")
	}
	ts := parseTS(args[1])
	s := store.New(root)
	preTs, err := s.Restore(rel(root, args[0]), ts)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("restored %s to revision %d\n", args[0], ts)
	fmt.Printf("previous state saved as pre-restore revision %d (undo with: salvager restore %s %d)\n",
		preTs, args[0], preTs)
}

func cmdMCP(root string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := store.New(root)
	err := mcp.Serve(ctx, s)
	// A client disconnect (stdin EOF) or our own shutdown is a clean exit,
	// not a failure. The SDK reports the disconnect as a wrapped/plain EOF.
	if err != nil &&
		!errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) &&
		!strings.Contains(err.Error(), "EOF") {
		fatal(err)
	}
}

func cmdGC(root string, args []string) {
	maxAge := 7 * 24 * time.Hour
	maxBytes := int64(-1) // -1 == flag absent: skip size GC, behave exactly as before
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--max-age" && i+1 < len(args):
			d, err := parseMaxAge(args[i+1])
			if err != nil {
				fatal(err)
			}
			maxAge = d
			i++
		case args[i] == "--max-bytes" && i+1 < len(args):
			b, err := parseMaxBytes(args[i+1])
			if err != nil {
				fatal(err)
			}
			maxBytes = b
			i++
		default:
			fatalf("usage: salvager gc [--max-age 7d] [--max-bytes 500M]")
		}
	}
	s := store.New(root)

	// Compose age then size: prune by age first, then cap whatever survives.
	if err := s.GC(maxAge); err != nil {
		fatal(err)
	}
	fmt.Printf("gc: purged revisions older than %s\n", maxAge)

	if maxBytes < 0 {
		return
	}
	finalBytes, err := s.GCBySize(maxBytes)
	if err != nil {
		fatal(err)
	}
	// Two legitimate, distinct endings — the user needs to know which happened.
	if finalBytes <= maxBytes {
		fmt.Printf("gc: store within size budget of %s (now %s)\n",
			humanBytes(maxBytes), humanBytes(finalBytes))
	} else {
		fmt.Printf("gc: reached the P1/P2 floor at %s — cannot shrink below the %s budget "+
			"without dropping a file's last revision or orphaning a restore\n",
			humanBytes(finalBytes), humanBytes(maxBytes))
	}
}

// rel makes a user-supplied path relative to root, accepting either an
// already-relative path or an absolute one inside the project.
func rel(root, p string) string {
	if !strings.HasPrefix(p, "/") {
		return p
	}
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// parseMaxAge accepts Go durations plus a "<n>d" days shorthand.
func parseMaxAge(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// parseMaxBytes parses a byte budget with optional binary suffix K/M/G
// (KiB/MiB/GiB), consistent with the rest of the project reasoning in KiB. A
// bare number is bytes. Stdlib only; no new dependency.
func parseMaxBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	}
	num := s
	if mult != 1 {
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n > (1<<63-1)/mult {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return n * mult, nil
}

// humanBytes renders a byte count with a binary suffix for the gc summary.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func parseTS(s string) int64 {
	ts, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		fatalf("invalid timestamp %q (use the raw ms value from `salvager history`)", s)
	}
	return ts
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// firstLine returns the first line of a start signature, clamped, so the history
// table stays one row per revision even when the signature spans several lines.
func firstLine(sig string) string {
	if i := strings.IndexByte(sig, '\n'); i >= 0 {
		sig = sig[:i]
	}
	if len(sig) > 60 {
		sig = sig[:60]
	}
	return sig
}

func fatal(err error)           { fmt.Fprintln(os.Stderr, "salvager:", err); os.Exit(1) }
func fatalf(f string, a ...any) { fmt.Fprintf(os.Stderr, "salvager: "+f+"\n", a...); os.Exit(1) }
