// Package store holds the per-file revision history under .salvager/.
//
// Layout on disk:
//
//	.salvager/
//	├── objects/<sha256>        full content, deduplicated by hash
//	└── index/<relpath>.log     one line per revision of that file
//
// Each .log line is tab-separated. Lines written before the content signal
// existed have three columns:
//
//	<unix_ms>\t<sha256>\t<label>
//
// Newer lines carry a content signal computed once at capture, so a caller can
// summarize a revision (and tell which one holds a given block of work) without
// ever reading its object back:
//
//	<unix_ms>\t<sha256>\t<label>\t<lines>\t<bytes>\t<delta>\t<quoted-start-signature>
//
// where <delta> is the signed line count vs the previous revision ("?" when the
// previous revision predates the signal) and the start signature is the first
// few non-empty lines, Go-quoted so it stays on a single tab-free line. Both
// shapes are readable without this tool: `ls` and `cat` recover any version by
// hand.
package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dir is the name of the history directory created inside a project root.
const Dir = ".salvager"

// ErrUnsafePath is returned when a caller-supplied relPath would escape the
// project root. The store is the safety net for one project's working tree; it
// must never read, write, or delete outside that tree. Containment lives here,
// at the lowest shared chokepoint, so every entry point (MCP tools, CLI, the
// watcher) is protected by one rule rather than each guarding itself.
var ErrUnsafePath = errors.New("path escapes project root")

// safeRel rejects any relPath that is not a clean path contained within the
// root. filepath.IsLocal accepts ordinary relative paths including
// subdirectories ("a/b/c.txt") and inner ".." that still stays inside
// ("a/../b"), while rejecting real escapes ("../x", "a/../../x"), absolute
// paths, volume-prefixed paths, and the empty string. The empty string
// rejection is deliberate: a missing/zero-value "file" argument is an unsafe
// path, not a request for some default file.
func safeRel(relPath string) error {
	if !filepath.IsLocal(relPath) {
		return fmt.Errorf("%w: %q", ErrUnsafePath, relPath)
	}
	return nil
}

// resolveContained validates relPath lexically (safeRel) and then, defending
// against a symlinked intermediate component, verifies that the deepest
// EXISTING ancestor of its absolute form resolves back inside the project root.
// filepath.IsLocal is purely lexical: it accepts "link/secret" even when "link"
// is a symlink escaping the tree, which would let an MCP/CLI caller read,
// overwrite, or delete files outside the root through record/Restore. The
// watcher never tracks files reached via a symlink, so legitimate paths always
// resolve under the root; this rejects the rest. Both sides are EvalSymlinks'd
// so a symlinked root prefix (e.g. macOS /var -> /private/var) compares equal.
// Returns the absolute working-tree path on success.
func (s *FS) resolveContained(relPath string) (string, error) {
	if err := safeRel(relPath); err != nil {
		return "", err
	}
	abs := filepath.Join(s.root, relPath)
	rootReal, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", err
	}
	// A not-yet-created leaf cannot itself be a symlink, so walk up to the
	// deepest ancestor that exists and resolve that.
	anc := filepath.Dir(abs)
	for {
		real, err := filepath.EvalSymlinks(anc)
		if err == nil {
			if real != rootReal && !strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
				return "", fmt.Errorf("%w: %q", ErrUnsafePath, relPath)
			}
			return abs, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			return "", fmt.Errorf("%w: %q", ErrUnsafePath, relPath)
		}
		anc = parent
	}
}

// Label classifies why a revision was recorded.
type Label string

const (
	// LabelInitial marks the first revision salvager recorded for a file. Its
	// value is "first-seen", not "initial", on purpose: this revision already
	// holds the content captured the moment salvager first saw the file — it is
	// NOT an empty starting point, and an agent must inspect it like any other.
	LabelInitial    Label = "first-seen"
	LabelModify     Label = "modify"
	LabelDelete     Label = "delete"
	LabelPreRestore Label = "pre-restore"
	LabelRestore    Label = "restore"
)

// Revision is one recorded version of a file.
type Revision struct {
	Timestamp int64 // unix milliseconds
	Hash      string
	Label     Label

	// Content signal: computed once at capture (from the content already in
	// memory for the hash) and stored in the .log line, so List can summarize a
	// revision without reading its object. HasSignal is false for legacy lines
	// written before the signal existed — treat those as "signal unavailable",
	// never as an error.
	HasSignal  bool
	Lines      int    // total line count of the revision's content (0 for a deletion)
	Bytes      int    // byte length of the content (0 for a deletion)
	Delta      int    // signed line delta vs the previous revision at capture time
	DeltaKnown bool   // false when the previous revision had no signal to diff against
	Sig        string // first up to 3 non-empty lines (empty for binary or no content)
}

// DeltaString renders the signed line delta for display: "+N"/"-N", "?" when it
// could not be computed (the previous revision predates the signal), or "" when
// the revision itself carries no signal at all.
func (r Revision) DeltaString() string {
	if !r.HasSignal {
		return ""
	}
	if !r.DeltaKnown {
		return "?"
	}
	return fmt.Sprintf("%+d", r.Delta)
}

