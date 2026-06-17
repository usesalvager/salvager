package main

// launchd backend: a per-user LaunchAgent (no root). RunAtLoad + KeepAlive make
// it start now and restart on death/login/reboot. It runs while the user is
// logged in — that is the dev-machine model; a pre-login root LaunchDaemon is
// out of scope. Commands use the modern bootstrap/bootout/kickstart/print forms,
// not the deprecated load/unload.

import (
	"html"
	"os"
	"path/filepath"
	"strings"
)

type launchdManager struct{}

func (launchdManager) platform() string { return "launchd" }

func launchdLabel(env *serviceEnv) string {
	return "com.salvager.watch." + projectToken(env.root)
}

func launchdPlistPath(env *serviceEnv) string {
	return filepath.Join(env.home, "Library", "LaunchAgents", launchdLabel(env)+".plist")
}

func launchdLogPaths(env *serviceEnv) (outPath, errPath string) {
	dir := filepath.Join(env.home, "Library", "Logs", "salvager")
	tok := projectToken(env.root)
	return filepath.Join(dir, tok+".out.log"), filepath.Join(dir, tok+".err.log")
}

func launchdDomain(env *serviceEnv) string { return "gui/" + env.uid }
func launchdTarget(env *serviceEnv) string { return launchdDomain(env) + "/" + launchdLabel(env) }

// buildPlist renders the LaunchAgent. Paths and args are absolute; values are
// XML-escaped so a path with an ampersand or angle bracket can't corrupt it.
func buildPlist(env *serviceEnv) string {
	outPath, errPath := launchdLogPaths(env)
	var args strings.Builder
	args.WriteString("    <string>" + html.EscapeString(env.exe) + "</string>\n")
	for _, a := range watchArgs(env) {
		args.WriteString("    <string>" + html.EscapeString(a) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + html.EscapeString(launchdLabel(env)) + `</string>

    <key>ProgramArguments</key>
    <array>
` + args.String() + `    </array>

    <key>WorkingDirectory</key>
    <string>` + html.EscapeString(env.root) + `</string>

    <key>RunAtLoad</key>
    <true/>

    <key>KeepAlive</key>
    <true/>

    <key>StandardOutPath</key>
    <string>` + html.EscapeString(outPath) + `</string>

    <key>StandardErrorPath</key>
    <string>` + html.EscapeString(errPath) + `</string>
</dict>
</plist>
`
}

func (m launchdManager) install(env *serviceEnv) []pieceResult {
	label := launchdLabel(env)
	plist := launchdPlistPath(env)
	outPath, _ := launchdLogPaths(env)

	// Ensure the log and LaunchAgents directories exist before we write/load.
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return []pieceResult{{label: "service", ok: false, state: "could not create log dir", detail: err.Error()}}
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return []pieceResult{{label: "service", ok: false, state: "could not create LaunchAgents dir", detail: err.Error()}}
	}
	if err := os.WriteFile(plist, []byte(buildPlist(env)), 0o644); err != nil {
		return []pieceResult{{label: "service", ok: false, state: "could not write plist", detail: err.Error()}}
	}

	// bootstrap loads the agent. Its exit code is unreliable (errors if already
	// bootstrapped, and other transient conditions), so we do not trust it — the
	// post-load verify below is the source of truth. kickstart -k then guarantees
	// a fresh running instance whether bootstrap just loaded it or it was already
	// present.
	env.run("launchctl", "bootstrap", launchdDomain(env), plist)
	env.run("launchctl", "kickstart", "-k", launchdTarget(env))

	st := m.status(env)
	if !st.Running {
		return []pieceResult{{
			label:  "service",
			ok:     false,
			state:  "installed but not running",
			detail: "inspect the log: " + outPath + "  (or: launchctl print " + launchdTarget(env) + ")",
		}}
	}
	return []pieceResult{
		{label: "service", ok: true, state: "running (" + label + ")"},
		{label: "persistent", ok: true, state: "✓ survives reboot (restarts at login)"},
		{label: "logs", ok: true, state: outPath},
	}
}

func (m launchdManager) uninstall(env *serviceEnv) []pieceResult {
	plist := launchdPlistPath(env)
	_, plistErr := os.Stat(plist)
	loaded := m.status(env).Installed

	if plistErr != nil && os.IsNotExist(plistErr) && !loaded {
		return []pieceResult{{label: "service", ok: true, state: "not installed (nothing to remove)"}}
	}

	// bootout stops + unloads. "not loaded" is fine — we proceed to remove the
	// plist regardless.
	env.run("launchctl", "bootout", launchdTarget(env))

	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return []pieceResult{{label: "service", ok: false, state: "could not remove plist", detail: err.Error()}}
	}
	outPath, _ := launchdLogPaths(env)
	return []pieceResult{
		{label: "service", ok: true, state: "removed"},
		{label: "logs", ok: true, state: "left in place: " + outPath},
	}
}

func (m launchdManager) status(env *serviceEnv) serviceStatus {
	label := launchdLabel(env)
	outPath, _ := launchdLogPaths(env)
	st := serviceStatus{Platform: "launchd", Root: env.root, Unit: label, Logs: outPath}

	out, code := env.run("launchctl", "print", launchdTarget(env))
	loaded := code == 0 && strings.Contains(out, label)
	_, plistErr := os.Stat(launchdPlistPath(env))
	plistExists := plistErr == nil

	switch {
	case !loaded && !plistExists:
		st.State = stateNotInstalled
		st.Installed = false
		return st
	case loaded && launchdRunning(out):
		st.State = stateRunning
	case loaded:
		st.State = stateStopped
	default:
		// plist on disk but not loaded → installed, stopped.
		st.State = stateStopped
	}
	st.Installed = true
	st.Running = st.State == stateRunning
	// A loaded LaunchAgent is persistent across reboot by design (RunAtLoad at
	// login). We report persistence as soon as it is installed.
	st.Persistent = true
	return st
}

// launchdRunning reads the `state = running` line from `launchctl print`.
func launchdRunning(printOut string) bool {
	for _, line := range strings.Split(printOut, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "state") && strings.Contains(t, "=") {
			return strings.Contains(t, "running")
		}
	}
	return false
}
