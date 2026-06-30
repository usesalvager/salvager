// Package guard is Salvager's agent-agnostic interception brain.
//
// It classifies an about-to-run tool call into one of three tiers and records a
// local "seismograph" of every non-trivial attempt. It knows NOTHING about any
// specific agent's interception protocol (no Claude Code PreToolUse JSON, no
// settings files) — a per-agent adapter (see ../hook.go for the Claude Code one)
// translates that protocol to/from this package. Keeping the brain portable means
// a second agent is a thin adapter over this same, already-tested core.
//
// The tiering principle is the whole product, stated once and encoded below:
//
//	The line between deny and checkpoint is what Salvager can vs cannot recover.
//
//	  Tier A (deny)       — damage the file-history net CANNOT undo: destruction
//	                        reaching outside the watched tree, destroying the net
//	                        itself (.salvager/), or an irreversible-beyond-the-
//	                        filesystem write (force-push, dd, mkfs, shred).
//	  Tier B (checkpoint) — destructive but recoverable WITHIN the tree (git reset
//	                        --hard, git clean -fd, bulk sed -i / find -delete). Let
//	                        it proceed, but hand the agent the restore-at instant.
//	  Pass                — everything else, fast and silent.
//
// Never block something the net could have undone anyway; only wall off what it
// cannot save. A missed dangerous command is still recoverable by the watcher; a
// false deny erodes trust — so the classifier favours precision over coverage.
//
// Honest limit: this classifier parses SHELL file-ops, not arbitrary interpreter
// code. An inline write through a language runtime — python -c "open('.env','w')…",
// node -e "fs.writeFileSync('.env',…)", perl -e … — is opaque to a shell parser and
// passes. Defence-in-depth still covers it: the watcher recovers anything the
// interpreter writes to a non-gitignored file. The one thing genuinely unprotected at
// this layer is a *gitignored secret* written by an interpreter (recovery never saw
// it, and the shell parser can't see the write). We do not pretend to close that gap.
package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tier is the severity of a classified request. Ordered so the most severe
// clause of a compound command wins by a simple `>` comparison.
type Tier int

const (
	TierPass       Tier = iota // allow, silently
	TierCheckpoint             // Tier B: allow, but ensure/announce a recovery point
	TierDeny                   // Tier A: block — the net cannot recover this
)

func (t Tier) String() string {
	switch t {
	case TierCheckpoint:
		return "checkpoint"
	case TierDeny:
		return "deny"
	default:
		return "pass"
	}
}

// Request is one normalized about-to-run tool call. Agent-agnostic on purpose:
// an adapter fills it from whatever protocol its agent speaks.
type Request struct {
	Tool     string // the tool name, e.g. "Bash"
	Command  string // for Bash: the shell command line
	FilePath string // for Edit/Write: the path the tool would write — checked against protected paths
	Root     string // the project tree the agent is operating in (cwd)
	Agent    string // who saw it, e.g. "claude-code" — recorded by the seismograph
}

// Decision is the classification result. Reason is filled for a deny (it tells
// the agent what to do instead); RecoveryHint is filled for a checkpoint (the
// restore-at instant); MatchedPattern is a short stable id of the rule that fired.
type Decision struct {
	Tier           Tier
	Reason         string
	RecoveryHint   string
	MatchedPattern string
}

// nowFunc is the clock, overridable in tests. Milliseconds since epoch, matching
// store revision timestamps so a RecoveryHint's ms lines up with `restore-at`.
var nowFunc = func() int64 { return time.Now().UnixMilli() }

// storeDir mirrors store.Dir (".salvager"). Hardcoded rather than imported so the
// brain carries no dependency on the rest of salvager; the value is stable and a
// drift would be caught by the seismograph test writing under it.
const storeDir = ".salvager"

// Classify is the fast, near-pure heart. It splits a compound command into
// independently-judged clauses and returns the most severe verdict. Its only I/O is
// one cheap, failure-tolerant read of .salvager/protected (via ProtectedHit) — it
// still cannot fail, so the adapter can always fail-open, and it runs in the agent's
// hot path in microseconds. The seismograph (LogAttempt) is the only other side
// effect, kept separate so a logging error never changes the verdict.
func Classify(req Request) Decision {
	// Direct write/edit (Edit/Write tools): a protected target is a Tier A deny. A
	// non-protected write is a pass — the watcher covers its recovery.
	if req.FilePath != "" {
		if d, ok := protectedDeny(req.FilePath, req.Root); ok {
			return d
		}
	}
	best := Decision{Tier: TierPass}
	for _, clause := range splitClauses(req.Command) {
		if d := classifyClause(clause, req.Root); d.Tier > best.Tier {
			best = d
			if best.Tier == TierDeny {
				break // nothing can outrank a deny
			}
		}
	}
	return best
}