// Store is the contract the rest of the program depends on. Restore is the
// only operation that writes to the working tree; it returns the timestamp of
// the safeguard revision it took first, so callers know how to undo.
type Store interface {
	Record(relPath string) error
	List(relPath string) ([]Revision, error)
	Get(relPath string, ts int64) ([]byte, error)
	Restore(relPath string, ts int64) (preRestoreTs int64, err error)
	GC(maxAge time.Duration) error
	GCBySize(maxBytes int64) (finalBytes int64, err error)
}

// nowFunc is the clock; overridable in tests.
var nowFunc = func() int64 { return time.Now().UnixMilli() }

// FS is a filesystem-backed Store rooted at a project directory.
type FS struct {
	root string
	mu   sync.Mutex // serializes all writes for the v1 (simple first)
}

// New returns a Store rooted at root. The .salvager/ directory is created lazily.
func New(root string) *FS {
	return &FS{root: root}
}

// Init eagerly creates the .salvager/ skeleton (objects/ and index/). The watcher
// calls this on startup so `salvager watch` materializes the history directory
// immediately, even in an empty project before the first change. Read-only
// entry points (history/show) still create nothing.
func (s *FS) Init() error {
	if err := os.MkdirAll(s.objectsDir(), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(s.indexDir(), 0o700)
}

func (s *FS) dir() string        { return filepath.Join(s.root, Dir) }
func (s *FS) objectsDir() string { return filepath.Join(s.dir(), "objects") }
func (s *FS) indexDir() string   { return filepath.Join(s.dir(), "index") }

func (s *FS) logPath(relPath string) string {
	return filepath.Join(s.indexDir(), relPath+".log")
}

func (s *FS) objectPath(hash string) string {
	return filepath.Join(s.objectsDir(), hash)
}

// lockWait bounds how long a writer (or a Get's shared reader) blocks waiting
// for the cross-process store lock before giving up with a clear error rather
// than hanging. 30s clears every BOUNDED holder with margin — a large-file
// Record/Restore runs at roughly 2s/GB — and sits ~4 orders of magnitude under
// Claude Code's default MCP_TOOL_TIMEOUT (~28h; salvager registers no per-server
// timeout), so a timeout surfaces as salvager's own clean error, never a
// truncated MCP tool call. It deliberately does NOT bound GC, whose hold scales
// with store size (see gcLocked): a rare overlap of an interactive restore with
// a large concurrent `salvager gc` fails loud and retryable, never silently
// corrupt and never an unlocked write.
const lockWait = 30 * time.Second

// lockPath is the single coarse lock guarding all cross-process access to one
// .salvager store. One lock file, never deleted (removing a flock'd file races a
// peer that still holds it). Writers take it exclusive; Get takes it shared.
func (s *FS) lockPath() string { return filepath.Join(s.dir(), "lock") }

// withWriteLock runs fn under both the in-process mutex (peers in THIS process)
// and an exclusive flock (peers in OTHER processes — the watcher service, CLI
// invocations, the MCP server, each holding its own store.New over the same
// directory). s.mu is taken first, so at most one goroutine per process ever
// contends the flock. fn never runs unless the flock was acquired: on timeout
// the helper returns the error and fn is skipped — the store is never mutated
// while unlocked.
func (s *FS) withWriteLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := acquireFlock(s.lockPath(), true, lockWait)
	if err != nil {
		return err
	}
	defer h.release()
	return fn()
}

// withReadLock runs fn under a SHARED flock: concurrent readers proceed together
// but exclude any exclusive writer. It does NOT take s.mu — readers must not
// serialize behind one another. It locks only when a store already exists: on an
// untracked project (no .salvager/) there is nothing to read and nothing to
// race, and locking would materialize the store, violating the "read-only
// commands create nothing" invariant. When the store exists it ensures the lock
// file (so even a store first written by a pre-lock binary is serialized against
// a concurrent sweep). Only Get needs this lock (see getLocked).
func (s *FS) withReadLock(fn func() error) error {
	if _, err := os.Stat(s.dir()); err != nil {
		if os.IsNotExist(err) {
			return fn() // no store: nothing to lock against, create nothing
		}
		return err
	}
	h, err := acquireFlock(s.lockPath(), false, lockWait)
	if err != nil {
		return err
	}
	defer h.release()
	return fn()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Record captures the current state of relPath if it differs from the last
// recorded revision. Deduplicated by content hash. A missing file is recorded
// as a delete (no new object).
func (s *FS) Record(relPath string) error {
	if err := safeRel(relPath); err != nil {
		return err
	}
	return s.withWriteLock(func() error {
		return s.record(relPath, "")
	})
}

// record does the work; label forces a specific label (used by Restore for
// pre-restore). Empty label means: pick initial/modify/delete automatically.
// Caller holds s.mu.
func (s *FS) record(relPath string, force Label) error {
	abs, err := s.resolveContained(relPath)
	if err != nil {
		return err
	}
	f, err := os.Open(abs)

	last, hasLast := s.lastRevision(relPath)

	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// File is gone from disk. (No content; same handling as before.)
		if force != "" {
			// Forced safeguard of an absent file: mark deletion explicitly.
			h := ""
			if hasLast {
				h = last.Hash
			}
			return s.appendLog(relPath, force, h, computeSig(nil, last, hasLast))
		}
		if !hasLast || last.Label == LabelDelete {
			return nil // never tracked, or already known deleted
		}
		return s.appendLog(relPath, LabelDelete, last.Hash, computeSig(nil, last, hasLast))
	}
	defer f.Close()

	// Stream the file once: object write, hash and content signal all computed
	// in a single pass over a fixed buffer, so resident memory stays O(bufSize)
	// regardless of file size. The object is content-addressed, so the hash —
	// and thus the dedup decision — is only known after the whole stream is read.
	hash, sig, err := s.captureStream(f, last, hasLast)
	if err != nil {
		return err
	}

	if force == "" && hasLast && last.Hash == hash && last.Label != LabelDelete {
		// Unchanged: the streamed object was deduped away by existence; record
		// nothing. Same observable result as before — now decided by the content
		// hash over the full streamed bytes, never by a stat shortcut.
		return nil
	}

	label := force
	if label == "" {
		if hasLast {
			label = LabelModify
		} else {
			label = LabelInitial
		}
	}
	return s.appendLog(relPath, label, hash, sig)
}

