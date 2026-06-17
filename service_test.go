package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordedRun is a fake serviceEnv.run: it records every command and answers
// from a responder. The responder may consult the (real, temp) filesystem so
// `status` naturally tracks what install/uninstall just wrote.
type recordedRun struct {
	calls     [][]string
	responder func(name string, args []string) (string, int)
}

func (r *recordedRun) run(name string, args ...string) (string, int) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.responder != nil {
		return r.responder(name, args)
	}
	return "", 0
}

func (r *recordedRun) sawArg(substr string) bool {
	for _, c := range r.calls {
		for _, a := range c {
			if strings.Contains(a, substr) {
				return true
			}
		}
	}
	return false
}

// newTestEnv builds a serviceEnv over a temp HOME with capturing buffers. The
// caller supplies the responder (and may close over the returned env for
// filesystem-aware answers via the env pointer it also gets).
func newServiceTestEnv(t *testing.T, goos string) (*serviceEnv, *recordedRun, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	root := t.TempDir()
	rec := &recordedRun{}
	var out, errBuf bytes.Buffer
	env := &serviceEnv{
		root:      root,
		home:      home,
		exe:       "/usr/local/bin/salvager",
		uid:       "501",
		user:      "tester",
		goos:      goos,
		stdout:    &out,
		stderr:    &errBuf,
		run:       rec.run,
		preflight: func(string) error { return nil },
		lookPath:  func(string) (string, error) { return "/usr/bin/systemctl", nil },
		getenv:    func(k string) string { return "/run/user/501" },
	}
	return env, rec, &out, &errBuf
}

// systemdInstalledResponder answers as a systemd box where the unit is "active"
// iff its file exists on disk, and lingering is off. This makes a single
// responder model the whole install lifecycle.
func systemdInstalledResponder(env *serviceEnv, linger bool) func(string, []string) (string, int) {
	return func(name string, args []string) (string, int) {
		switch {
		case name == "systemctl" && len(args) >= 2 && args[1] == "is-system-running":
			return "running", 0
		case name == "systemctl" && len(args) >= 2 && args[1] == "show":
			if _, err := os.Stat(systemdUnitPath(env)); err == nil {
				return "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nNRestarts=0\n", 0
			}
			return "LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nNRestarts=0\n", 0
		case name == "loginctl":
			if linger {
				return "Linger=yes\n", 0
			}
			return "Linger=no\n", 0
		default:
			return "", 0
		}
	}
}

// Test 1: preflight failure must NOT write a unit; must fail loud + actionable.
func TestInstallPreflightFailureDoesNotInstall(t *testing.T) {
	env, rec, _, errBuf := newServiceTestEnv(t, "linux")
	env.responderPreflightFail()
	env.run = rec.run
	rec.responder = systemdInstalledResponder(env, false)

	code := runServiceInstall(env, nil)
	if code == 0 {
		t.Fatalf("expected non-zero exit on preflight failure, got 0")
	}
	if _, err := os.Stat(systemdUnitPath(env)); !os.IsNotExist(err) {
		t.Fatalf("unit file must not be written when preflight fails")
	}
	msg := errBuf.String()
	for _, want := range []string{"preflight failed", "max_user_instances", "--allow-partial"} {
		if !strings.Contains(msg, want) {
			t.Errorf("actionable message missing %q; got:\n%s", want, msg)
		}
	}
	if rec.sawArg("enable") {
		t.Errorf("must not enable a unit after a failed preflight")
	}
}

// Test 1b: the REAL preflight guards install. Test 1 stubs preflight to isolate
// the install plumbing; this drives a genuine construction failure through the
// production defaultPreflight — root is a regular file, so store.Init's MkdirAll
// fails with ENOTDIR — proving the guard catches a real failure, not just that
// install honours an injected error.
func TestInstallRealPreflightGuards(t *testing.T) {
	// The production preflight itself must fail on a non-directory root.
	notDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := defaultPreflight(notDir); err == nil {
		t.Fatalf("defaultPreflight must fail when the root cannot host a store")
	}

	// And install wired to the real preflight refuses, writing no unit.
	env, rec, _, errBuf := newServiceTestEnv(t, "linux")
	env.root = notDir
	env.preflight = defaultPreflight // production preflight, NOT a stub
	rec.responder = systemdInstalledResponder(env, false)

	if code := runServiceInstall(env, nil); code == 0 {
		t.Fatalf("install must refuse when the real preflight fails")
	}
	if _, err := os.Stat(systemdUnitPath(env)); !os.IsNotExist(err) {
		t.Errorf("no unit may be written when the real preflight fails")
	}
	if !strings.Contains(errBuf.String(), "preflight failed") {
		t.Errorf("must surface the actionable failure; got:\n%s", errBuf.String())
	}
	if rec.sawArg("enable") {
		t.Errorf("must not enable a unit after a real preflight failure")
	}
}

