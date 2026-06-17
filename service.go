package main

// salvager service runs the watcher as a persistent per-project service so it
// survives terminal close and reboot. It is a dedicated noun-then-verb surface
// (install / uninstall / status), kept separate from `init`'s pure config-file
// reconciliation because it touches OS service managers and (on Linux) user
// lingering state.
//
// Two managers sit behind one small interface — launchd (macOS) and systemd
// user services (Linux) — selected by environment detection. Every OS call goes
// through the injectable serviceEnv.run / .preflight seams so the whole command
// is testable without a real launchd or systemd, exactly as init injects the
// `claude` CLI. The honesty bar from init carries over: never claim persistence
// we did not verify (launchd: running; systemd: running AND lingering), and
// never falsely report success.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/usesalvager/salvager/ignore"
	"github.com/usesalvager/salvager/store"
	"github.com/usesalvager/salvager/watch"
)

// serviceState is the resolved liveness of a unit. It is read from the manager,
// never inferred from the presence of a unit file.
type serviceState string

const (
	stateNotInstalled serviceState = "not-installed"
	stateRunning      serviceState = "running"
	stateStopped      serviceState = "stopped"
	stateFailed       serviceState = "failed"
	stateFlapping     serviceState = "flapping"
)

// serviceStatus is the stable shape reported by `service status` (and --json).
// Keys are always present so a consumer never has to branch on key existence.
type serviceStatus struct {
	Platform   string       `json:"platform"`   // "launchd" | "systemd" | ""
	Installed  bool         `json:"installed"`  //
	State      serviceState `json:"state"`      // running|stopped|failed|flapping|not-installed
	Running    bool         `json:"running"`    // convenience: State == running
	Persistent bool         `json:"persistent"` // launchd: installed; systemd: installed && linger
	Root       string       `json:"root"`       // watched tree (absolute)
	Unit       string       `json:"unit"`       // label / unit name
	Logs       string       `json:"logs"`       // log path or journal hint
}

// serviceManager is the OS-specific backend. Both implementations read every
// piece of external state through serviceEnv.run, so tests inject a fake runner.
type serviceManager interface {
	platform() string
	// install returns the report pieces plus the final running-state it polled
	// for (via awaitRunning), so the caller need not re-query the manager.
	install(env *serviceEnv) ([]pieceResult, serviceStatus)
	uninstall(env *serviceEnv) []pieceResult
	status(env *serviceEnv) serviceStatus
}

// serviceEnv carries everything the command touches. Production wires real OS
// calls; tests inject fakes (a temp HOME, a recording run func, a stub preflight)
// so CI needs neither launchd nor systemd. Mirrors initEnv.
type serviceEnv struct {
	root         string // watched project root (absolute)
	home         string // user home dir
	exe          string // selfExe(): absolute, symlink left unresolved (Homebrew)
	uid          string // numeric uid, for launchd's gui/<uid> domain
	user         string // $USER, for loginctl linger
	goos         string // runtime.GOOS, injectable so tests exercise both managers
	allowPartial bool   // thread --allow-partial into the unit's watch args

	stdout, stderr io.Writer

	// run executes an external command and returns combined output + exit code.
	run func(name string, args ...string) (string, int)
	// preflight verifies the watcher can construct on root (store init + backend),
	// with no side effects. Injectable so a test can force a construction failure.
	preflight func(root string) error
	lookPath  func(file string) (string, error)
	getenv    func(key string) string
	// sleep paces the post-install running-state poll. Injectable so tests
	// exercise the retry loop without real wall-clock delay.
	sleep func(d time.Duration)
}

// newServiceEnv wires the production environment.
func newServiceEnv(root string) *serviceEnv {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	exe, err := selfExe()
	if err != nil {
		fatal(err)
	}
	user := os.Getenv("USER")
	return &serviceEnv{
		root:      root,
		home:      home,
		exe:       exe,
		uid:       strconv.Itoa(os.Getuid()),
		user:      user,
		goos:      runtime.GOOS,
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		run:       defaultRun,
		preflight: defaultPreflight,
		lookPath:  exec.LookPath,
		getenv:    os.Getenv,
		sleep:     time.Sleep,
	}
}

