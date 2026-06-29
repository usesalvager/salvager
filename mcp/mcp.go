// Package mcp exposes the store over the Model Context Protocol as a thin
// layer: four tools, all read or restore. No purge or delete is ever exposed —
// the safety net must not be removable by the agent that might break things, and
// every restore is non-destructive (it saves a pre-restore safeguard first).
package mcp

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/usesalvager/salvager/store"
	"github.com/usesalvager/salvager/version"
)

// maxGetBytes bounds the content salvager_get_version returns inline. A single
// revision is loaded, stringified, and JSON-encoded into one MCP frame; without
// a cap a multi-hundred-MB object would blow up memory and the agent's context.
// Larger revisions are inspected via the list signal (lines/delta/signature) or
// read from disk directly.
const maxGetBytes = 5 << 20 // 5 MiB

// Backend is the slice of the store the MCP server consumes.
type Backend interface {
	List(relPath string) ([]store.Revision, error)
	Get(relPath string, ts int64) ([]byte, error)
	Restore(relPath string, ts int64) (preRestoreTs int64, err error)
	RestoreAt(relDir string, atMs int64) (batchStart, batchEnd int64, results []store.RestoreResult, err error)
}

// --- tool I/O types (json + jsonschema tags drive the wire schema) ---

type listInput struct {
	File string `json:"file" jsonschema:"project-relative path of the file"`
}

type versionInfo struct {
	TimestampHuman string `json:"timestamp_human"`
	Timestamp      int64  `json:"timestamp"`
	HashShort      string `json:"hash_short"`
	Label          string `json:"label"`

	// Content signal, computed at capture and read here without opening the
	// object. has_signal is false for legacy revisions recorded before the
	// signal existed; their lines/bytes/delta/signature are omitted.
	HasSignal  bool   `json:"has_signal"`
	Lines      int    `json:"lines,omitempty"`
	Bytes      int    `json:"bytes,omitempty"`
	DeltaLines string `json:"delta_lines,omitempty"` // "+N" / "-N" vs the previous version; "?" if unknowable
	Signature  string `json:"signature,omitempty"`   // first non-empty lines; "" for a deletion or binary
}

type listOutput struct {
	// File echoes the queried path and Tracked says whether any history exists
	// for it. An empty Versions list is a success, not an error (a file simply
	// has no recorded history yet); Tracked makes that explicit so an agent can
	// tell "no history" apart from a failed call without inferring it from an
	// empty array.
	File     string        `json:"file"`
	Tracked  bool          `json:"tracked"`
	Versions []versionInfo `json:"versions"`
}

type getInput struct {
	File      string `json:"file" jsonschema:"project-relative path of the file"`
	Timestamp int64  `json:"timestamp" jsonschema:"timestamp of the revision to read"`
}

type getOutput struct {
	Content string `json:"content"`
}

type restoreInput struct {
	File      string `json:"file" jsonschema:"project-relative path of the file"`
	Timestamp int64  `json:"timestamp" jsonschema:"timestamp of the revision to restore"`
}

type restoreOutput struct {
	OK                  bool  `json:"ok"`
	PreRestoreTimestamp int64 `json:"pre_restore_timestamp"`
}

type restoreAtInput struct {
	Timestamp int64  `json:"timestamp" jsonschema:"restore each file to its state at or before this millisecond timestamp"`
	Path      string `json:"path,omitempty" jsonschema:"project-relative directory to limit the restore to; omit or \".\" for the whole project"`
}

// restoreAtFile is one file's outcome, mirroring store.RestoreResult on the wire.
type restoreAtFile struct {
	Path string `json:"path"`
	// Action is one of: "restored", "unchanged", "skipped-no-revision",
	// "skipped-deletion" (see store.Action* — the latter two are how the
	// non-destructive contract is reported, not failures).
	Action string `json:"action"`
	// RestoredToTimestamp is the <=timestamp revision the file landed on; differs
	// from the requested timestamp when that exact instant was garbage-collected.
	RestoredToTimestamp int64 `json:"restored_to_timestamp,omitempty"`
	// PreRestoreTimestamp is this file's undo point: salvager_restore <path> with
	// it reverts just this file. 0 when nothing was written.
	PreRestoreTimestamp int64 `json:"pre_restore_timestamp,omitempty"`
}

type restoreAtOutput struct {
	OK            bool `json:"ok"`
	RestoredCount int  `json:"restored_count"`
	// BatchStart/BatchEnd bound the pre-restore timestamps this batch wrote — the
	// window an undo would target. Returned for completeness; per-file undo uses
	// each file's pre_restore_timestamp.
	BatchStart int64           `json:"batch_start"`
	BatchEnd   int64           `json:"batch_end"`
	Files      []restoreAtFile `json:"files"`
}

func human(tsMillis int64) string {
	return time.UnixMilli(tsMillis).Format("2006-01-02 15:04:05")
}