// Test 2: install when already running is a no-op (no enable, no file rewrite).
func TestInstallIdempotentAlreadyRunning(t *testing.T) {
	env, rec, out, _ := newServiceTestEnv(t, "linux")
	rec.responder = func(name string, args []string) (string, int) {
		switch {
		case name == "systemctl" && len(args) >= 2 && args[1] == "is-system-running":
			return "running", 0
		case name == "systemctl" && len(args) >= 2 && args[1] == "show":
			return "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nNRestarts=0\n", 0
		case name == "loginctl":
			return "Linger=yes\n", 0
		default:
			return "", 0
		}
	}
	code := runServiceInstall(env, nil)
	if code != 0 {
		t.Fatalf("already-running install should exit 0, got %d", code)
	}
	if !strings.Contains(out.String(), "already running") {
		t.Errorf("expected 'already running'; got:\n%s", out.String())
	}
	if rec.sawArg("enable") {
		t.Errorf("no-op install must not call enable")
	}
	if _, err := os.Stat(systemdUnitPath(env)); !os.IsNotExist(err) {
		t.Errorf("no-op install must not write a unit file")
	}
}

// Test 3: uninstall when nothing is installed is a clean no-op.
func TestUninstallIdempotentNotInstalled(t *testing.T) {
	env, rec, out, _ := newServiceTestEnv(t, "linux")
	rec.responder = systemdInstalledResponder(env, false) // file absent → not installed
	code := runServiceUninstall(env, nil)
	if code != 0 {
		t.Fatalf("uninstall no-op should exit 0, got %d", code)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("expected 'not installed'; got:\n%s", out.String())
	}
}