// --- command splitting -----------------------------------------------------

// splitClauses breaks a shell command into independently-classified clauses. It
// splits on unquoted ; && || | and newlines, and additionally lifts the inner
// text of $(...) and `...` substitutions and of `sh -c "..."`/`eval "..."` out as
// their own clauses (handled in classifyClause), so `echo $(rm -rf ~)` is judged
// by its rm, not its echo. Conservative by construction: it executes nothing and,
// when unsure about quoting, errs toward MORE clauses (more chances to catch a
// dangerous one), never fewer.
func splitClauses(cmd string) []string {
	outer, subs := liftSubstitutions(cmd)
	clauses := splitTop(outer)
	for _, s := range subs {
		clauses = append(clauses, splitClauses(s)...) // recurse: shrinking input bounds it
	}
	return clauses
}

// liftSubstitutions removes $(...) and `...` regions from cmd, returning the
// stripped outer string plus each inner command string for separate splitting.
func liftSubstitutions(cmd string) (outer string, subs []string) {
	var b strings.Builder
	var q byte // 0, '\'' or '"'
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if q != 0 {
			b.WriteByte(c)
			if c == q {
				q = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			q = c
			b.WriteByte(c)
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			inner, end := matchParen(cmd, i+2)
			subs = append(subs, inner)
			b.WriteByte(' ')
			i = end
		case c == '`':
			if j := strings.IndexByte(cmd[i+1:], '`'); j >= 0 {
				subs = append(subs, cmd[i+1:i+1+j])
				b.WriteByte(' ')
				i = i + 1 + j
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), subs
}

// matchParen returns the text inside a $(...) starting at start (just past the
// open paren) and the index of the matching ')'. Nested parens are balanced; an
// unbalanced open runs to end of string (still classified, conservatively).
func matchParen(s string, start int) (inner string, end int) {
	depth := 1
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start:i], i
			}
		}
	}
	return s[start:], len(s) - 1
}

// splitTop splits on unquoted separators ; && || | and newline. A `|` immediately
// after a `>` is the `>|` clobber-override redirect operator, not a pipe — it stays
// in the clause so redirectTargets can judge its file target.
func splitTop(s string) []string {
	var out []string
	var b strings.Builder
	var q byte
	flush := func() {
		if t := strings.TrimSpace(b.String()); t != "" {
			out = append(out, t)
		}
		b.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			b.WriteByte(c)
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			q = c
			b.WriteByte(c)
		case ';', '\n':
			flush()
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				i++
			}
			flush()
		case '|':
			if i > 0 && s[i-1] == '>' {
				b.WriteByte(c) // `>|` clobber-override redirect, not a pipe
				continue
			}
			if i+1 < len(s) && s[i+1] == '|' {
				i++
			}
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}