// bufSize is the fixed streaming buffer. Capture reads and hashes through a
// buffer this size, so resident memory is O(bufSize) regardless of file size.
// 128 KiB amortizes read syscalls on page-cache-warm reads; sha256 runs at GB/s
// so capture is read-bound and throughput is flat past ~64-128 KiB. Captures are
// serialized under s.mu, so only one buffer is ever live.
const bufSize = 128 * 1024

// sigScanLimit bounds how much of a file's head is retained to compute the start
// signature. It is >= the 8192-byte binary-check window, and large enough that
// any file whose first three non-empty lines end within it yields a signature
// byte-identical to a whole-file scan. A file whose first three non-empty lines
// fall beyond it — notably a long single-line file (minified JSON, a one-row
// CSV, a one-line dataset) — gets a possibly-shorter signature. That never
// affects the hash or dedup (both run over the full streamed bytes); it only
// makes the human-facing signal less distinctive for those files.
const sigScanLimit = 64 * 1024

// captureStream reads src exactly once, writing its bytes to a temp object while
// feeding the same bytes to the hasher, the line/byte counter and a bounded
// prefix retained for the start signature. On success the content lives at
// objects/<hash> — renamed from the temp, or the temp discarded when the object
// already exists (dedup by existence) — and the content signal is returned.
//
// Atomicity (temp + rename) and dedup-by-existence are preserved from the
// previous byte-slice writeObject; the only change is that the existence check
// happens after the hash is known, because the object path is the hash. A single
// pass over one buffer also closes the TOCTOU a two-pass (hash, then re-read to
// copy) design would open: the object bytes and their content-address come from
// the very same byte stream.
// captureStream takes ownership of closing src: it releases the source fd the
// instant its single read completes, before the object is placed, so a Stat or
// Rename failure during placement can never hold the source open. The caller may
// still keep a deferred Close as a belt — os.File.Close is safe to call twice.
func (s *FS) captureStream(src io.ReadCloser, last Revision, hasLast bool) (string, revSig, error) {
	if err := os.MkdirAll(s.objectsDir(), 0o700); err != nil {
		return "", revSig{}, err
	}
	tmp, err := os.CreateTemp(s.objectsDir(), ".tmp-*")
	if err != nil {
		return "", revSig{}, err
	}
	tmpName := tmp.Name()

	h := sha256.New()
	ctr := &lineByteCounter{}
	pre := &prefixSink{limit: sigScanLimit}
	buf := make([]byte, bufSize)

	// One pass: every byte goes to the object, the hasher, the counter and the
	// prefix at once. Only the temp write can fail; the other sinks never error.
	_, copyErr := io.CopyBuffer(io.MultiWriter(tmp, h, ctr, pre), src, buf)
	// Release the source fd now — its single read is done — and the temp fd, both
	// before placement. src close errors are not actionable here: the bytes were
	// already fully read and hashed, so a cosmetic read-fd close failure must not
	// discard a valid capture; the deferred close in the caller is the belt.
	_ = src.Close()
	closeErr := tmp.Close()
	// On any failure, best-effort remove the temp; the returned error is the real
	// cause, so a failed cleanup must not mask it.
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return "", revSig{}, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return "", revSig{}, closeErr
	}

	hash := hex.EncodeToString(h.Sum(nil))
	path := s.objectPath(hash)
	if _, statErr := os.Stat(path); statErr == nil {
		_ = os.Remove(tmpName) // object already present: content-addressed, identical bytes
	} else if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", revSig{}, err
	}

	return hash, computeSigFromStream(ctr, pre, last, hasLast), nil
}

