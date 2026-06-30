package guard

// Protected paths — the prevention half of Salvager, complementing recovery.
//
// Recovery answers "you can get it back." Protection answers "it must never be
// touched." The two do not overlap: the watcher CANNOT recover a gitignored file
// (it never captured it), so for gitignored secrets — .env, private keys —
// *prevention is the only protection there is*. The default set below therefore
// prioritises exactly what recovery can't save, which is also what an agent doing
// dev work almost never legitimately rewrites — keeping false-denies near zero.
//
// Model = antivirus definitions: a built-in default set ("base definitions")
// shipped in the binary, layered with a user file (.salvager/protected) that both
// ADDS patterns and EXCLUDES (un-protects) with a leading "!", gitignore-style:
// last matching rule wins.

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// defaultProtected is the built-in base definition set: conservative globs aimed
// at the unrecoverable/sacred (secrets, credentials, git internals). Patterns with
// no "/" match a file's BASENAME at any depth; a trailing "/**" matches anything
// beneath a directory of that name at any depth; any other "/" pattern matches the
// full root-relative path. .salvager/** is deliberately ABSENT — it is already
// net-protected by P1 (pathNet); duplicating it here would be redundant.
//
// NOTE: fleet-tuned protected definitions are a future paid-feed layer. This slice
// is the seam where a richer, remotely-updated set would plug in — but nothing
// remote is loaded today; the user layer (.salvager/protected) is the only addition.
var defaultProtected = []string{
	".env", ".env.*", // dotenv secrets (gitignored — recovery can't save them)
	"*.pem", "*.key", "*.p12", "*.pfx", // private keys / cert bundles
	"id_rsa", "id_*", // ssh private keys
	"credentials",       // aws/gcloud-style credential files
	".npmrc", ".pypirc", // package-registry tokens
	".aws/**", ".ssh/**", // credential directories
	".git/**", // git internals
}

// ProtectedHit reports whether a write/delete to relPath (a path as it appears in
// a command or tool input, absolute or root-relative) is forbidden, and whether the
// matching rule came from the "default" set or the "user" layer. It is pure except
// for one cheap, failure-tolerant read of <root>/.salvager/protected; an absent,
// unreadable, or malformed file degrades to the defaults and never errors — so a
// caller in the agent's hot path can always fail open.
//
// Resolution is gitignore-style last-match-wins: defaults are applied first, then
// the user lines in order, so a user "!.env" can un-protect a default and a later
// re-addition can re-protect. The path is normalised to a root-relative slash path;
// a target outside the tree falls back to its basename so basename-globbed secrets
// (id_rsa, *.pem) are still caught.
func ProtectedHit(relPath, root string) (hit bool, source string) {
	rel := normalizeRel(relPath, root)
	if rel == "" || rel == "." {
		return false, ""
	}
	base := path.Base(rel)
	apply := func(rule, src string) {
		pat := rule
		neg := strings.HasPrefix(pat, "!")
		if neg {
			pat = pat[1:]
		}
		if pat == "" {
			return
		}
		if matchGlob(pat, rel, base) {
			if neg {
				hit, source = false, ""
			} else {
				hit, source = true, src
			}
		}
	}
	for _, p := range defaultProtected {
		apply(p, "default")
	}
	for _, p := range loadUserPatterns(root) {
		apply(p, "user")
	}
	return hit, source
}

// loadUserPatterns reads <root>/.salvager/protected: one glob per line, "#"
// comments and blank lines ignored, a leading "!" marks an exclusion. Order is
// preserved (precedence is positional). Missing/unreadable file → nil (defaults
// only); a malformed glob line is skipped, never fatal.
func loadUserPatterns(root string) []string {
	if root == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(root, storeDir, "protected"))
	if err != nil {
		return nil // no file (or unreadable) → defaults only
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		pat := strings.TrimPrefix(ln, "!")
		if pat == "" {
			continue
		}
		// Validate the glob; a bad pattern is skipped, never surfaced as an error.
		if _, err := path.Match(strings.TrimSuffix(pat, "/**"), ""); err != nil {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// matchGlob tests one (already !-stripped) pattern against a root-relative slash
// path and its basename. Slash-less patterns match the basename at any depth;
// "dir/**" matches anything beneath a dir of that name at any depth; any other
// pattern matches the full relative path (path.Match's * never crosses "/").
func matchGlob(pat, rel, base string) bool {
	if !strings.Contains(pat, "/") {
		ok, _ := path.Match(pat, base)
		return ok
	}
	if strings.HasSuffix(pat, "/**") {
		return matchTree(strings.TrimSuffix(pat, "/**"), rel)
	}
	ok, _ := path.Match(pat, rel)
	return ok
}

// matchTree reports whether prefix appears as a run of leading directory segments
// anywhere in rel with at least one segment beneath it — so ".ssh/**" protects
// ".ssh/id_rsa" and "home/.ssh/config" but not a bare ".ssh" (nothing under it).
func matchTree(prefix, rel string) bool {
	pre := strings.Split(prefix, "/")
	seg := strings.Split(rel, "/")
	for start := 0; start+len(pre) < len(seg); start++ { // strictly < : require content beneath
		if segEqual(seg[start:start+len(pre)], pre) {
			return true
		}
	}
	return false
}

func segEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalizeRel turns a command/tool path token into a root-relative slash path. A
// token outside the tree (or unresolvable against an empty root) falls back to its
// basename, so basename-globbed secrets are still matched even from outside.
func normalizeRel(p, root string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if root == "" {
		return filepath.ToSlash(p)
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(filepath.Base(p)) // outside the tree → basename only
	}
	return filepath.ToSlash(rel)
}

// denyProtected is the Tier A verdict for a write/delete to a protected path. The
// reason is self-correctable: it names where the rule came from and how to override
// it (an antivirus-style "!" exception), so the agent can defer to the user or
// undo the protection without a human round-trip.
func denyProtected(target, source string) Decision {
	origin := "your .salvager/protected"
	if source == "default" {
		origin = "a Salvager default"
	}
	return Decision{
		Tier:           TierDeny,
		MatchedPattern: "protected-" + source,
		Reason: "`" + target + "` is a protected path (" + origin + "); refused. " +
			"If this is intended, ask the user, or add `!" + target + "` to .salvager/protected to override.",
	}
}

// protectedDeny is the shared check the command classifiers fold in alongside their
// existing inside/outside/net containment check: a protected target is a Tier A
// deny. The deny displays the root-relative path so the suggested `!` override is
// copy-pasteable straight into .salvager/protected (an absolute path is not a glob).
func protectedDeny(target, root string) (Decision, bool) {
	if hit, src := ProtectedHit(target, root); hit {
		return denyProtected(normalizeRel(target, root), src), true
	}
	return Decision{}, false
}
