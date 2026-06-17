package main

// systemd user-service backend: a unit in ~/.config/systemd/user/, managed with
// `systemctl --user` (no root for the unit itself). Logs go to the journal.
//
// THE LINGER GOTCHA: a user manager is normally killed at logout, so the service
// does NOT survive reboot unless lingering is enabled (loginctl enable-linger).
// We never enable it ourselves (it may need sudo and is shared user state) — we
// detect it and report honestly, and we gate the "survives reboot" claim on an
// observed Linger=yes. systemd also gives up after a restart burst and leaves a
// bad unit `failed` and silent, which is why install verifies it is actually
// active rather than trusting `enable --now`'s exit code.

import (
	"os"
	"path/filepath"
	"strings"
)

type systemdManager struct{}

func (systemdManager) platform() string { return "systemd" }

func systemdUnitName(env *serviceEnv) string {
	return "salvager-" + projectToken(env.root) + ".service"
}

func systemdUnitPath(env *serviceEnv) string {
	return filepath.Join(env.home, ".config", "systemd", "user", systemdUnitName(env))
}

func systemdJournalHint(env *serviceEnv) string {
	return "journalctl --user -u " + systemdUnitName(env)
}

// buildUnit renders the user service. ExecStart carries the absolute binary and
// the absolute --root, so the watched tree is visible in `systemctl status`.
// Restart=on-failure keeps it up across crashes (bounded by systemd's burst
// limit — see the package note).
func buildUnit(env *serviceEnv) string {
	exec := env.exe + " " + strings.Join(watchArgs(env), " ")
	return `[Unit]
Description=Salvager watcher (` + env.root + `)
After=default.target

[Service]
Type=simple
ExecStart=` + exec + `
WorkingDirectory=` + env.root + `
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`
}

func (m systemdManager) install(env *serviceEnv) []pieceResult {
	unit := systemdUnitName(env)
	path := systemdUnitPath(env)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return []pieceResult{{label: "service", ok: false, state: "could not create unit dir", detail: err.Error()}}
	}
	if err := os.WriteFile(path, []byte(buildUnit(env)), 0o644); err != nil {
		return []pieceResult{{label: "service", ok: false, state: "could not write unit", detail: err.Error()}}
	}

	env.run("systemctl", "--user", "daemon-reload")
	if _, code := env.run("systemctl", "--user", "enable", "--now", unit); code != 0 {
		return []pieceResult{{
			label:  "service",
			ok:     false,
			state:  "could not enable",
			detail: "inspect: " + systemdJournalHint(env),
		}}
	}

	st := m.status(env)
	results := []pieceResult{}
	if !st.Running {
		return []pieceResult{{
			label:  "service",
			ok:     false,
			state:  "enabled but not running",
			detail: "inspect: " + systemdJournalHint(env),
		}}
	}
	results = append(results, pieceResult{label: "service", ok: true, state: "running (" + unit + ")"})

	// Linger honesty: enabled+running now is NOT the same as surviving reboot.
	if st.Persistent {
		results = append(results, pieceResult{label: "persistent", ok: true, state: "✓ survives reboot (lingering enabled)"})
	} else {
		results = append(results, pieceResult{
			label: "persistent",
			ok:    false,
			state: "running, but NOT yet persistent",
			detail: "this will not survive logout/reboot until lingering is enabled:\n" +
				"        loginctl enable-linger \"" + env.user + "\"   (re-run with sudo if denied)\n" +
				"      then confirm with: salvager service status",
		})
	}
	results = append(results, pieceResult{label: "logs", ok: true, state: systemdJournalHint(env)})
	return results
}

func (m systemdManager) uninstall(env *serviceEnv) []pieceResult {
	unit := systemdUnitName(env)
	path := systemdUnitPath(env)
	_, statErr := os.Stat(path)

	if statErr != nil && os.IsNotExist(statErr) && !m.status(env).Installed {
		return []pieceResult{{label: "service", ok: true, state: "not installed (nothing to remove)"}}
	}

	// disable --now BEFORE removing the file: this stops the unit and removes the
	// default.target.wants/ symlink. Deleting the file first would orphan that
	// symlink. We deliberately do NOT touch lingering — it is shared user state
	// that another project's service may rely on.
	env.run("systemctl", "--user", "disable", "--now", unit)

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return []pieceResult{{label: "service", ok: false, state: "could not remove unit", detail: err.Error()}}
	}
	env.run("systemctl", "--user", "daemon-reload")
	return []pieceResult{{label: "service", ok: true, state: "removed"}}
}

func (m systemdManager) status(env *serviceEnv) serviceStatus {
	unit := systemdUnitName(env)
	st := serviceStatus{Platform: "systemd", Root: env.root, Unit: unit, Logs: systemdJournalHint(env)}

	out, _ := env.run("systemctl", "--user", "show", unit,
		"-p", "LoadState,ActiveState,SubState,Result,NRestarts")
	props := parseSystemdShow(out)

	_, statErr := os.Stat(systemdUnitPath(env))
	fileExists := statErr == nil
	loadState := props["LoadState"]

	if (loadState == "not-found" || loadState == "") && !fileExists {
		st.State = stateNotInstalled
		st.Installed = false
		return st
	}
	st.Installed = true
	st.State = systemdState(props)
	st.Running = st.State == stateRunning
	st.Persistent = st.Installed && systemdLinger(env)
	return st
}

// systemdState maps the show properties to our liveness enum. A unit caught in
// the restart burst (auto-restart with restarts logged) is flapping, not
// healthy; one systemd has given up on is failed; active is running.
func systemdState(p map[string]string) serviceState {
	switch p["ActiveState"] {
	case "active":
		return stateRunning
	case "failed":
		return stateFailed
	case "activating":
		if p["SubState"] == "auto-restart" || p["NRestarts"] != "" && p["NRestarts"] != "0" {
			return stateFlapping
		}
		return stateStopped
	default:
		if p["NRestarts"] != "" && p["NRestarts"] != "0" && p["Result"] != "success" {
			return stateFlapping
		}
		return stateStopped
	}
}

func parseSystemdShow(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// systemdLinger reads whether lingering is on for the user. Any failure (no
// loginctl, no user record) is read as "no" — we never over-claim persistence.
func systemdLinger(env *serviceEnv) bool {
	out, code := env.run("loginctl", "show-user", env.user, "--property=Linger")
	if code != 0 {
		return false
	}
	return strings.Contains(out, "Linger=yes")
}
