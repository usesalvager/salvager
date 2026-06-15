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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dir is the name of the history directory created inside a project root.
const Dir = ".salvager"

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
	if err := os.MkdirAll(s.objectsDir(), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(s.indexDir(), 0o755)
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

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Record captures the current state of relPath if it differs from the last
// recorded revision. Deduplicated by content hash. A missing file is recorded
// as a delete (no new object).
func (s *FS) Record(relPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record(relPath, "")
}

// record does the work; label forces a specific label (used by Restore for
// pre-restore). Empty label means: pick initial/modify/delete automatically.
// Caller holds s.mu.
func (s *FS) record(relPath string, force Label) error {
	abs := filepath.Join(s.root, relPath)
	content, err := os.ReadFile(abs)

	last, hasLast := s.lastRevision(relPath)

	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// File is gone from disk.
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

	hash := sha256hex(content)
	if force == "" && hasLast && last.Hash == hash && last.Label != LabelDelete {
		return nil // unchanged: dedup, record nothing
	}

	if err := s.writeObject(hash, content); err != nil {
		return err
	}

	label := force
	if label == "" {
		if hasLast {
			label = LabelModify
		} else {
			label = LabelInitial
		}
	}
	return s.appendLog(relPath, label, hash, computeSig(content, last, hasLast))
}

// writeObject stores content at objects/<hash>, atomically, once. No-op if the
// object already exists (content-addressed: same hash == same bytes).
func (s *FS) writeObject(hash string, content []byte) error {
	path := s.objectPath(hash)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.objectsDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.objectsDir(), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
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
	switch {
	case !hasLast:
		rs.delta, rs.deltaKnown = n, true // first revision: all lines are new
	case last.HasSignal:
		rs.delta, rs.deltaKnown = n-last.Lines, true
	default:
		rs.deltaKnown = false // previous revision has no signal to diff against
	}
	return rs
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
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}
	ts := nowFunc()
	if last, ok := s.lastRevision(relPath); ok && ts <= last.Timestamp {
		ts = last.Timestamp + 1
	}
	f, err := os.OpenFile(lp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Safeguard: record current state, forced (even if hash is unchanged).
	if err := s.record(relPath, LabelPreRestore); err != nil {
		return 0, fmt.Errorf("pre-restore safeguard failed, aborting: %w", err)
	}
	pre, _ := s.lastRevision(relPath) // the safeguard becomes this restore's predecessor

	// 2. Read the requested revision.
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

	abs := filepath.Join(s.root, relPath)

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
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
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
	s.mu.Lock()
	defer s.mu.Unlock()

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
		var kept []Revision
		for _, r := range revs {
			if r.Timestamp >= cutoff {
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
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
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
