// Package store holds the per-file revision history under .lochis/.
//
// Layout on disk:
//
//	.lochis/
//	├── objects/<sha256>        full content, deduplicated by hash
//	└── index/<relpath>.log     one line per revision of that file
//
// The data is readable without this tool: `ls` and `cat` are enough to
// recover any version by hand.
package store

import (
	"bufio"
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
const Dir = ".lochis"

// Label classifies why a revision was recorded.
type Label string

const (
	LabelInitial    Label = "initial"
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

// New returns a Store rooted at root. The .lochis/ directory is created lazily.
func New(root string) *FS {
	return &FS{root: root}
}

// Init eagerly creates the .lochis/ skeleton (objects/ and index/). The watcher
// calls this on startup so `lochis watch` materializes the history directory
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
			return s.appendLog(relPath, force, h)
		}
		if !hasLast || last.Label == LabelDelete {
			return nil // never tracked, or already known deleted
		}
		return s.appendLog(relPath, LabelDelete, last.Hash)
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
	return s.appendLog(relPath, label, hash)
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

// appendLog appends one revision line to the file's .log, O_APPEND, with a
// timestamp guaranteed strictly greater than the previous line's so every
// revision of a file is uniquely addressable by timestamp.
func (s *FS) appendLog(relPath string, label Label, hash string) error {
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
	_, err = fmt.Fprintf(f, "%d\t%s\t%s\n", ts, hash, label)
	return err
}

// parseLine parses one .log line. ok is false for malformed lines (which are
// tolerated, e.g. a final line truncated by a crash).
func parseLine(line string) (Revision, bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) != 3 {
		return Revision{}, false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Revision{}, false
	}
	return Revision{Timestamp: ts, Hash: parts[1], Label: Label(parts[2])}, true
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
	preTs, _ := s.lastRevision(relPath)

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
		if err := s.appendLog(relPath, LabelRestore, target.Hash); err != nil {
			return 0, err
		}
		return preTs.Timestamp, nil
	}

	content, err := os.ReadFile(s.objectPath(target.Hash))
	if err != nil {
		return 0, err
	}

	// 3. Overwrite the working-tree file atomically.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".lochis-restore-*")
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
	if err := s.appendLog(relPath, LabelRestore, target.Hash); err != nil {
		return 0, err
	}
	return preTs.Timestamp, nil
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
		fmt.Fprintf(&b, "%d\t%s\t%s\n", r.Timestamp, r.Hash, r.Label)
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