// tokenize splits one clause into words on unquoted whitespace, stripping the
// surrounding quote characters but keeping their contents. Minimal and
// conservative: it does not expand variables or process escapes, so a quoted
// `"$HOME"` stays the token `$HOME` and is still judged as the user's home —
// over-denying a quoted home path is acceptable; under-denying it is not.
func tokenize(clause string) []string {
	var out []string
	var b strings.Builder
	var q byte
	started := false
	for i := 0; i < len(clause); i++ {
		c := clause[i]
		if q != 0 {
			if c == q {
				q = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			q = c
			started = true
		case ' ', '\t':
			if started {
				out = append(out, b.String())
				b.Reset()
				started = false
			}
		default:
			b.WriteByte(c)
			started = true
		}
	}
	if started {
		out = append(out, b.String())
	}
	return out
}

// --- clause classification -------------------------------------------------

func classifyClause(clause, root string) Decision {
	toks := tokenize(clause)
	if len(toks) == 0 {
		return pass()
	}

	// A redirection to the net or outside the tree is destruction the watcher
	// can't recover, whatever the command is — as is one onto a protected path.
	for _, tgt := range redirectTargets(toks) {
		if d, ok := protectedDeny(tgt, root); ok {
			return d
		}
		switch pathClass(tgt, root) {
		case pathNet:
			return denyNet("redirect", tgt)
		case pathOutside:
			return denyOutside("redirect", tgt)
		}
	}

	cmd, args := realCommand(toks)
	base := filepath.Base(cmd)

	// Shell/eval wrappers: re-classify their inner command so `bash -c "rm -rf ~"`
	// is judged by the rm, not by bash.
	if isShell(base) {
		if inner, ok := dashCArg(args); ok {
			return Classify(Request{Command: inner, Root: root})
		}
	}
	if base == "eval" {
		return Classify(Request{Command: strings.Join(args, " "), Root: root})
	}

	switch base {
	case "rm":
		return classifyRm(args, root)
	case "git":
		return classifyGit(args)
	case "sed":
		return classifySed(args, root)
	case "mv":
		return classifyMv(args, root)
	case "tee":
		return classifyTee(args, root)
	case "cp":
		return classifyCp("cp", args, root)
	case "install":
		return classifyCp("install", args, root)
	case "ln":
		return classifyLn(args, root)
	case "find":
		return classifyFind(args, root)
	case "truncate":
		return classifyTruncate(args, root)
	case "dd":
		return denyIrreversible("dd")
	case "shred":
		return denyIrreversible("shred")
	case "xargs":
		if argsContain(args, "rm") {
			return checkpoint("xargs-rm")
		}
		return pass()
	}
	if strings.HasPrefix(base, "mkfs") {
		return denyIrreversible(base)
	}
	return pass()
}

// realCommand strips leading wrappers (sudo, env, VAR=val assignments, nice, …)
// to find the actual command and its arguments, so `sudo rm -rf /` is judged as
// rm, not sudo.
func realCommand(toks []string) (string, []string) {
	i := 0
	for i < len(toks) {
		t := toks[i]
		switch t {
		case "sudo", "command", "nice", "nohup", "time", "builtin", "exec", "doas":
			i++
			continue
		case "env":
			i++
			continue
		}
		if strings.Contains(t, "=") && !strings.ContainsAny(t, "/ ") && isAssignment(t) {
			i++ // FOO=bar leading assignment
			continue
		}
		break
	}
	if i >= len(toks) {
		return "", nil
	}
	return toks[i], toks[i+1:]
}

// isAssignment reports whether t looks like NAME=value (a leading env assignment).
func isAssignment(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	for j := 0; j < eq; j++ {
		c := t[j]
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (j > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func isShell(base string) bool {
	switch base {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

// dashCArg returns the string argument of a `-c` flag, if present.
func dashCArg(args []string) (string, bool) {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if filepath.Base(a) == want {
			return true
		}
	}
	return false
}

// --- rm --------------------------------------------------------------------

func classifyRm(args []string, root string) Decision {
	recursive, force, targets := parseRm(args)
	for _, t := range targets {
		if d, ok := protectedDeny(t, root); ok {
			return d
		}
		switch pathClass(t, root) {
		case pathNet:
			return denyNet("rm", t)
		case pathRoot:
			return denyWholeTree(t)
		case pathOutside:
			return denyOutside("rm", t)
		}
	}
	// Inside the tree: a recursive/forced or bulk delete is recoverable but worth
	// a recovery point; a single plain `rm file` is low-signal and passes silently.
	if recursive || force || len(targets) > 1 {
		return checkpoint("rm-recursive")
	}
	return pass()
}

// parseRm separates rm's recursive/force flags from its file targets, handling
// combined short flags (-rf, -fr), long flags, and the `--` end-of-flags marker.
func parseRm(args []string) (recursive, force bool, targets []string) {
	endFlags := false
	for _, a := range args {
		if endFlags {
			targets = append(targets, a)
			continue
		}
		switch {
		case a == "--":
			endFlags = true
		case a == "--recursive":
			recursive = true
		case a == "--force":
			force = true
		case a == "-" || !strings.HasPrefix(a, "-"):
			targets = append(targets, a)
		default: // a combined short-flag bundle like -rf
			for _, c := range a[1:] {
				if c == 'r' || c == 'R' {
					recursive = true
				}
				if c == 'f' {
					force = true
				}
			}
		}
	}
	return recursive, force, targets
}

// --- git -------------------------------------------------------------------

func classifyGit(args []string) Decision {
	sub, rest := gitSubcommand(args)
	switch sub {
	case "push":
		if hasAny(rest, "--force", "-f") || hasPrefix(rest, "--force-with-lease") {
			return Decision{
				Tier:           TierDeny,
				MatchedPattern: "git-push-force",
				Reason: "`git push --force` rewrites already-published history, which Salvager " +
					"(a local file-history net) cannot recover. If this is intended, ask the user to run it manually.",
			}
		}
	case "reset":
		if hasAny(rest, "--hard") {
			return checkpoint("git-reset-hard")
		}
	case "clean":
		if gitCleanForced(rest) {
			return checkpoint("git-clean-force")
		}
	case "checkout":
		if hasAny(rest, "-f", "--force") || containsToken(rest, "--") || containsToken(rest, ".") {
			return checkpoint("git-checkout-force")
		}
	case "stash":
		if !startsWithAny(rest, "list", "show") {
			return checkpoint("git-stash")
		}
	}
	return pass()
}

// gitSubcommand skips git's global options (and their values) to find the
// subcommand and its remaining args.
func gitSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace":
			i++ // skip the option's value too
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, args[i+1:]
	}
	return "", nil
}

// gitCleanForced reports whether a `git clean` is a real (forced, non-dry-run)
// clean — it carries -f/--force in some flag bundle and not -n/--dry-run.
func gitCleanForced(args []string) bool {
	forced := false
	for _, a := range args {
		if a == "--force" {
			forced = true
		}
		if a == "--dry-run" {
			return false
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			for _, c := range a[1:] {
				if c == 'n' {
					return false
				}
				if c == 'f' {
					forced = true
				}
			}
		}
	}
	return forced
}

// --- sed / find / truncate -------------------------------------------------

// classifySed flags an in-place edit (sed -i). Targets outside the tree or in the
// net are unrecoverable (deny); in-place edits inside the tree are recoverable
// (checkpoint). A sed without -i writes to stdout and changes nothing.
func classifySed(args []string, root string) Decision {
	inPlace := false
	var targets []string
	for _, a := range args {
		switch {
		case a == "-i" || a == "--in-place" || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place="):
			inPlace = true
		case strings.HasPrefix(a, "-"):
			// a flag (e.g. -e, -n); the script arg is also passed but is not a path
		default:
			targets = append(targets, a)
		}
	}
	if !inPlace {
		return pass()
	}
	// The first non-flag token is sed's script, not a file; judge the rest as files.
	for _, t := range fileTargets(targets, 1, root) {
		if d, ok := protectedDeny(t, root); ok {
			return d
		}
		switch pathClass(t, root) {
		case pathNet:
			return denyNet("sed -i", t)
		case pathOutside:
			return denyOutside("sed -i", t)
		}
	}
	return checkpoint("sed-i")
}

// classifyFind flags a destructive find: `-delete`, or an `-exec`/`-execdir` whose
// command is a destructive verb (rm, mv, truncate, …) — `find . -name .env -exec rm
// {} \;` deletes just as `-delete` does. Search roots are the path tokens before the
// first predicate; a root outside the tree (find / …, find ~ …) is unrecoverable, and
// a protected path named anywhere in the expression (a root, or a -name/-path operand
// naming a protected file to delete) is a protected deny. Inside the tree → checkpoint.
func classifyFind(args []string, root string) Decision {
	if !findDestructive(args) {
		return pass()
	}
	// Search roots only (judged for tree-escape): a -path/-name value that happens to
	// look absolute is an operand, not a walk root, so net/outside is checked here.
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break // predicates start here; search roots are done
		}
		switch pathClass(a, root) {
		case pathNet:
			return denyNet("find", a)
		case pathOutside:
			return denyOutside("find", a)
		}
	}
	// A protected path named anywhere in a destructive find is a protected deny.
	// TODO(review#3): a find rooted UNDER a protected dir with no matching -name (e.g.
	// in-tree `find .ssh -type f -delete`) isn't denied — rare, out of current scope.
	for _, a := range args {
		if strings.HasPrefix(a, "-") || a == "{}" || a == ";" || a == `\;` || a == "+" {
			continue // a flag/predicate or find's -exec syntax, not a target path
		}
		if d, ok := protectedDeny(a, root); ok {
			return d
		}
	}
	return checkpoint("find-destructive")
}

// findDestructive reports whether a find expression deletes: a -delete predicate, or
// an -exec/-execdir running a destructive verb. Conservative — an -exec sh -c "…" is
// NOT expanded (the inner is opaque shell, the documented interpreter limit), only a
// directly-named destructive verb counts.
func findDestructive(args []string) bool {
	for i, a := range args {
		if a == "-delete" {
			return true
		}
		if (a == "-exec" || a == "-execdir") && i+1 < len(args) &&
			isDestructiveVerb(filepath.Base(args[i+1])) {
			return true
		}
	}
	return false
}

// isDestructiveVerb reports whether a command name (an -exec target, say) destroys
// files. Kept to verbs that always write/delete, so a benign -exec stays a pass.
func isDestructiveVerb(base string) bool {
	switch base {
	case "rm", "mv", "truncate", "shred", "dd":
		return true
	}
	return false
}

// classifyTruncate flags `truncate` (it shortens/zeroes a file). Inside the tree
// the prior content is in the watcher's history (checkpoint); a target outside
// the tree or in the net is unrecoverable (deny).
func classifyTruncate(args []string, root string) Decision {
	var targets []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case a == "-s" || a == "--size" || a == "-r" || a == "--reference":
			skipNext = true // value follows
		case strings.HasPrefix(a, "-"):
			// other flag (e.g. -s0 bundled) — ignore
		default:
			targets = append(targets, a)
		}
	}
	for _, t := range targets {
		if d, ok := protectedDeny(t, root); ok {
			return d
		}
		switch pathClass(t, root) {
		case pathNet:
			return denyNet("truncate", t)
		case pathOutside:
			return denyOutside("truncate", t)
		}
	}
	if len(targets) == 0 {
		return pass()
	}
	return checkpoint("truncate")
}

