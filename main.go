// Command lochis is a filesystem-level local-history safety net for agents.
//
//	lochis watch                    start the watcher (runs until killed)
//	lochis history <file>           list recorded versions of a file
//	lochis show <file> <ts>         print the content of one version
//	lochis restore <file> <ts>      restore a file to a version (reversible)
//	lochis mcp                      start the MCP server (stdio)
//	lochis gc [--max-age 7d]        purge revisions older than the threshold
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

	"lochis/ignore"
	"lochis/mcp"
	"lochis/store"
	"lochis/watch"
)

const usage = `lochis — local history for agents

Usage:
  lochis watch                    start the watcher (runs until killed)
  lochis history <file>           list recorded versions of a file
  lochis show <file> <timestamp>  print the content of one version
  lochis restore <file> <ts>      restore a file to a version (reversible)
  lochis mcp                      start the MCP server (stdio)
  lochis gc [--max-age 7d]        purge revisions older than the threshold
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
	case "watch":
		cmdWatch(root)
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
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "lochis: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func cmdWatch(root string) {
	s := store.New(root)
	if err := s.Init(); err != nil {
		fatal(err)
	}
	w, err := watch.New(root, s, ignore.New(root))
	if err != nil {
		fatal(err)
	}
	defer w.Close()

	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		close(done)
	}()

	fmt.Fprintf(os.Stderr, "lochis: watching %s (Ctrl-C to stop)\n", root)
	if err := w.Run(done); err != nil {
		fatal(err)
	}
}

func cmdHistory(root string, args []string) {
	if len(args) != 1 {
		fatalf("usage: lochis history <file>")
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
	fmt.Fprintln(tw, "TIMESTAMP\tHASH\tLABEL")
	for _, r := range revs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			time.UnixMilli(r.Timestamp).Format("2006-01-02 15:04:05"),
			shortHash(r.Hash), r.Label)
	}
	tw.Flush()
	fmt.Fprintln(os.Stderr, "\nrestore with: lochis restore", args[0], "<timestamp-ms>")
	fmt.Fprintln(os.Stderr, "(timestamps below are human-readable; raw ms:)")
	for _, r := range revs {
		fmt.Fprintf(os.Stderr, "  %d  %s\n", r.Timestamp, r.Label)
	}
}

func cmdShow(root string, args []string) {
	if len(args) != 2 {
		fatalf("usage: lochis show <file> <timestamp>")
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
		fatalf("usage: lochis restore <file> <timestamp>")
	}
	ts := parseTS(args[1])
	s := store.New(root)
	preTs, err := s.Restore(rel(root, args[0]), ts)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("restored %s to revision %d\n", args[0], ts)
	fmt.Printf("previous state saved as pre-restore revision %d (undo with: lochis restore %s %d)\n",
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
	for i := 0; i < len(args); i++ {
		if args[i] == "--max-age" && i+1 < len(args) {
			d, err := parseMaxAge(args[i+1])
			if err != nil {
				fatal(err)
			}
			maxAge = d
			i++
		} else {
			fatalf("usage: lochis gc [--max-age 7d]")
		}
	}
	s := store.New(root)
	if err := s.GC(maxAge); err != nil {
		fatal(err)
	}
	fmt.Printf("gc: purged revisions older than %s\n", maxAge)
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

func parseTS(s string) int64 {
	ts, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		fatalf("invalid timestamp %q (use the raw ms value from `lochis history`)", s)
	}
	return ts
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func fatal(err error)           { fmt.Fprintln(os.Stderr, "lochis:", err); os.Exit(1) }
func fatalf(f string, a ...any) { fmt.Fprintf(os.Stderr, "lochis: "+f+"\n", a...); os.Exit(1) }