// lineByteCounter tallies bytes and newline-delimited lines as content streams
// through it, applying the same final-unterminated-line rule as lineCount so a
// signal computed from a stream equals the one computed from the whole slice.
type lineByteCounter struct {
	total    int64
	newlines int
	lastByte byte
	wrote    bool
}

func (c *lineByteCounter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.total += int64(len(p))
		c.newlines += bytes.Count(p, []byte{'\n'})
		c.lastByte = p[len(p)-1]
		c.wrote = true
	}
	return len(p), nil
}

// lines mirrors lineCount: empty content is 0 lines; otherwise count newlines and
// add one for a final unterminated line.
func (c *lineByteCounter) lines() int {
	if !c.wrote || c.total == 0 {
		return 0
	}
	n := c.newlines
	if c.lastByte != '\n' {
		n++
	}
	return n
}

// prefixSink retains the first limit bytes of a stream and discards the rest, so
// the start signature can be computed from the head without buffering the whole
// file (keeping capture O(bufSize) memory even for a huge single-line file).
type prefixSink struct {
	buf   []byte
	limit int
}

func (p *prefixSink) Write(b []byte) (int, error) {
	if room := p.limit - len(p.buf); room > 0 {
		if room > len(b) {
			room = len(b)
		}
		p.buf = append(p.buf, b[:room]...)
	}
	return len(b), nil
}

// revSig is the content signal for one revision, computed at capture from the
// content already in memory. For a deletion content is empty: lines/bytes are 0
// and sig is "".
type revSig struct {
	lines      int
	bytes      int
	delta      int
	deltaKnown bool
	sig        string
}

// computeSig builds the content signal for content (nil/empty for a deletion),
// diffing the line count against the previous revision. hasLast false means this
// is the first revision of the file, so every line counts as added. When the
// previous revision predates the signal its line count is unknown, so the delta
// is left unknowable rather than guessed.
func computeSig(content []byte, last Revision, hasLast bool) revSig {
	n := lineCount(content)
	rs := revSig{lines: n, bytes: len(content), sig: startSignature(content)}
	rs.delta, rs.deltaKnown = deltaFor(n, last, hasLast)
	return rs
}

// computeSigFromStream builds the same signal as computeSig, but from the
// streamed counters and retained head prefix instead of a full byte slice, so
// the streaming capture path and the byte-slice path produce identical signals.
func computeSigFromStream(ctr *lineByteCounter, pre *prefixSink, last Revision, hasLast bool) revSig {
	n := ctr.lines()
	rs := revSig{lines: n, bytes: int(ctr.total), sig: startSignature(pre.buf)}
	rs.delta, rs.deltaKnown = deltaFor(n, last, hasLast)
	return rs
}

// deltaFor computes the signed line delta vs the previous revision and whether it
// is knowable. The first revision counts every line as added; a previous
// revision that predates the signal leaves the delta unknowable rather than
// guessed. Shared by both signal paths so they cannot drift.
func deltaFor(n int, last Revision, hasLast bool) (int, bool) {
	switch {
	case !hasLast:
		return n, true // first revision: all lines are new
	case last.HasSignal:
		return n - last.Lines, true
	default:
		return 0, false // previous revision has no signal to diff against
	}
}

// lineCount counts newline-delimited lines, counting a final unterminated line.
// Empty content is 0 lines. Matches the intuition behind a "+N/-N lines" delta.
func lineCount(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}

// startSignature returns the first up to 3 non-empty lines as a lightweight
// fingerprint of how the content begins. It degrades gracefully: binary content
// (a NUL byte in the head, the standard heuristic) yields "" rather than a mess
// of raw bytes, and each line is clamped so the signature stays glanceable. The
// caller Go-quotes the result before storage, so any stray control bytes can
// never corrupt the single-line .log format.
func startSignature(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	head := content
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "" // binary: no meaningful text signature
	}
	var picked []string
	rest := content
	for len(picked) < 3 && len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		s := strings.TrimRight(string(line), "\r")
		if strings.TrimSpace(s) == "" {
			continue
		}
		if len(s) > 120 {
			s = s[:120]
		}
		picked = append(picked, s)
	}
	return strings.Join(picked, "\n")
}

// formatLine serializes one revision to its .log line. Revisions carrying a
// signal write the seven-column form; legacy revisions (HasSignal false, only
// ever produced by parsing an old three-column line) round-trip unchanged so GC
// never fabricates a signal it cannot compute without reading the object.
func formatLine(r Revision) string {
	if !r.HasSignal {
		return fmt.Sprintf("%d\t%s\t%s\n", r.Timestamp, r.Hash, r.Label)
	}
	delta := "?"
	if r.DeltaKnown {
		delta = strconv.Itoa(r.Delta)
	}
	return fmt.Sprintf("%d\t%s\t%s\t%d\t%d\t%s\t%s\n",
		r.Timestamp, r.Hash, r.Label, r.Lines, r.Bytes, delta, strconv.Quote(r.Sig))
}