// Post-install, the freshly kickstarted watcher can take a few seconds to
// settle before the service manager reports it "running": registering fsevents
// watches over a large tree is not instant, and launchd's minimum-runtime gate
// means an immediate check can race ahead of the daemon. Rather than
// false-reporting "installed but not running", we poll with a short grace
// window (~6s total) and only give up if it genuinely never comes up.
const (
	serviceStartRetries = 12
	serviceStartDelay   = 500 * time.Millisecond
)

// awaitRunning polls status until it reports Running, or the grace window is
// exhausted. status is the manager's own status func, so the seam stays
// injectable for tests.
func awaitRunning(env *serviceEnv, status func(*serviceEnv) serviceStatus) serviceStatus {
	st := status(env)
	for i := 0; i < serviceStartRetries && !st.Running; i++ {
		env.sleep(serviceStartDelay)
		st = status(env)
	}
	return st
}

// defaultRun invokes a real command, folding stderr into stdout for parsing,
// returning the exit code (non-zero is a normal signal, never an error here).
func defaultRun(name string, args ...string) (string, int) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return out.String(), code
}

// defaultPreflight runs the genuine startup checks with no side effects: the
// store must initialise and the OS watch backend must construct. It writes no
// revisions and starts no watch loop. These are the two failures that would
// otherwise crash-loop a freshly installed service.
func defaultPreflight(root string) error {
	s := store.New(root)
	if err := s.Init(); err != nil {
		return err
	}
	return watch.Preflight(root, s, ignore.New(root))
}

// cmdService dispatches the verb. Flat sub-switch, matching main.go's style.
func cmdService(root string, args []string) {
	if len(args) == 0 {
		fatalf("usage: salvager service install|uninstall|status [--json]")
	}
	env := newServiceEnv(root)
	verb, rest := args[0], args[1:]
	switch verb {
	case "install":
		os.Exit(runServiceInstall(env, rest))
	case "uninstall":
		os.Exit(runServiceUninstall(env, rest))
	case "status":
		os.Exit(runServiceStatus(env, rest))
	default:
		fatalf("usage: salvager service install|uninstall|status [--json]")
	}
}

// runServiceInstall does all install work and printing, returning an exit code.
// Kept os.Exit-free so tests can assert behaviour and the actionable message.
func runServiceInstall(env *serviceEnv, args []string) int {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--allow-partial":
			env.allowPartial = true
		case "--root":
			if i+1 >= len(args) {
				fmt.Fprintln(env.stderr, "usage: salvager service install [--root <path>] [--allow-partial]")
				return 2
			}
			if strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(env.stderr, "salvager: --root requires a path, got flag %q\n", args[i+1])
				return 2
			}
			abs, err := filepath.Abs(args[i+1])
			if err != nil {
				fmt.Fprintln(env.stderr, "salvager:", err)
				return 1
			}
			env.root = abs
			i++
		default:
			fmt.Fprintln(env.stderr, "usage: salvager service install [--root <path>] [--allow-partial]")
			return 2
		}
	}

	mgr, ok := selectManager(env)
	if !ok {
		printManualFallback(env)
		return 1
	}

	// Idempotency: already running → no-op. Query the manager, never assume.
	if st := mgr.status(env); st.Running {
		printServiceReport(env, "service install", []pieceResult{
			{label: "service", ok: true, state: "already running"},
		})
		return 0
	}

	// Fail-fast preflight BEFORE writing any unit. A descriptor/instance-starved
	// tree must not produce a crash-looping (launchd) or failed-silent (systemd)
	// service. Refuse to install; offer the two real outs.
	if err := env.preflight(env.root); err != nil {
		fmt.Fprintf(env.stderr, "salvager: preflight failed — not installing a service that would not stay up.\n  %v\n\n", err)
		fmt.Fprintln(env.stderr, "The watcher could not construct on this tree. Two options:")
		fmt.Fprintln(env.stderr, "  (a) raise the kernel watch limit, then retry:")
		fmt.Fprintln(env.stderr, "        sudo sysctl fs.inotify.max_user_instances=512")
		fmt.Fprintln(env.stderr, "  (b) accept degraded coverage explicitly (you must type the flag):")
		fmt.Fprintln(env.stderr, "        salvager service install --allow-partial")
		return 1
	}

	results, st := mgr.install(env)
	printServiceReport(env, "service install", results)
	if !st.Running {
		return 1
	}
	return 0
}