// denyIfUnsafeWrite returns a Tier A deny if writing to target is unrecoverable: it
// escapes the watched tree (outside, or the .salvager net) or hits a protected path.
// This is the cp/tee/mv/install/ln write-destination equivalent of the guard already
// applied to rm, sed -i and > redirects — closing the tree-escape parity gap.
//
// Deliberately NO pathRoot/denyWholeTree case: for a WRITE, the root (".") is just the
// cwd you write into (cp x ., mv x .) — recoverable and ubiquitous — not whole-tree
// destruction, which is a delete-only concept. This mirrors the > redirect / sed -i /
// truncate guards exactly (they too deny only net/outside); only rm, a delete, treats
// the root as fatal. Including pathRoot here would false-deny `cp x .`.
func denyIfUnsafeWrite(verb, target, root string) (Decision, bool) {
	switch pathClass(target, root) {
	case pathNet:
		return denyNet(verb, target), true
	case pathOutside:
		return denyOutside(verb, target), true
	}
	return protectedDeny(target, root) // protected hit (or no deny)
}

// The cp/tee/mv/install/ln classifiers share one rule: escape-check the unambiguous
// WRITE destination(s); protected-check destinations (and, for mv, sources too, since
// a moved-away source is destroyed); never escape-check a READ argument.