// appendLog appends one revision line to the file's .log, O_APPEND, with a
// timestamp guaranteed strictly greater than the previous line's so every
// revision of a file is uniquely addressable by timestamp. The content signal
// is computed by the caller (which already holds the content) and stored inline.
func (s *FS) appendLog(relPath string, label Label, hash string, sig revSig) error {
	lp := s.logPath(relPath)
	// 0o700/0o600: the history store holds recovered file contents (and any
	// secrets in them); it must be readable only by the owner, not world-readable.
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		return err
	}
	ts := nowFunc()
	if last, ok := s.lastRevision(relPath); ok && ts <= last.Timestamp {
		ts = last.Timestamp + 1
	}
	f, err := os.OpenFile(lp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(formatLine(Revision{
		Timestamp: ts, Hash: hash, Label: label,
		HasSignal: true,
		Lines:     sig.lines, Bytes: sig.bytes,
		Delta: sig.delta, DeltaKnown: sig.deltaKnown,
		Sig: sig.sig,
	}))
	return err
}

// parseLine parses one .log line. ok is false for malformed lines (which are
// tolerated, e.g. a final line truncated by a crash). Both the legacy
// three-column form and the seven-column signal form are accepted; a line whose
// signal columns are corrupt is kept as a signal-less revision rather than
// dropped, so a recoverable revision is never lost to a bad signal.
func parseLine(line string) (Revision, bool) {
	parts := strings.SplitN(line, "\t", 7)
	if len(parts) < 3 {
		return Revision{}, false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Revision{}, false
	}
	r := Revision{Timestamp: ts, Hash: parts[1], Label: Label(parts[2])}
	if len(parts) == 7 {
		applySignal(&r, parts[3], parts[4], parts[5], parts[6])
	}
	return r, true
}

// applySignal fills the content-signal fields from the four trailing columns. A
// malformed column leaves the revision signal-less (HasSignal stays false).
func applySignal(r *Revision, linesF, bytesF, deltaF, sigF string) {
	lines, err1 := strconv.Atoi(linesF)
	byteN, err2 := strconv.Atoi(bytesF)
	sig, err3 := strconv.Unquote(sigF)
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	r.Lines, r.Bytes, r.Sig, r.HasSignal = lines, byteN, sig, true
	if d, err := strconv.Atoi(deltaF); err == nil {
		r.Delta, r.DeltaKnown = d, true
	}
	// deltaF == "?" (or any non-integer) leaves DeltaKnown false: unknowable.
}

// readLog returns every parseable revision of relPath, oldest first.
func (s *FS) readLog(relPath string) ([]Revision, error) {
	f, err := os.Open(s.logPath(relPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var revs []Revision
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if r, ok := parseLine(sc.Text()); ok {
			revs = append(revs, r)
		}
		// Unparseable lines are skipped, not fatal.
	}
	if err := sc.Err(); err != nil {
		return revs, err
	}
	return revs, nil
}

// lastRevision returns the most recent revision of relPath. Caller holds s.mu
// for writes; reads are tolerant of concurrent appends.
func (s *FS) lastRevision(relPath string) (Revision, bool) {
	revs, err := s.readLog(relPath)
	if err != nil || len(revs) == 0 {
		return Revision{}, false
	}
	return revs[len(revs)-1], true
}

// List returns the revisions of relPath, most recent first.
func (s *FS) List(relPath string) ([]Revision, error) {
	if err := safeRel(relPath); err != nil {
		return nil, err
	}
	revs, err := s.readLog(relPath)
	if err != nil {
		return nil, err
	}
	// Reverse to most-recent-first.
	for i, j := 0, len(revs)-1; i < j; i, j = i+1, j-1 {
		revs[i], revs[j] = revs[j], revs[i]
	}
	return revs, nil
}

// Get returns the content of the revision of relPath taken at ts.
func (s *FS) Get(relPath string, ts int64) ([]byte, error) {
	if err := safeRel(relPath); err != nil {
		return nil, err
	}
	var out []byte
	err := s.withReadLock(func() error {
		b, e := s.getLocked(relPath, ts)
		out = b
		return e
	})
	return out, err
}

// getLocked is the body of Get; the caller holds a SHARED read lock. That lock
// is what makes the log→object lookup atomic against a concurrent GC sweep:
// without it, an object that was valid when the log named it can be swept in the
// window before ReadFile opens it (a dangling read). List needs no such lock —
// it never performs this second lookup into objects/.
func (s *FS) getLocked(relPath string, ts int64) ([]byte, error) {
	revs, err := s.readLog(relPath)
	if err != nil {
		return nil, err
	}
	for _, r := range revs {
		if r.Timestamp == ts {
			if r.Label == LabelDelete {
				return nil, fmt.Errorf("revision %d of %s is a deletion (no content)", ts, relPath)
			}
			return os.ReadFile(s.objectPath(r.Hash))
		}
	}
	return nil, fmt.Errorf("no revision of %s at timestamp %d", relPath, ts)
}

// Restore overwrites relPath with the content of the revision at ts. It first
// records the current on-disk state as a pre-restore safeguard, so the restore
// is itself reversible. Strict order; if the safeguard fails, the working tree
// is left untouched. Returns the safeguard's timestamp.
func (s *FS) Restore(relPath string, ts int64) (int64, error) {
	// Containment is checked before ANY effect — rejecting both lexical escapes
	// and symlinked intermediate components — so neither the pre-restore safeguard
	// (which would read a foreign file's bytes into objects/, an information leak)
	// nor the destructive overwrite/delete branches below can touch a path outside
	// the tree.
	abs, err := s.resolveContained(relPath)
	if err != nil {
		return 0, err
	}

	var preTs int64
	err = s.withWriteLock(func() error {
		p, e := s.restoreLocked(relPath, abs, ts)
		preTs = p
		return e
	})
	return preTs, err
}

// restoreLocked is the body of Restore; the caller holds the write lock. abs is
// the validated, containment-checked working-tree path.
func (s *FS) restoreLocked(relPath, abs string, ts int64) (int64, error) {
	// 1. Validate the requested revision exists BEFORE taking any effect. A bogus
	// (file, timestamp) must be a pure no-op: recording a pre-restore safeguard
	// first would append a spurious revision for a target that turns out absent.
	revs, err := s.readLog(relPath)
	if err != nil {
		return 0, err
	}
	var target *Revision
	for i := range revs {
		if revs[i].Timestamp == ts {
			target = &revs[i]
			break
		}
	}
	if target == nil {
		return 0, fmt.Errorf("no revision of %s at timestamp %d", relPath, ts)
	}

	// 2. Safeguard: record current state, forced (even if hash is unchanged).
	if err := s.record(relPath, LabelPreRestore); err != nil {
		return 0, fmt.Errorf("pre-restore safeguard failed, aborting: %w", err)
	}
	pre, _ := s.lastRevision(relPath) // the safeguard becomes this restore's predecessor

	if target.Label == LabelDelete {
		// Restoring to a deletion: remove the file from the working tree.
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		if err := s.appendLog(relPath, LabelRestore, target.Hash, computeSig(nil, pre, true)); err != nil {
			return 0, err
		}
		return pre.Timestamp, nil
	}

	content, err := os.ReadFile(s.objectPath(target.Hash))
	if err != nil {
		return 0, err
	}

	// 3. Overwrite the working-tree file atomically.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".salvager-restore-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	// On any failure below, best-effort remove the temp; the returned error is
	// the real cause, so a failed cleanup must not mask it.
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}

	// 4. Mark the restore in the log.
	if err := s.appendLog(relPath, LabelRestore, target.Hash, computeSig(content, pre, true)); err != nil {
		return 0, err
	}
	return pre.Timestamp, nil
}

