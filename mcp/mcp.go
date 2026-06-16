// Package mcp exposes the store over the Model Context Protocol as a thin
// layer: exactly three tools, all read or restore. No purge or delete is ever
// exposed — the safety net must not be removable by the agent that might break
// things.
package mcp

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/usesalvager/salvager/store"
	"github.com/usesalvager/salvager/version"
)

// Backend is the slice of the store the MCP server consumes.
type Backend interface {
	List(relPath string) ([]store.Revision, error)
	Get(relPath string, ts int64) ([]byte, error)
	Restore(relPath string, ts int64) (preRestoreTs int64, err error)
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

	return s
}

// Serve runs the MCP server over stdio until the client disconnects or ctx is
// cancelled.
func Serve(ctx context.Context, b Backend) error {
	return NewServer(b).Run(ctx, &mcp.StdioTransport{})
}