func runServiceUninstall(env *serviceEnv, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(env.stderr, "usage: salvager service uninstall")
		return 2
	}
	mgr, ok := selectManager(env)
	if !ok {
		printManualFallback(env)
		return 1
	}
	results := mgr.uninstall(env)
	printServiceReport(env, "service uninstall", results)
	// Exit code must match what we printed: a ✗ piece (e.g. the job could not be
	// removed) is a failure, not a success.
	if anyFailed(results) {
		return 1
	}
	return 0
}

func runServiceStatus(env *serviceEnv, args []string) int {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			fmt.Fprintln(env.stderr, "usage: salvager service status [--json]")
			return 2
		}
	}

	mgr, ok := selectManager(env)
	if !ok {
		st := serviceStatus{Platform: "", Installed: false, State: stateNotInstalled, Root: env.root}
		if asJSON {
			printJSON(env, st)
			return 0
		}
		fmt.Fprintf(env.stdout, "salvager service — %s\n\n  no supported service manager on this platform (%s)\n", env.root, env.goos)
		fmt.Fprintf(env.stdout, "  run the watcher manually:  %s\n", manualWatchCmd(env))
		return 0
	}

	st := mgr.status(env)
	if asJSON {
		printJSON(env, st)
		return 0
	}
	printStatusHuman(env, st)
	return 0
}

// selectManager chooses the OS manager from the (injectable) environment. Linux
// requires a usable systemd user bus; otherwise we fall back rather than write a
// unit no manager can run.
func selectManager(env *serviceEnv) (serviceManager, bool) {
	switch env.goos {
	case "darwin":
		return &launchdManager{}, true
	case "linux":
		if systemdUsable(env) {
			return &systemdManager{}, true
		}
	}
	return nil, false
}

// systemdUsable probes for a reachable `systemctl --user` bus. A degraded or
// maintenance system still has a bus (non-zero exit, but no connect failure);
// only a missing binary, missing XDG_RUNTIME_DIR, or an explicit bus-connect
// failure means user services are unavailable here.
func systemdUsable(env *serviceEnv) bool {
	if _, err := env.lookPath("systemctl"); err != nil {
		return false
	}
	if env.getenv("XDG_RUNTIME_DIR") == "" {
		return false
	}
	out, _ := env.run("systemctl", "--user", "is-system-running")
	return !strings.Contains(out, "Failed to connect")
}

// projectToken is a fixed-length, collision-free id for the watched root: a
// sanitized basename for human readability plus 8 hex of sha256(absRoot). It is
// a pure function of the absolute root, so uninstall and status recompute it
// without any stored state, and it dodges systemd-escape entirely.
func projectToken(absRoot string) string {
	sum := sha256.Sum256([]byte(absRoot))
	h := shortHash(hex.EncodeToString(sum[:]))
	base := sanitizeBase(filepath.Base(absRoot))
	if base == "" {
		return h
	}
	return base + "-" + h
}