// TrackedUnder returns the relPaths of files the store already has history for
// that live under relDir (relDir == "" means the whole tree). With recursive
// false only direct children of relDir are returned; with true the entire
// subtree. A file whose latest revision is a deletion is excluded — it is
// already known gone, so it must not be re-reported as a fresh delete.
//
// The watch reconciliation sweep uses this to detect files that vanished while
// their directory was covered by polling (or by an FSEvents directory rescan):
// a path the store knows but disk no longer holds is a delete. This reads only
// the index/ side (one ReadDir or WalkDir of small log files), never object
// content, so it stays cheap relative to the disk-side stat pass.
func (s *FS) TrackedUnder(relDir string, recursive bool) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	base := s.indexDir()
	if relDir != "" && relDir != "." {
		base = filepath.Join(base, relDir)
	}

	collect := func(logRel string) []string {
		// logRel is the path under indexDir, ".log" still attached.
		if !strings.HasSuffix(logRel, ".log") {
			return nil
		}
		rel := strings.TrimSuffix(logRel, ".log")
		if last, ok := s.lastRevision(rel); !ok || last.Label == LabelDelete {
			return nil
		}
		return []string{rel}
	}

	var out []string
	if recursive {
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(s.indexDir(), path)
			if err != nil {
				return err
			}
			out = append(out, collect(rel)...)
			return nil
		})
		if os.IsNotExist(err) {
			return nil, nil
		}
		return out, err
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		logRel := e.Name()
		if relDir != "" && relDir != "." {
			logRel = filepath.Join(relDir, e.Name())
		}
		out = append(out, collect(logRel)...)
	}
	return out, nil
}

// GC removes revisions older than maxAge and any objects no longer referenced
// by any log (reference-counted garbage collection).
func (s *FS) GC(maxAge time.Duration) error {
	return s.withWriteLock(func() error { return s.gcLocked(maxAge) })
}

