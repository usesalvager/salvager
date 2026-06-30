package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProtected seeds <root>/.salvager/protected with the given body.
func writeProtected(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, storeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "protected"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProtectedHit_DefaultSet pins the built-in definitions: secrets and git
// internals are protected (source "default"), ordinary source files are not. This
// is the false-deny guard as much as the coverage check.
func TestProtectedHit_DefaultSet(t *testing.T) {
	cases := []struct {
		path string
		hit  bool
	}{
		{".env", true},
		{".env.local", true},
		{"config/.env", true},
		{"certs/server.pem", true},
		{"deploy/key.pfx", true},
		{"id_rsa", true},
		{".ssh/id_rsa", true},
		{"home/.ssh/config", true},
		{".aws/credentials", true},
		{"credentials", true},
		{".npmrc", true},
		{".git/config", true},
		{"sub/.git/HEAD", true},
		// false-deny guard: ordinary dev files must never be protected.
		{"src/app.go", false},
		{"README.md", false},
		{"main.go", false},
		{"pkg/env.go", false}, // not ".env"
		{".github/workflows/ci.yml", false},
		{".ssh", false}, // a bare dir, nothing beneath it
	}
	for _, c := range cases {
		hit, src := ProtectedHit(c.path, "/proj")
		if hit != c.hit {
			t.Errorf("ProtectedHit(%q) hit=%v, want %v", c.path, hit, c.hit)
		}
		if hit && src != "default" {
			t.Errorf("ProtectedHit(%q) source=%q, want default", c.path, src)
		}
	}
}

// TestProtectedHit_UserAdditions: a .salvager/protected glob protects a path the
// defaults don't, and the source is reported as "user".
func TestProtectedHit_UserAdditions(t *testing.T) {
	root := t.TempDir()
	writeProtected(t, root, "# secrets\nconfig/prod.yaml\n*.secret\n")

	for _, p := range []string{"config/prod.yaml", "data/tokens.secret"} {
		hit, src := ProtectedHit(p, root)
		if !hit || src != "user" {
			t.Errorf("ProtectedHit(%q) = (%v,%q), want (true,user)", p, hit, src)
		}
	}
	// A path matching neither defaults nor additions is not protected.
	if hit, _ := ProtectedHit("config/dev.yaml", root); hit {
		t.Errorf("config/dev.yaml must not be protected")
	}
}

// TestProtectedHit_Exclusions: a leading "!" un-protects a default or an addition,
// the antivirus-exception escape hatch that keeps protection from being the thing
// users disable wholesale.
func TestProtectedHit_Exclusions(t *testing.T) {
	root := t.TempDir()
	writeProtected(t, root, "!.env\nbuild/out.bin\n!build/out.bin\n")

	if hit, _ := ProtectedHit(".env", root); hit {
		t.Errorf("!.env should un-protect the default .env")
	}
	// addition then its own exclusion → not protected (last match wins).
	if hit, _ := ProtectedHit("build/out.bin", root); hit {
		t.Errorf("an addition followed by its !exclusion should not be protected")
	}
	// an unrelated default is still protected.
	if hit, src := ProtectedHit("id_rsa", root); !hit || src != "default" {
		t.Errorf("excluding .env must not weaken other defaults: id_rsa=(%v,%q)", hit, src)
	}
}

// TestProtectedHit_LayeringPrecedence: order decides, both directions. An exclusion
// followed by a re-addition re-protects; an addition followed by an exclusion frees.
func TestProtectedHit_LayeringPrecedence(t *testing.T) {
	root := t.TempDir()
	// exclude .env, then re-add it → protected (user), last match wins.
	writeProtected(t, root, "!.env\n.env\n")
	if hit, src := ProtectedHit(".env", root); !hit || src != "user" {
		t.Errorf("re-addition after exclusion should re-protect: (%v,%q)", hit, src)
	}

	// reverse order: add then exclude → free.
	writeProtected(t, root, ".env\n!.env\n")
	if hit, _ := ProtectedHit(".env", root); hit {
		t.Errorf("exclusion after addition should free the path")
	}
}

// TestProtectedHit_MalformedAndMissing: a missing or malformed protected file falls
// back to the defaults and never errors.
func TestProtectedHit_MalformedAndMissing(t *testing.T) {
	// Missing file: defaults still apply.
	root := t.TempDir()
	if hit, src := ProtectedHit(".env", root); !hit || src != "default" {
		t.Errorf("missing protected file should leave defaults: (%v,%q)", hit, src)
	}

	// Malformed line ("[" is a bad glob) is skipped; the good line still works.
	writeProtected(t, root, "[\nmy.secret\n")
	if hit, _ := ProtectedHit("[", root); hit {
		t.Errorf("a malformed glob line must be skipped, not matched")
	}
	if hit, src := ProtectedHit("my.secret", root); !hit || src != "user" {
		t.Errorf("a valid line after a malformed one must still apply: (%v,%q)", hit, src)
	}
}

// TestClassify_ProtectedWrites: the two wiring paths — a direct Edit/Write to a
// protected path, and a shell command (rm / sed -i / mv / redirect) targeting one —
// both yield a Tier A deny carrying a self-correctable, !-override reason.
func TestClassify_ProtectedWrites(t *testing.T) {
	root := t.TempDir()
	writeProtected(t, root, "config/prod.yaml\n")

	// Direct write/edit.
	for _, tool := range []string{"Edit", "Write"} {
		d := Classify(Request{Tool: tool, FilePath: ".env", Root: root})
		if d.Tier != TierDeny {
			t.Errorf("%s .env: tier=%v, want deny", tool, d.Tier)
		}
		if !strings.Contains(d.Reason, "protected") || !strings.Contains(d.Reason, "!") {
			t.Errorf("%s deny reason should be self-correctable: %q", tool, d.Reason)
		}
	}

	// Shell commands targeting a protected path.
	deny := []string{
		"rm -rf .env",
		"rm .env", // even a single plain delete of a protected path
		"sed -i s/a/b/ .env",
		"mv evil.txt .env",     // overwrite a protected dest
		"mv .env /tmp/stolen",  // move a protected source away
		"cat secrets > .npmrc", // redirect onto a protected path
		"rm .ssh/id_rsa",       // default tree pattern
		"rm config/prod.yaml",  // user addition
	}
	for _, cmd := range deny {
		if got := Classify(Request{Tool: "Bash", Command: cmd, Root: root}).Tier; got != TierDeny {
			t.Errorf("Classify(%q) = %v, want deny", cmd, got)
		}
	}

	// Reads and non-mutating uses of a protected path are NOT denied.
	allow := []string{
		"cat .env",          // a read
		"sed s/a/b/ .env",   // no -i: writes to stdout
		"grep TOKEN .env",   // a read
		"echo hi > out.txt", // redirect to an ordinary file
		"rm build/app.o",    // ordinary delete inside the tree
	}
	for _, cmd := range allow {
		if got := Classify(Request{Tool: "Bash", Command: cmd, Root: root}).Tier; got == TierDeny {
			t.Errorf("Classify(%q) = deny, want pass/checkpoint (must not over-deny)", cmd)
		}
	}

	// A non-protected direct write is a pass (the watcher covers its recovery).
	if got := Classify(Request{Tool: "Write", FilePath: "src/app.go", Root: root}).Tier; got != TierPass {
		t.Errorf("Write src/app.go = %v, want pass", got)
	}
}

// TestClassify_ProtectedExclusionAllowsWrite: a "!" exclusion in the user layer
// frees an otherwise-default-protected path for writing.
func TestClassify_ProtectedExclusionAllowsWrite(t *testing.T) {
	root := t.TempDir()
	writeProtected(t, root, "!.env\n")

	if d := Classify(Request{Tool: "Write", FilePath: ".env", Root: root}); d.Tier == TierDeny {
		t.Errorf("an excluded .env must be writable, got deny: %q", d.Reason)
	}
	if d := Classify(Request{Tool: "Bash", Command: "rm .env", Root: root}); d.Tier == TierDeny {
		t.Errorf("an excluded .env must be deletable, got deny: %q", d.Reason)
	}
}
