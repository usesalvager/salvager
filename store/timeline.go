package store

import "sort"

// RevisionEvent is one recorded revision tagged with the file it belongs to, for
// read-only timeline scans across the whole tree.
type RevisionEvent struct {
	Path string
	Revision
}

// ScanRevisions returns every recorded revision of every tracked file under
// relDir — including deletions and files whose latest revision is a deletion —
// ordered by timestamp (ties broken by path). It is read-only: it opens no
// object, takes no lock, and creates nothing. relDir "" or "." means the whole
// tree. Reads only the index/ side (small log files), so it stays cheap.
func (s *FS) ScanRevisions(relDir string) ([]RevisionEvent, error) {
	relDir, err := s.cleanContainedDir(relDir)
	if err != nil {
		return nil, err
	}
	logs, err := s.allLogs()
	if err != nil {
		return nil, err
	}
	var out []RevisionEvent
	for _, relPath := range logs {
		if !underDir(relPath, relDir) {
			continue
		}
		revs, err := s.readLog(relPath)
		if err != nil {
			return nil, err
		}
		for _, r := range revs {
			out = append(out, RevisionEvent{Path: relPath, Revision: r})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Timestamp != out[j].Timestamp {
			return out[i].Timestamp < out[j].Timestamp
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Burst is a cluster of destructive revisions close in time — the signature of a
// runaway bulk command (git clean -fd / checkout -f / reset --hard) that wiped or
// shrank many files at once. RestoreAt is the instant to hand to `restore-at` to
// rewind every affected file to just before the damage.
type Burst struct {
	Start, End int64    // timestamp span of the destructive events (ms)
	Files      []string // distinct files hit, sorted
	Deletes    int      // how many of the events were deletions
	RestoreAt  int64    // recommended `restore-at` instant: Start-1 (just before)
}

// Burst-detection tuning. ponytail: hardcoded heuristics; promote to flags only
// if real use shows the defaults misfire.
const (
	burstMinFiles = 3    // a burst needs at least this many distinct files
	burstGapMs    = 2000 // events more than this far apart start a new cluster
	burstShrink   = -50  // a modify dropping >= this many lines counts as destructive
)

// destructive reports whether a revision is the kind of change a runaway bulk
// command produces: a deletion, or a content edit that dropped a large number of
// lines. salvager's own restore bookkeeping (restore / pre-restore) and the
// first-seen baseline are never destructive, so a prior restore-at batch is not
// mistaken for fresh damage.
func destructive(r Revision) bool {
	switch r.Label {
	case LabelDelete:
		return true
	case LabelModify:
		return r.HasSignal && r.DeltaKnown && r.Delta <= burstShrink
	default:
		return false
	}
}

// DetectBursts groups the destructive revisions in events — which must be
// timestamp-ordered, as ScanRevisions returns — into bursts: maximal runs whose
// consecutive destructive events are <= burstGapMs apart and which hit at least
// burstMinFiles distinct files. Distinct files (not events) gate a burst, so a
// loop rewriting one file many times is not flagged. Pure; no I/O.
func DetectBursts(events []RevisionEvent) []Burst {
	var bursts []Burst
	var cluster []RevisionEvent

	flush := func() {
		if len(cluster) == 0 {
			return
		}
		files := map[string]bool{}
		deletes := 0
		for _, e := range cluster {
			files[e.Path] = true
			if e.Label == LabelDelete {
				deletes++
			}
		}
		if len(files) >= burstMinFiles {
			names := make([]string, 0, len(files))
			for f := range files {
				names = append(names, f)
			}
			sort.Strings(names)
			start := cluster[0].Timestamp
			bursts = append(bursts, Burst{
				Start:     start,
				End:       cluster[len(cluster)-1].Timestamp,
				Files:     names,
				Deletes:   deletes,
				RestoreAt: start - 1,
			})
		}
		cluster = nil
	}

	for _, e := range events {
		if !destructive(e.Revision) {
			continue
		}
		if len(cluster) > 0 && e.Timestamp-cluster[len(cluster)-1].Timestamp > burstGapMs {
			flush()
		}
		cluster = append(cluster, e)
	}
	flush()
	return bursts
}