// gcLocked is the body of GC; the caller holds the write lock. GC intentionally
// holds the lock for its ENTIRE run (read logs → rewrite → sweep), so its hold
// scales with store size — tens of seconds on a very large store (~28s over a
// measured 20k-file store). Bounding GC's hold by chunking it requires
// re-validating the referenced set per batch against concurrent writers — a
// distinct re-reference hazard that deserves its own proven change, not this
// one. lockWait is sized with this unbounded hold in mind: a rare
// restore-vs-GC overlap fails loud and retryable, never silently corrupt.
func (s *FS) gcLocked(maxAge time.Duration) error {
	cutoff := nowFunc() - maxAge.Milliseconds()

	logs, err := s.allLogs()
	if err != nil {
		return err
	}

	// Prune each log; collect hashes still referenced afterwards.
	referenced := map[string]struct{}{}
	for _, relPath := range logs {
		revs, err := s.readLog(relPath)
		if err != nil {
			return err
		}
		// Pin each file's newest revision (P1), plus its pre-restore predecessor
		// when the newest is a restore (P2), regardless of age — mirroring
		// buildEvictionUnits. Without this floor, a file whose every revision
		// predates the cutoff would be stripped of its last net, the exact data
		// loss the store exists to prevent.
		pinned := pinnedIndices(revs)
		var kept []Revision
		for i, r := range revs {
			if r.Timestamp >= cutoff || pinned[i] {
				kept = append(kept, r)
			}
		}
		if len(kept) == len(revs) {
			for _, r := range kept {
				referenced[r.Hash] = struct{}{}
			}
			continue
		}
		if err := s.rewriteLog(relPath, kept); err != nil {
			return err
		}
		for _, r := range kept {
			referenced[r.Hash] = struct{}{}
		}
	}

	// Delete objects nobody references anymore.
	return s.sweepUnreferenced(referenced)
}