func short(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// NewServer builds the MCP server backed by b.
func NewServer(b Backend) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "salvager", Version: version.Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "salvager_list_versions",
		Description: "List the recorded versions of a file, most recent first. " +
			"Each version carries a content signal computed when it was captured — total " +
			"lines, the line delta vs the previous version (delta_lines, e.g. \"+21\"/\"-21\"), " +
			"and the first non-empty lines (signature) — so you can tell which version contains " +
			"a given function or block WITHOUT calling salvager_get_version. " +
			"The oldest version is labeled \"first-seen\": it already holds the work captured when " +
			"salvager first saw the file — it is NOT an empty baseline, so inspect it like any other. " +
			"Before concluding that something was never added or no longer exists, use the signal " +
			"(especially delta_lines and signature) to find the version that has it; a large positive " +
			"delta marks where lines were added and a negative delta where they were removed.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
		revs, err := b.List(in.File)
		if err != nil {
			return nil, listOutput{}, err
		}
		out := listOutput{
			File:     in.File,
			Tracked:  len(revs) > 0,
			Versions: make([]versionInfo, 0, len(revs)),
		}
		for _, r := range revs {
			vi := versionInfo{
				TimestampHuman: human(r.Timestamp),
				Timestamp:      r.Timestamp,
				HashShort:      short(r.Hash),
				Label:          string(r.Label),
				HasSignal:      r.HasSignal,
			}
			if r.HasSignal {
				vi.Lines = r.Lines
				vi.Bytes = r.Bytes
				vi.DeltaLines = r.DeltaString()
				vi.Signature = r.Sig
			}
			out.Versions = append(out.Versions, vi)
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "salvager_get_version",
		Description: "Return the content of a specific recorded version, so it can be inspected before acting.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
		content, err := b.Get(in.File, in.Timestamp)
		if err != nil {
			return nil, getOutput{}, err
		}
		if len(content) > maxGetBytes {
			return nil, getOutput{}, fmt.Errorf("revision %d of %s is %d bytes, over the %d-byte inline limit; use salvager_list_versions for its signal or read the object from disk", in.Timestamp, in.File, len(content), maxGetBytes)
		}
		// Binary content cannot survive a round-trip through a JSON string field
		// (invalid UTF-8 is silently replaced); refuse rather than hand back
		// corrupted bytes the agent would mistake for the real revision.
		if !utf8.Valid(content) {
			return nil, getOutput{}, fmt.Errorf("revision %d of %s is binary (non-UTF-8); cannot return as text", in.Timestamp, in.File)
		}
		return nil, getOutput{Content: string(content)}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "salvager_restore",
		Description: "Restore a file to a recorded version. The current state is saved first (pre-restore), so this is reversible.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in restoreInput) (*mcp.CallToolResult, restoreOutput, error) {
		preTs, err := b.Restore(in.File, in.Timestamp)
		if err != nil {
			return nil, restoreOutput{}, err
		}
		return nil, restoreOutput{OK: true, PreRestoreTimestamp: preTs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "salvager_restore_at",
		Description: "Point-in-time batch restore: rewind a whole SET of files at once to their state " +
			"at or before a timestamp — the recovery for a bulk command that wiped or clobbered many " +
			"files together (a parallel agent running `git clean -fd` / `git reset --hard` / `git checkout -f`). " +
			"Restore them as one batch instead of one salvager_restore per file. " +
			"`path` limits the restore to a project-relative subtree (omit for the whole project); `timestamp` " +
			"is the instant to rewind to (use salvager_list_versions to find a good one — pick just BEFORE the damage). " +
			"NON-DESTRUCTIVE: a file created after that instant is left untouched, a file whose state then was a " +
			"deletion is left in place (never removed), and only files whose recorded content differs from disk are " +
			"rewritten (including one the bad command deleted from disk — it is brought back). Each rewritten file " +
			"first saves a pre_restore safeguard, so the whole batch is reversible: undo one file with salvager_restore " +
			"using its pre_restore_timestamp. The result lists every file with its action and timestamps.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in restoreAtInput) (*mcp.CallToolResult, restoreAtOutput, error) {
		start, end, results, err := b.RestoreAt(in.Path, in.Timestamp)
		if err != nil {
			// Honest atomicity: a mid-batch failure leaves the files restored so far
			// individually reversible via their pre_restore safeguards, but the partial
			// set is not returned here. Surface the error; recover per file with
			// salvager_list_versions + salvager_restore.
			return nil, restoreAtOutput{}, err
		}
		out := restoreAtOutput{
			OK:         true,
			BatchStart: start,
			BatchEnd:   end,
			Files:      make([]restoreAtFile, 0, len(results)),
		}
		for _, r := range results {
			if r.Action == store.ActionRestored {
				out.RestoredCount++
			}
			out.Files = append(out.Files, restoreAtFile{
				Path:                r.Path,
				Action:              r.Action,
				RestoredToTimestamp: r.RestoredToTs,
				PreRestoreTimestamp: r.PreRestoreTs,
			})
		}
		return nil, out, nil
	})

	return s
}

// Serve runs the MCP server over stdio until the client disconnects or ctx is
// cancelled.
func Serve(ctx context.Context, b Backend) error {
	return NewServer(b).Run(ctx, &mcp.StdioTransport{})
}