// Test 4: linger-off → install reports running-but-NOT-persistent with the
// enable-linger instruction, never "persistent ✓"; uninstall never touches
// lingering (shared user state).
func TestInstallLingerOffHonesty(t *testing.T) {
	env, rec, out, _ := newServiceTestEnv(t, "linux")
	rec.responder = systemdInstalledResponder(env, false)

	if code := runServiceInstall(env, nil); code != 0 {
		t.Fatalf("install should succeed (running), got %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "NOT yet persistent") {
		t.Errorf("must report not-yet-persistent; got:\n%s", s)
	}
	if !strings.Contains(s, "enable-linger") {
		t.Errorf("must instruct enable-linger; got:\n%s", s)
	}
	if strings.Contains(s, "survives reboot") {
		t.Errorf("must NOT claim persistence when linger is off; got:\n%s", s)
	}

	// Uninstall must never enable or disable lingering.
	rec.calls = nil
	if code := runServiceUninstall(env, nil); code != 0 {
		t.Fatalf("uninstall should exit 0, got %d", code)
	}
	if rec.sawArg("enable-linger") || rec.sawArg("disable-linger") {
		t.Errorf("uninstall must never touch lingering state")
	}
}

// Test 5: --json shape — both a not-installed case and an installed/running case.
func TestStatusJSONShape(t *testing.T) {
	wantKeys := []string{"platform", "installed", "state", "running", "persistent", "root", "unit", "logs"}

	// not-installed
	env, rec, out, _ := newServiceTestEnv(t, "linux")
	rec.responder = systemdInstalledResponder(env, false)
	if code := runServiceStatus(env, []string{"--json"}); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("not-installed JSON missing key %q", k)
		}
	}
	if m["installed"] != false || m["state"] != string(stateNotInstalled) {
		t.Errorf("not-installed JSON wrong: installed=%v state=%v", m["installed"], m["state"])
	}

	// installed + running: write the unit so the responder reports active.
	env2, rec2, out2, _ := newServiceTestEnv(t, "linux")
	rec2.responder = systemdInstalledResponder(env2, true)
	if err := os.MkdirAll(filepath.Dir(systemdUnitPath(env2)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(systemdUnitPath(env2), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runServiceStatus(env2, []string{"--json"}); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var m2 map[string]any
	if err := json.Unmarshal(out2.Bytes(), &m2); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m2["installed"] != true || m2["running"] != true || m2["state"] != string(stateRunning) {
		t.Errorf("running JSON wrong: %+v", m2)
	}
	if m2["persistent"] != true {
		t.Errorf("running+linger JSON should be persistent; got %+v", m2)
	}
}

// Test 6: --root flag contract on watch (regression-proof).
func TestParseWatchFlags(t *testing.T) {
	cwd := "/cwd/project"

	// no flag → root unchanged, allowPartial false
	r, ap, err := parseWatchFlags(cwd, nil)
	if err != nil || r != cwd || ap {
		t.Errorf("no-flag: got (%q,%v,%v)", r, ap, err)
	}

	// --root <abs> → that abs path
	r, _, err = parseWatchFlags(cwd, []string{"--root", "/abs/elsewhere"})
	if err != nil || r != "/abs/elsewhere" {
		t.Errorf("--root abs: got (%q,%v)", r, err)
	}

	// --allow-partial alone
	if _, ap, _ := parseWatchFlags(cwd, []string{"--allow-partial"}); !ap {
		t.Errorf("--allow-partial not parsed")
	}

	// --root with no value → error
	if _, _, err := parseWatchFlags(cwd, []string{"--root"}); err == nil {
		t.Errorf("--root without value should error")
	}

	// service --root override threads into the token/unit name
	env, rec, _, _ := newServiceTestEnv(t, "linux")
	rec.responder = systemdInstalledResponder(env, false)
	override := t.TempDir()
	env.preflight = func(string) error { return nil }
	if code := runServiceInstall(env, []string{"--root", override}); code != 0 {
		t.Fatalf("install --root exit %d", code)
	}
	if env.root != override {
		t.Errorf("service --root did not override root: %q", env.root)
	}
}

// Test 7: no supported manager → graceful manual fallback, nothing written.
func TestInstallFallbackUnsupported(t *testing.T) {
	// unknown GOOS
	env, _, _, errBuf := newServiceTestEnv(t, "plan9")
	if code := runServiceInstall(env, nil); code == 0 {
		t.Errorf("unsupported platform should exit non-zero")
	}
	if !strings.Contains(errBuf.String(), "watch --root") {
		t.Errorf("fallback must print the manual watch command; got:\n%s", errBuf.String())
	}

	// linux with no systemctl on PATH
	env2, _, _, errBuf2 := newServiceTestEnv(t, "linux")
	env2.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if code := runServiceInstall(env2, nil); code == 0 {
		t.Errorf("linux without systemd should exit non-zero")
	}
	if !strings.Contains(errBuf2.String(), "watch --root") {
		t.Errorf("fallback must print the manual watch command; got:\n%s", errBuf2.String())
	}
}

// helper: make this env's preflight fail.
func (env *serviceEnv) responderPreflightFail() {
	env.preflight = func(string) error { return errors.New("too many open files") }
}

// launchdResponder answers as a macOS box: `launchctl print` reports the job as
// running iff the plist exists on disk.
func launchdResponder(env *serviceEnv) func(string, []string) (string, int) {
	return func(name string, args []string) (string, int) {
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			if _, err := os.Stat(launchdPlistPath(env)); err == nil {
				return launchdLabel(env) + " = {\n\tstate = running\n}\n", 0
			}
			return "Could not find service", 1
		}
		return "", 0
	}
}

// launchd path: install writes a plist, verifies running, and reports persistent
// by design; status --json reflects platform=launchd. Runs on any CI box because
// goos is injected.
func TestInstallLaunchdPath(t *testing.T) {
	env, rec, out, _ := newServiceTestEnv(t, "darwin")
	rec.responder = launchdResponder(env)

	if code := runServiceInstall(env, nil); code != 0 {
		t.Fatalf("launchd install should succeed, got %d", code)
	}
	if _, err := os.Stat(launchdPlistPath(env)); err != nil {
		t.Fatalf("plist must be written: %v", err)
	}
	plist, _ := os.ReadFile(launchdPlistPath(env))
	for _, want := range []string{env.exe, "--root", "RunAtLoad", "KeepAlive"} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist missing %q", want)
		}
	}
	if !strings.Contains(out.String(), "survives reboot") {
		t.Errorf("launchd should report persistence; got:\n%s", out.String())
	}
	if !rec.sawArg("bootstrap") || !rec.sawArg("kickstart") {
		t.Errorf("launchd install must bootstrap + kickstart; calls: %v", rec.calls)
	}

	// status --json
	out.Reset()
	if code := runServiceStatus(env, []string{"--json"}); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["platform"] != "launchd" || m["running"] != true || m["persistent"] != true {
		t.Errorf("launchd JSON wrong: %+v", m)
	}

	// uninstall removes the plist, leaves nothing behind.
	if code := runServiceUninstall(env, nil); code != 0 {
		t.Fatalf("launchd uninstall exit %d", code)
	}
	if _, err := os.Stat(launchdPlistPath(env)); !os.IsNotExist(err) {
		t.Errorf("plist must be removed on uninstall")
	}
	if !rec.sawArg("bootout") {
		t.Errorf("uninstall must bootout the job")
	}
}