// sweepUnreferenced removes every object whose hash is absent from referenced
// (the set of hashes still named by some surviving revision). It is the shared
// object-collection sweep used by both GC (by age) and GCBySize (by size): an
// object is freed if and only if no surviving revision of any log references it,
// so the caller's `referenced` set is the implicit reference count. Skips the
// in-flight capture temps (".tmp-*") and any nested dirs. A missing objects/ dir
// is not an error (nothing was ever captured).
func (s *FS) sweepUnreferenced(referenced map[string]struct{}) error {
	entries, err := os.ReadDir(s.objectsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if _, ok := referenced[e.Name()]; !ok {
			if err := os.Remove(s.objectPath(e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// objectSize returns the on-disk size of the object named by hash, or 0 if it
// cannot be stat'd (e.g. a referenced object missing from a corrupt store, which
// must not abort a GC). Measuring by Stat — not Revision.Bytes — is deliberate:
// the object is the physically freeable unit and is shared across revisions,
// whereas Bytes is the per-revision content length and is absent on legacy lines.
func (s *FS) objectSize(hash string) int64 {
	fi, err := os.Stat(s.objectPath(hash))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// evMember names one revision inside an eviction unit.
type evMember struct {
	relPath string
	ts      int64
	hash    string
}

// pinnedIndices marks the revisions retention must never evict from a single
// file's oldest-first history: P1 pins the newest revision; P2 also pins the
// pre-restore immediately preceding a pinned newest restore. It is the floor
// shared by age GC and (in unit form) buildEvictionUnits.
func pinnedIndices(revs []Revision) map[int]bool {
	pinned := map[int]bool{}
	if len(revs) == 0 {
		return pinned
	}
	newest := len(revs) - 1
	pinned[newest] = true
	if revs[newest].Label == LabelRestore && newest-1 >= 0 && revs[newest-1].Label == LabelPreRestore {
		pinned[newest-1] = true
	}
	return pinned
}

// buildEvictionUnits returns the eviction units in global oldest-first order by
// (timestamp, relPath). A unit is a slice of revisions evicted together: a single
// revision, or an atomic pre-restore/restore pair. The pinning rules encode the
// product's two safety invariants:
//
//	P1 — each file's newest revision (oldest-first: the last element) is pinned
//	     and never appears in a unit, so no file is ever stripped of its last net.
//	P2 — a pre-restore/restore pair (the restore is the pre-restore's immediate
//	     successor; Restore writes them back-to-back under s.mu) is one atomic
//	     unit. If that restore is itself the file's newest (pinned by P1), the pair
//	     cannot be evicted, so the pre-restore is pinned by extension — evicting it
//	     alone would orphan a live restore and make a recorded restore irreversible.
func (s *FS) buildEvictionUnits(perFile map[string][]Revision) [][]evMember {
	var units [][]evMember
	for relPath, revs := range perFile {
		newest := len(revs) - 1 // P1: the newest revision is pinned
		for i := 0; i < newest; i++ {
			r := revs[i]
			if r.Label == LabelPreRestore && i+1 < len(revs) && revs[i+1].Label == LabelRestore {
				if i+1 == newest {
					// The restore is this file's newest (pinned): the pair cannot be
					// evicted, so the pre-restore is pinned too. Both skipped.
					continue
				}
				units = append(units, []evMember{
					{relPath, r.Timestamp, r.Hash},
					{relPath, revs[i+1].Timestamp, revs[i+1].Hash},
				})
				i++ // the restore is consumed into this atomic unit
				continue
			}
			units = append(units, []evMember{{relPath, r.Timestamp, r.Hash}})
		}
	}
	// Deterministic global order: by each unit's oldest member (its [0]), then
	// relPath to break ties between files sharing a timestamp.
	sort.Slice(units, func(a, b int) bool {
		ua, ub := units[a][0], units[b][0]
		if ua.ts != ub.ts {
			return ua.ts < ub.ts
		}
		return ua.relPath < ub.relPath
	})
	return units
}

// GCBySize enforces a maximum total size on the object store, evicting the oldest
// revisions until the referenced objects fit within maxBytes. It returns the
// store's referenced size AFTER eviction so the caller can tell the two
// legitimate outcomes apart:
//
//	finalBytes <= maxBytes  the budget was met.
//	finalBytes >  maxBytes  the P1/P2 floor was reached — the store cannot shrink
//	                        further without dropping a file's last net (P1) or
//	                        orphaning a recorded restore (P2). NOT an error.
//
// Size is measured over OBJECTS (the only thing that occupies disk),
// deduplicated by content hash: an object is freed only when its last
// referencing revision is evicted, so a blob shared by two revisions survives
// until both are gone. Eviction order is global oldest-first by (timestamp,
// relPath). Composes after GC(maxAge): prune by age first, then cap what remains.
func (s *FS) GCBySize(maxBytes int64) (int64, error) {
	var final int64
	err := s.withWriteLock(func() error {
		f, e := s.gcBySizeLocked(maxBytes)
		final = f
		return e
	})
	return final, err
}

// gcBySizeLocked is the body of GCBySize; the caller holds the write lock. Like
// gcLocked it holds the lock for its whole run (see that note on GC's hold).
func (s *FS) gcBySizeLocked(maxBytes int64) (int64, error) {
	logs, err := s.allLogs()
	if err != nil {
		return 0, err
	}

	// Load every file's revisions (oldest-first) and build the implicit reference
	// count: how many live revisions name each object hash, plus each object's
	// on-disk size. Deletions (empty hash) reference no object. total is the
	// deduped sum — each object counted once.
	perFile := make(map[string][]Revision, len(logs))
	refcount := map[string]int{}
	size := map[string]int64{}
	var total int64
	for _, relPath := range logs {
		revs, err := s.readLog(relPath)
		if err != nil {
			return 0, err
		}
		if len(revs) == 0 {
			continue
		}
		perFile[relPath] = revs
		for _, r := range revs {
			if r.Hash == "" {
				continue
			}
			if refcount[r.Hash] == 0 {
				sz := s.objectSize(r.Hash)
				size[r.Hash] = sz
				total += sz
			}
			refcount[r.Hash]++
		}
	}

	if total <= maxBytes {
		return total, nil // already within budget: no-op
	}

	// Walk the eviction units oldest-first, decrementing the refcount as each
	// revision is removed and subtracting an object's size the moment its last
	// reference disappears. Pinned revisions (P1/P2) are absent from `units`, so
	// the walk skips them; when units run out while still over budget, the floor
	// has been reached.
	removed := map[string]map[int64]bool{} // relPath -> set of removed timestamps
	for _, u := range s.buildEvictionUnits(perFile) {
		if total <= maxBytes {
			break
		}
		for _, m := range u {
			if refcount[m.hash] > 0 {
				refcount[m.hash]--
				if refcount[m.hash] == 0 {
					total -= size[m.hash]
				}
			}
			if removed[m.relPath] == nil {
				removed[m.relPath] = map[int64]bool{}
			}
			removed[m.relPath][m.ts] = true
		}
	}

	// Rewrite only the logs that lost revisions; `kept` is never empty because P1
	// pins each file's newest revision. Collect the surviving hashes for the sweep.
	referenced := map[string]struct{}{}
	for relPath, revs := range perFile {
		rm := removed[relPath]
		if len(rm) == 0 {
			for _, r := range revs {
				if r.Hash != "" {
					referenced[r.Hash] = struct{}{}
				}
			}
			continue
		}
		kept := make([]Revision, 0, len(revs))
		for _, r := range revs {
			if rm[r.Timestamp] {
				continue
			}
			kept = append(kept, r)
			if r.Hash != "" {
				referenced[r.Hash] = struct{}{}
			}
		}
		if err := s.rewriteLog(relPath, kept); err != nil {
			return total, err
		}
	}

	if err := s.sweepUnreferenced(referenced); err != nil {
		return total, err
	}
	return total, nil
}

// rewriteLog atomically replaces a log with kept (or removes it if empty).
func (s *FS) rewriteLog(relPath string, kept []Revision) error {
	lp := s.logPath(relPath)
	if len(kept) == 0 {
		return os.Remove(lp)
	}
	var b strings.Builder
	for _, r := range kept {
		b.WriteString(formatLine(r)) // preserves each revision's signal (or its absence)
	}
	tmp, err := os.CreateTemp(filepath.Dir(lp), ".tmp-log-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// On any failure below, best-effort remove the temp; the returned error is
	// the real cause, so a failed cleanup must not mask it.
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, lp)
}

// allLogs returns every tracked relPath (logs under index/, .log stripped).
func (s *FS) allLogs() ([]string, error) {
	var out []string
	idx := s.indexDir()
	err := filepath.WalkDir(idx, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".log") {
			return nil
		}
		rel, err := filepath.Rel(idx, path)
		if err != nil {
			return err
		}
		out = append(out, strings.TrimSuffix(rel, ".log"))
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}
