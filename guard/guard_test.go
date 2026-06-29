package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// root used across the table. A fixed absolute path so containment is
// deterministic regardless of where the test runs.
const root = "/proj"

// TestClassify is the crux: many command variants, the most severe clause wins.
// The principle under test — deny only what the net cannot recover, checkpoint
// what it can, pass the rest.
func TestClassify(t *testing.T) {
	cases := []struct {
		cmd  string
		want Tier
	}{
		// --- Tier A: destruction escaping the watched tree -------------------
		{"rm -rf /", TierDeny},
		{"rm -rf ~", TierDeny},
		{"rm -rf ~/Documents", TierDeny},
		{"rm -rf $HOME", TierDeny},
		{"rm -rf ${HOME}/.ssh", TierDeny},
		{"rm -rf ../..", TierDeny},
		{"rm -rf ../sibling", TierDeny},
		{"rm ~/.bashrc", TierDeny}, // no -rf, still escapes → unrecoverable
		{"sudo rm -rf /", TierDeny},

		// --- Tier A: destroying the net itself -------------------------------
		{"rm -rf .salvager", TierDeny},
		{"rm -rf .salvager/objects", TierDeny},
		{"rm -rf .", TierDeny}, // the whole tree takes .salvager with it
		{"cat data > .salvager/objects/x", TierDeny},

		// --- Tier A: irreversible-beyond-the-filesystem ----------------------
		{"git push --force origin main", TierDeny},
		{"git push -f", TierDeny},
		{"git push --force-with-lease", TierDeny},
		{"dd if=/dev/zero of=disk.img", TierDeny},
		{"mkfs.ext4 /dev/sdb", TierDeny},
		{"shred -u secret.txt", TierDeny},
		{"truncate -s 0 /etc/hosts", TierDeny},
		{"sed -i s/a/b/ /etc/hosts", TierDeny},
		{"find / -delete", TierDeny},
		{"find ~ -delete", TierDeny},

		// --- Tier A via wrappers / substitution ------------------------------
		{"echo $(rm -rf ~)", TierDeny},
		{"echo `rm -rf /`", TierDeny},
		{"bash -c 'rm -rf /'", TierDeny},
		{"eval rm -rf ~", TierDeny},
		{"git reset --hard && rm -rf ~", TierDeny}, // most-severe clause wins

		// --- Tier B: destructive but recoverable inside the tree -------------
		{"rm -rf build", TierCheckpoint},
		{"rm -rf node_modules && npm ci", TierCheckpoint},
		{"rm a.txt b.txt", TierCheckpoint}, // bulk
		{"git reset --hard", TierCheckpoint},
		{"git reset --hard HEAD~3", TierCheckpoint},
		{"git clean -fd", TierCheckpoint},
		{"git clean -fdx", TierCheckpoint},
		{"git checkout -f", TierCheckpoint},
		{"git checkout -- .", TierCheckpoint},
		{"git checkout .", TierCheckpoint},
		{"git -C sub reset --hard", TierCheckpoint}, // global -C skipped
		{"git stash", TierCheckpoint},
		{"git stash pop", TierCheckpoint},
		{"sed -i s/a/b/g main.go", TierCheckpoint},
		{"find . -name *.tmp -delete", TierCheckpoint},
		{"truncate -s 0 log.txt", TierCheckpoint},
		{"grep -rl X . | xargs rm", TierCheckpoint},

		// --- Pass: benign, must never be blocked or noised up ----------------
		{"", TierPass},
		{"ls -la", TierPass},
		{"go build ./...", TierPass},
		{"rm file.txt", TierPass}, // single plain delete inside the tree: low signal
		{"git checkout main", TierPass},
		{"git checkout -b feature", TierPass},
		{"git reset --soft HEAD~1", TierPass},
		{"git clean -n", TierPass}, // dry-run
		{"git push origin main", TierPass},
		{"git stash list", TierPass},
		{"sed s/a/b/ main.go", TierPass}, // no -i: writes to stdout
		{"find . -name *.tmp", TierPass}, // no -delete
		{"echo hi > out.txt", TierPass},  // redirect inside the tree is recoverable
		{"echo \"rm -rf /\"", TierPass},  // the dangerous text is a quoted argument
	}
	for _, c := range cases {
		got := Classify(Request{Tool: "Bash", Command: c.cmd, Root: root}).Tier
		if got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestClassify_DenyCarriesActionableReason — a deny must tell the agent what to
// do instead, and a checkpoint must carry the restore-at hint. The deny/hint text
// is the feedback channel, not decoration.
func TestClassify_DenyCarriesActionableReason(t *testing.T) {
	d := Classify(Request{Tool: "Bash", Command: "rm -rf ~", Root: root})
	if d.Tier != TierDeny || d.Reason == "" {
		t.Fatalf("expected a deny with a reason, got %+v", d)
	}
	if !strings.Contains(d.Reason, "manually") {
		t.Errorf("deny reason should tell the agent to defer to the user: %q", d.Reason)
	}

	c := Classify(Request{Tool: "Bash", Command: "git reset --hard", Root: root})
	if c.Tier != TierCheckpoint || c.RecoveryHint == "" {
		t.Fatalf("expected a checkpoint with a recovery hint, got %+v", c)
	}
	if !strings.Contains(c.RecoveryHint, "restore-at") {
		t.Errorf("checkpoint hint should name restore-at: %q", c.RecoveryHint)
	}
}

// TestPathClass pins the Tier A/B discriminator for deletion directly.
func TestPathClass(t *testing.T) {
	cases := []struct {
		token string
		want  int
	}{
		{"build", pathInside},
		{"src/main.go", pathInside},
		{"/proj/sub", pathInside},
		{".salvager", pathNet},
		{".salvager/objects/x", pathNet},
		{".", pathRoot}, // the whole tree; rm -rf . takes the store with it
		{"/", pathOutside},
		{"~", pathOutside},
		{"$HOME", pathOutside},
		{"../x", pathOutside},
		{"/etc/passwd", pathOutside},
		{"-rf", pathOther}, // a flag, not a path
	}
	for _, c := range cases {
		if got := pathClass(c.token, root); got != c.want {
			t.Errorf("pathClass(%q) = %d, want %d", c.token, got, c.want)
		}
	}
	// With an empty root only clearly-external tokens are judged outside; a
	// relative token defaults to inside (never a false deny).
	if pathClass("build", "") != pathInside {
		t.Error("empty root: relative token should default to inside")
	}
	if pathClass("~", "") != pathOutside {
		t.Error("empty root: ~ should still be outside")
	}
}

// TestLogAttempt_Seismograph verifies the attempt log: one JSON line per Tier
// A/B event, 0600, command hashed not stored verbatim, Pass not logged.
func TestLogAttempt_Seismograph(t *testing.T) {
	orig := nowFunc
	nowFunc = func() int64 { return 1700000000000 }
	defer func() { nowFunc = orig }()

	dir := t.TempDir()
	cmd := "rm -rf ~/secret-token-xyz"
	d := Classify(Request{Tool: "Bash", Command: cmd, Root: dir})
	if d.Tier != TierDeny {
		t.Fatalf("setup: expected deny, got %v", d.Tier)
	}
	req := Request{Tool: "Bash", Command: cmd, Root: dir, Agent: "claude-code"}
	if err := LogAttempt(req, d); err != nil {
		t.Fatalf("LogAttempt: %v", err)
	}

	logPath := filepath.Join(dir, storeDir, "hook-log")
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat hook-log: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("hook-log perms = %o, want 600", perm)
	}

	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), "secret-token-xyz") {
		t.Error("the command must NOT be stored verbatim — it can hold secrets")
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	var e attemptEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if e.TS != 1700000000000 || e.Tier != "deny" || e.Tool != "Bash" ||
		e.Agent != "claude-code" || e.Matched != "escape" || e.CmdHash == "" {
		t.Errorf("log entry fields wrong: %+v", e)
	}

	// A second event appends rather than truncates.
	if err := LogAttempt(req, d); err != nil {
		t.Fatalf("LogAttempt #2: %v", err)
	}
	raw, _ = os.ReadFile(logPath)
	if got := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; got != 2 {
		t.Errorf("expected 2 appended lines, got %d", got)
	}

	// Pass is never logged.
	pdir := t.TempDir()
	if err := LogAttempt(Request{Tool: "Bash", Command: "ls", Root: pdir}, pass()); err != nil {
		t.Fatalf("LogAttempt(pass): %v", err)
	}
	if _, err := os.Stat(filepath.Join(pdir, storeDir, "hook-log")); !os.IsNotExist(err) {
		t.Error("a Pass decision must not create a hook-log")
	}
}