// classifyCp handles cp and install: `[flags] src… dest`. The dest is the last non-flag
// arg and the only write — escape+protected-check it. The src args are reads, untouched.
func classifyCp(verb string, args []string, root string) Decision {
	nf := nonFlagArgs(args)
	if len(nf) == 0 {
		return pass()
	}
	if d, ok := denyIfUnsafeWrite(verb, nf[len(nf)-1], root); ok {
		return d
	}
	return pass()
}

// classifyTee handles tee: `[flags] file…`. Every non-flag operand is a write target.
func classifyTee(args []string, root string) Decision {
	for _, a := range nonFlagArgs(args) {
		if d, ok := denyIfUnsafeWrite("tee", a, root); ok {
			return d
		}
	}
	return pass()
}

// classifyMv handles mv: `[flags] src… dest`. The dest (last non-flag) is the write —
// escape+protected-check it. The sources are reads for the escape check (mv /etc/hosts
// ./x reads outside, writes in-tree → must pass), but a protected source is still a deny
// because the move destroys it at its old location.
func classifyMv(args []string, root string) Decision {
	nf := nonFlagArgs(args)
	if len(nf) == 0 {
		return pass()
	}
	dest := nf[len(nf)-1]
	for _, src := range nf[:len(nf)-1] {
		if d, ok := protectedDeny(src, root); ok {
			return d
		}
	}
	if d, ok := denyIfUnsafeWrite("mv", dest, root); ok {
		return d
	}
	return pass()
}

