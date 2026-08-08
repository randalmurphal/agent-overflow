package sessionimport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider/claude/sessionfork"
)

// LoadedBranch is one importable thread: the branch, the events the writer
// will turn into rows, and whatever converting it warned about.
//
// Its Chain is the SKELETON chain — the decoded line bodies live only for
// the duration of ConvertBranch. A caller that holds every branch of a
// large transcript therefore holds kilobytes, not gigabytes.
type LoadedBranch struct {
	Branch
	Events   []importir.Event
	Warnings []importir.Warning
}

// LoadedSession is an OPEN transcript: every branch in it, plus the file
// handle a per-branch conversion reads its rows back from. Callers must
// Close it.
//
// Conversion is deliberately LAZY and per-branch. A transcript with four
// leaves converted eagerly holds four branches' events at once, on top of
// the whole file decoded — which is how a 220 MB session became most of a
// gigabyte. Here the caller converts one branch, applies it, and lets it go
// before asking for the next.
type LoadedSession struct {
	SessionID string
	Path      string
	// SessionDir is the per-session sidecar directory
	// (`<projectDir>/<sessionID>`) holding `subagents/` and
	// `tool-results/`.
	SessionDir string
	// Branches are the skeleton branches, ordered by leaf file position.
	// The file's ACTIVE branch — the one `claude --resume` reopens — is the
	// last.
	Branches []Branch
	// Warnings are the file-level ones: the scan's and the DAG's. A
	// branch's own conversion warnings ride on its LoadedBranch.
	Warnings []importir.Warning

	file *os.File
}

// LoadSession opens one Claude transcript and enumerates its branches.
//
// It reads the file once, keeping only a skeleton per row (see
// transcript.go); no line body survives the call. ConvertBranch is what
// decodes rows, and only the ones on the branch it is asked for.
//
// A file past maxTranscriptBytes is refused with user-facing prose rather
// than read: the refusal is per session, so an "Import All" carries on.
func LoadSession(path string) (*LoadedSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open claude transcript: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			f.Close()
		}
	}()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat claude transcript: %w", err)
	}
	if stat.Size() > maxTranscriptBytes {
		return nil, &transcriptTooLargeError{path: path, size: stat.Size()}
	}

	skeleton, err := scanTranscript(f)
	if err != nil {
		return nil, fmt.Errorf("read claude transcript %s: %w", path, err)
	}

	sessionID := sessionfork.SessionIDFromPath(path)
	loaded := &LoadedSession{
		SessionID:  sessionID,
		Path:       path,
		SessionDir: filepath.Join(filepath.Dir(path), sessionID),
		Warnings:   skeleton.Warnings,
		file:       f,
	}

	branches, warnings := BuildBranches(skeleton.Rows, skeleton.LeafTitles)
	loaded.Warnings = append(loaded.Warnings, warnings...)
	loaded.Branches = branches

	closeOnError = false
	return loaded, nil
}

// Close releases the transcript handle. Safe on a nil session so callers
// can `defer loaded.Close()` next to the error check.
func (s *LoadedSession) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// ConvertBranch decodes one branch's rows and projects them into events.
//
// This is pass 2. It re-reads exactly the lines the branch's chain names —
// their byte ranges came off the same read that built the skeleton — joins
// the subagent transcripts those rows launched, converts, and drops every
// decoded body before returning. Subagent transcripts are read per branch
// rather than once per file: branches share a prefix, so a Task launch can
// appear on several, and each of them is its own thread that should nest
// the agent's rows under its own launch row.
func (s *LoadedSession) ConvertBranch(index int) (LoadedBranch, error) {
	if s == nil || s.file == nil {
		return LoadedBranch{}, fmt.Errorf("sessionimport: transcript is not open")
	}
	if index < 0 || index >= len(s.Branches) {
		return LoadedBranch{}, fmt.Errorf(
			"sessionimport: branch %d is out of range for %s (%d branches)",
			index, s.Path, len(s.Branches))
	}
	branch := s.Branches[index]
	chain, err := s.decodeChain(branch.Chain)
	if err != nil {
		return LoadedBranch{}, err
	}
	if branch.Title == "" {
		branch.Title = lastUserPromptText(chain)
	}

	decoded := branch
	decoded.Chain = chain
	subagents, warnings := LoadSubagents(s.SessionDir, []Branch{decoded})

	events, convertWarnings := Convert(chain, ConvertOptions{
		SessionDir: s.SessionDir,
		Subagents:  subagents,
	})
	return LoadedBranch{
		Branch:   branch,
		Events:   events,
		Warnings: append(warnings, convertWarnings...),
	}, nil
}

// decodeChain reads each skeleton row's own line back and decodes it,
// leaving the caller's skeleton untouched.
//
// A row that decoded in pass 1 and does not decode now means the file
// changed under us; that is an error rather than a dropped row, because a
// branch missing an arbitrary row imports a conversation that never
// happened.
func (s *LoadedSession) decodeChain(skeleton []Row) ([]Row, error) {
	out := make([]Row, len(skeleton))
	var buf []byte
	for i, row := range skeleton {
		out[i] = row
		if row.Raw != nil || row.Length <= 0 {
			// Already decoded (a caller that built the DAG from full rows),
			// or a row with no recorded extent.
			continue
		}
		if cap(buf) < row.Length {
			buf = make([]byte, row.Length)
		}
		line := buf[:row.Length]
		if _, err := s.file.ReadAt(line, row.Offset); err != nil {
			return nil, fmt.Errorf(
				"sessionimport: re-read row %s of %s at offset %d: %w",
				row.UUID, s.Path, row.Offset, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, fmt.Errorf(
				"sessionimport: row %s of %s no longer decodes (the file changed while it was being read): %w",
				row.UUID, s.Path, err)
		}
		out[i].Raw = raw
	}
	return out, nil
}