// sanitizeBase lowercases and keeps [a-z0-9-], mapping separators to '-', then
// trims and clamps. Returns "" when nothing usable survives (caller uses the
// hash alone).
func sanitizeBase(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '_' || r == '.' || r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = strings.Trim(out[:24], "-")
	}
	return out
}

// watchArgs builds the `watch` argument vector a unit must invoke: absolute root
// always, --allow-partial only when the operator explicitly opted in.
func watchArgs(env *serviceEnv) []string {
	a := []string{"watch", "--root", env.root}
	if env.allowPartial {
		a = append(a, "--allow-partial")
	}
	return a
}

func manualWatchCmd(env *serviceEnv) string {
	return env.exe + " " + strings.Join(watchArgs(env), " ")
}

func printManualFallback(env *serviceEnv) {
	fmt.Fprintf(env.stderr, "salvager: no supported service manager here (%s) — not installing.\n", env.goos)
	fmt.Fprintf(env.stderr, "  run the watcher manually (e.g. from your shell profile or a terminal that stays open):\n")
	fmt.Fprintf(env.stderr, "    %s\n", manualWatchCmd(env))
}

// --- reporting -------------------------------------------------------------

// anyFailed reports whether any piece of a service report is a failure, so the
// command can set its exit code to match what it printed.
func anyFailed(results []pieceResult) bool {
	for _, r := range results {
		if !r.ok {
			return true
		}
	}
	return false
}

func printServiceReport(env *serviceEnv, verb string, results []pieceResult) {
	printPieceReport(env.stdout, verb, env.root, results)
}

// printPieceReport renders a reconciliation report: a header line, one line per
// piece (✓/✗ label state), then any pieces carrying extra detail. Shared by the
// `service` and `init` surfaces so the two render identically.
func printPieceReport(w io.Writer, verb, root string, results []pieceResult) {
	fmt.Fprintf(w, "salvager %s — %s\n\n", verb, root)
	var details []pieceResult
	for _, r := range results {
		mark := "✓"
		if !r.ok {
			mark = "✗"
		}
		fmt.Fprintf(w, "  %-12s %s %s\n", r.label, mark, r.state)
		if r.detail != "" {
			details = append(details, r)
		}
	}
	if len(details) > 0 {
		fmt.Fprintln(w)
		for _, r := range details {
			fmt.Fprintf(w, "  %s: %s\n", r.label, r.detail)
		}
	}
}

func printStatusHuman(env *serviceEnv, st serviceStatus) {
	w := env.stdout
	fmt.Fprintf(w, "salvager service — %s\n\n", env.root)
	if !st.Installed {
		fmt.Fprintf(w, "  not installed\n  install with: salvager service install\n")
		return
	}
	persistent := "✗ not yet — see below"
	if st.Persistent {
		persistent = "✓ survives reboot"
	}
	fmt.Fprintf(w, "  platform     %s\n", st.Platform)
	fmt.Fprintf(w, "  unit         %s\n", st.Unit)
	fmt.Fprintf(w, "  state        %s\n", st.State)
	fmt.Fprintf(w, "  watched root %s\n", st.Root)
	fmt.Fprintf(w, "  persistent   %s\n", persistent)
	fmt.Fprintf(w, "  logs         %s\n", st.Logs)
	if st.Platform == "systemd" && st.Installed && !st.Persistent {
		fmt.Fprintf(w, "\n  This service is running but will NOT survive logout/reboot until you enable\n")
		fmt.Fprintf(w, "  lingering for your user:\n      %s\n", lingerCommand(env.user))
		fmt.Fprintf(w, "  then confirm with: salvager service status\n")
	}
}

// lingerCommand is the single source for the enable-linger remediation, shared
// by the status output and the systemd install report so the two cannot drift.
func lingerCommand(user string) string {
	return fmt.Sprintf("loginctl enable-linger %q   (re-run with sudo if denied)", user)
}

func printJSON(env *serviceEnv, st serviceStatus) {
	enc := json.NewEncoder(env.stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
}