// classifyLn handles ln. With two+ non-flag args (`[flags] target linkname`) the linkname
// is the write — escape+protected-check it; target is a read. With a single non-flag arg
// (`ln target`) the real write is basename(target) created in cwd (in-tree, so no escape
// check) — only protected-check that basename, so `ln /backup/.env` (which would create
// .env here) is still caught.
func classifyLn(args []string, root string) Decision {
	nf := nonFlagArgs(args)
	switch len(nf) {
	case 0:
		return pass()
	case 1:
		if d, ok := protectedDeny(filepath.Base(nf[0]), root); ok {
			return d
		}
		return pass()
	default:
		if d, ok := denyIfUnsafeWrite("ln", nf[len(nf)-1], root); ok {
			return d
		}
		return pass()
	}
}

// nonFlagArgs returns the arguments that are not "-"-prefixed flags, in order. A bundled
// value flag (cp -t DIR) could shift which non-flag is the true dest, but that's an
// obscure form; taking operands positionally is the conservative reading.
func nonFlagArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// fileTargets returns ts[skip:] (its leading non-file tokens dropped). root is
// unused beyond documenting intent but kept for symmetry with the path helpers.
func fileTargets(ts []string, skip int, root string) []string {
	_ = root
	if skip >= len(ts) {
		return nil
	}
	return ts[skip:]
}

// --- path containment ------------------------------------------------------

// path classes for a target token, judged against the project root. Geometric
// only — each command decides the tier, because the same position means different
// things to different verbs (`.` is fatal to `rm -rf` but fine as `find`'s root).
const (
	pathInside  = iota // a proper subpath of the tree — recoverable by the watcher
	pathOutside        // escapes the tree (or is an ancestor of it) — unrecoverable
	pathNet            // is, or lives under, .salvager/ — destroys the net itself
	pathRoot           // is the tree root (".") — recursive delete takes the net too
	pathOther          // not a filesystem path we should judge (a flag, etc.)
)

// pathClass decides whether a target token is recoverable. The discriminator
// between Tier A and Tier B for a deletion: a token that escapes the tree (or
// would take the whole tree, and thus .salvager/, with it) is unrecoverable;
// a proper subpath is the watcher's home turf.
//
// With an empty root only clearly-external tokens (~, $HOME, /) are judged
// outside; relative tokens default to inside, never a false deny.
func pathClass(token, root string) int {
	if token == "" || strings.HasPrefix(token, "-") {
		return pathOther
	}
	// Home and filesystem root are external regardless of the project root.
	if token == "~" || strings.HasPrefix(token, "~/") {
		return pathOutside
	}
	if token == "$HOME" || token == "${HOME}" ||
		strings.HasPrefix(token, "$HOME/") || strings.HasPrefix(token, "${HOME}/") {
		return pathOutside
	}
	if token == "/" {
		return pathOutside
	}
	// A still-unexpanded variable target we can't resolve — don't guess; pass.
	if strings.HasPrefix(token, "$") {
		return pathOther
	}

	if root == "" {
		if filepath.IsAbs(token) {
			return pathOther // can't compare to an unknown root
		}
		return pathInside
	}
	root = filepath.Clean(root)
	abs := token
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, token)
	}
	abs = filepath.Clean(abs)

	net := filepath.Join(root, storeDir)
	sep := string(os.PathSeparator)
	if abs == net || strings.HasPrefix(abs, net+sep) {
		return pathNet
	}
	if abs == root {
		return pathRoot
	}
	if strings.HasPrefix(abs, root+sep) {
		return pathInside
	}
	return pathOutside // ancestor of root, or fully outside
}

// redirectTargets returns the file targets of any >/>>/>| redirections in a clause,
// so a redirect that writes into the net or outside the tree can be denied. It covers
// the clobber forms (>, >>, >| with -o noclobber overridden); fd duplications
// (2>&1, >&2) are not file writes and are skipped.
func redirectTargets(toks []string) []string {
	var out []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		j := 0
		for j < len(t) && t[j] >= '0' && t[j] <= '9' {
			j++
		}
		rest := t[j:]
		switch {
		case strings.HasPrefix(rest, ">>"):
			rest = rest[2:]
		case strings.HasPrefix(rest, ">"):
			rest = rest[1:]
		default:
			continue
		}
		rest = strings.TrimPrefix(rest, "|") // >| clobber-override: the file follows the bar
		if strings.HasPrefix(rest, "&") {
			continue // fd dup, not a file
		}
		if rest != "" {
			out = append(out, rest)
		} else if i+1 < len(toks) {
			out = append(out, toks[i+1])
			i++
		}
	}
	return out
}

