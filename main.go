// Command salvager is a filesystem-level local-history safety net for agents.
//
//	salvager init                     connect this project's agent to salvager
//	salvager watch [--root <path>]    start the watcher (runs until killed)
//	salvager service install|uninstall|status
//	                                  run the watcher as a persistent service
//	salvager history <file>           list recorded versions of a file
//	salvager show <file> <ts>         print the content of one version
//	salvager restore <file> <ts>      restore a file to a version (reversible)
//	salvager restore-at <ts> [path]   restore a set of files to a point in time
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
  salvager restore-at <ts> [path]   restore a set of files to a point in time
  salvager restore-at --undo        revert the last restore-at batch
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
	case "restore-at":
		cmdRestoreAt(root, args)
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

// lastRestoreAtFile holds the window of the most recent restore-at batch so
// `restore-at --undo` can target exactly it. It is internal state, not user
// config, and lives inside .salvager/ — already outside the watched set.
func lastRestoreAtFile(root string) string {
	return filepath.Join(root, store.Dir, "last-restore-at")
}

func cmdRestoreAt(root string, args []string) {
	if len(args) == 1 && args[0] == "--undo" {
		cmdRestoreAtUndo(root)
		return
	}
	if len(args) < 1 || len(args) > 2 {
		fatalf("usage: salvager restore-at <timestamp-ms> [path]\n" +
			"       salvager restore-at --undo")
	}
	ts := parseTS(args[0])
	relDir := ""
	if len(args) == 2 {
		relDir = rel(root, args[1])
	}

	s := store.New(root)
	batchStart, batchEnd, results, err := s.RestoreAt(relDir, ts)
	// Print whatever the batch reached even on a mid-batch failure: the partial
	// results name the files already rewritten (each still reversible).
	printRestoreAtSummary(results, ts)

	restored := 0
	for _, r := range results {
		if r.Action == store.ActionRestored {
			restored++
		}
	}
	// Persist the batch window BEFORE failing, so `restore-at --undo` can revert
	// exactly what was rewritten — including a partial batch left by a mid-batch
	// failure. A write failure here is non-fatal: the restore already happened.
	if restored > 0 {
		data := fmt.Sprintf("%d\t%d\t%s\n", batchStart, batchEnd, relDir)
		if werr := os.WriteFile(lastRestoreAtFile(root), []byte(data), 0o600); werr != nil {
			fmt.Fprintln(os.Stderr, "salvager: warning: could not save undo state:", werr)
		}
	}
	if err != nil {
		fatal(err) // the partial batch persisted above is revertible with: salvager restore-at --undo
	}
	if restored == 0 {
		fmt.Println("✓ nothing to restore — no file differed from its state at that time.")
		return
	}
	fmt.Printf("✓ %d file(s) restored.  Undo this batch:  salvager restore-at --undo\n", restored)
}

func cmdRestoreAtUndo(root string) {
	b, err := os.ReadFile(lastRestoreAtFile(root))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no restore-at batch to undo")
			return
		}
		fatal(err)
	}
	// Tab-separated: "<batchStart>\t<batchEnd>\t<relDir>". relDir may be empty (the
	// whole tree) and may contain spaces, so it is the untouched remainder after the
	// second tab. ponytail: a relDir containing a tab or newline would break this —
	// pathological for a project path; upgrade to a length-prefixed encoding if ever needed.
	line := strings.TrimRight(string(b), "\n")
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 2 {
		fatalf("corrupt undo state in %s", lastRestoreAtFile(root))
	}
	start, err1 := strconv.ParseInt(parts[0], 10, 64)
	end, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		fatalf("corrupt undo state in %s", lastRestoreAtFile(root))
	}
	relDir := ""
	if len(parts) == 3 {
		relDir = parts[2]
	}

	s := store.New(root)
	results, err := s.UndoRestoreAt(relDir, start, end)
	printUndoSummary(results)
	if err != nil {
		fatal(err)
	}
	if len(results) == 0 {
		fmt.Println("✓ nothing to undo — the last batch left no reversible files.")
		return
	}
	// One-shot: drop the window so a second --undo is a friendly no-op (the undo
	// itself is reversible per file via the pre-restore timestamps printed above).
	_ = os.Remove(lastRestoreAtFile(root))
	fmt.Printf("✓ reverted %d file(s) to their pre-batch state.\n", len(results))
}

// printResultTable renders one row per file: path, the human time of the revision
// it landed on (so a GC-shifted target, or the pre-restore an undo reverts to, is
// visible), and the action. whenLabel names the middle column.
func printResultTable(results []store.RestoreResult, whenLabel string) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "PATH\t%s\tACTION\n", whenLabel)
	for _, r := range results {
		when := "-"
		if r.RestoredToTs != 0 {
			when = time.UnixMilli(r.RestoredToTs).Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Path, when, r.Action)
	}
	tw.Flush()
}

func printRestoreAtSummary(results []store.RestoreResult, atMs int64) {
	if len(results) == 0 {
		return
	}
	fmt.Printf("Restoring files to their state at or before %s…\n",
		time.UnixMilli(atMs).Format("2006-01-02 15:04:05"))
	printResultTable(results, "RESTORED TO")
}

func printUndoSummary(results []store.RestoreResult) {
	if len(results) == 0 {
		return
	}
	printResultTable(results, "REVERTED TO")
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