// --- decision constructors -------------------------------------------------

func pass() Decision { return Decision{Tier: TierPass} }

func checkpoint(pattern string) Decision {
	return Decision{Tier: TierCheckpoint, MatchedPattern: pattern, RecoveryHint: recoveryHint(nowFunc())}
}

// recoveryHint is honest about the net: the continuous capture is the running
// watcher's job, so the hint references the instant and reminds the agent the net
// only exists if the watcher is up. The core never walks/captures the tree here —
// that keeps Classify fast and the adapter able to fail open.
func recoveryHint(ms int64) string {
	return fmt.Sprintf(
		"Salvager has a recovery point as of %d (only if its watcher is running — confirm with `salvager service status`). "+
			"If this command breaks the working tree, rewind with: salvager restore-at %d  "+
			"(or run `salvager timeline` to pick the exact instant).", ms, ms)
}

func denyOutside(cmd, target string) Decision {
	return Decision{
		Tier:           TierDeny,
		MatchedPattern: "escape",
		Reason: fmt.Sprintf("`%s` targets %q, a path outside this project that Salvager's "+
			"watcher cannot recover. If this is intended, ask the user to run it manually.", cmd, target),
	}
}

func denyNet(cmd, target string) Decision {
	return Decision{
		Tier:           TierDeny,
		MatchedPattern: "salvager-store",
		Reason: fmt.Sprintf("`%s` would write to %q — Salvager's own recovery store (.salvager/). "+
			"Refused, because it would destroy the safety net. Use a real project path instead.", cmd, target),
	}
}

func denyWholeTree(target string) Decision {
	return Decision{
		Tier:           TierDeny,
		MatchedPattern: "whole-tree",
		Reason: fmt.Sprintf("`rm` would recursively delete %q — the entire project tree, taking "+
			"Salvager's own store (.salvager/) with it. Refused. Delete a specific subpath instead.", target),
	}
}

func denyIrreversible(cmd string) Decision {
	return Decision{
		Tier:           TierDeny,
		MatchedPattern: cmd,
		Reason: fmt.Sprintf("`%s` makes an irreversible change Salvager cannot recover (it bypasses the "+
			"file-history watcher). If this is intended, ask the user to run it manually.", cmd),
	}
}

// --- small slice predicates ------------------------------------------------

func hasAny(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	return false
}

func hasPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func containsToken(args []string, tok string) bool {
	for _, a := range args {
		if a == tok {
			return true
		}
	}
	return false
}

func startsWithAny(args []string, want ...string) bool {
	if len(args) == 0 {
		return false
	}
	for _, w := range want {
		if args[0] == w {
			return true
		}
	}
	return false
}

// --- the seismograph -------------------------------------------------------

// attemptEntry is one line of the append-only attempt log. The command is hashed,
// never stored verbatim — it can contain secrets. This is the raw, agent-agnostic
// signal a future signature catalog is built from.
type attemptEntry struct {
	TS      int64  `json:"ts"`
	Tier    string `json:"tier"`
	Tool    string `json:"tool"`
	Agent   string `json:"agent"`
	Matched string `json:"matched"`
	CmdHash string `json:"cmd_hash"`
}

// LogAttempt appends a Tier A/B decision to .salvager/hook-log (append-only,
// 0600, under the watch-excluded store dir). Pass decisions are not logged. It is
// the adapter's job to call this (and to ignore its error — logging must never
// affect whether the guarded tool runs). Separate from Classify so the verdict
// stays pure and the adapter can always fail open.
func LogAttempt(req Request, d Decision) error {
	if d.Tier == TierPass || req.Root == "" {
		return nil
	}
	dir := filepath.Join(req.Root, storeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(attemptEntry{
		TS:      nowFunc(),
		Tier:    d.Tier.String(),
		Tool:    req.Tool,
		Agent:   req.Agent,
		Matched: d.MatchedPattern,
		CmdHash: hashCommand(req.Command),
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "hook-log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// hashCommand returns a short, non-reversible fingerprint of a command, so the
// seismograph records that an attempt happened and which without ever persisting
// the command (and any secrets in it) verbatim.
func hashCommand(cmd string) string {
	sum := sha256.Sum256([]byte(cmd))
	return hex.EncodeToString(sum[:])[:16]
}
