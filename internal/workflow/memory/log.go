package memory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The on-disk log. One directory per ROOT run, one append-only NDJSON file in
// it, and nothing else the app writes: the digest a prompt carries is rendered
// live from this file, so there is no second artifact to keep in step with it.
//
// The tree lives under the app's config root rather than in the repository or a
// worktree. A campaign's memory is not part of its deliverable — putting it on
// the branch would make every lane's merge carry it, and every discard delete
// it — and the tree outlives the run for the same reason: a finished campaign's
// memory IS its record.

// DirName is the config-root-relative directory holding every run tree's memory.
const DirName = "workflow-memory"

// NotesFileName is the append-only log inside one tree.
const NotesFileName = "notes.ndjson"

const (
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

// Dir returns the absolute memory directory of one root run. The id is
// validated as a single path segment: it is an app-minted uuid at every call
// site, so a value that is not can only be a bug, and the check belongs where
// the path is built rather than in each caller.
func Dir(configRoot, rootRunID string) (string, error) {
	if strings.TrimSpace(configRoot) == "" {
		return "", fmt.Errorf("workflow memory directory: config root is required")
	}
	if strings.TrimSpace(rootRunID) == "" {
		return "", fmt.Errorf("workflow memory directory: root run id is required")
	}
	if rootRunID != filepath.Base(rootRunID) || rootRunID == "." || rootRunID == ".." ||
		strings.ContainsRune(rootRunID, filepath.Separator) {
		return "", fmt.Errorf("workflow memory directory: root run id %q is not a single path segment", rootRunID)
	}
	path, err := filepath.Abs(filepath.Join(configRoot, DirName, rootRunID))
	if err != nil {
		return "", fmt.Errorf("workflow memory directory: %w", err)
	}
	return path, nil
}

// NotesPath returns the absolute log path of one root run's tree.
func NotesPath(configRoot, rootRunID string) (string, error) {
	dir, err := Dir(configRoot, rootRunID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, NotesFileName), nil
}

// EncodeNote renders one note as the single NDJSON line it occupies, newline
// included. The encoder is escape-free so a cited path or a note's prose reads
// back as itself, and `SetEscapeHTML(false)` is what keeps a `<` in a note from
// becoming `<` in the file a human greps.
func EncodeNote(note Note) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(note); err != nil {
		return nil, fmt.Errorf("encode workflow memory note: %w", err)
	}
	// Encode already terminates with exactly one newline, which is the line
	// separator the format is built on.
	return []byte(buffer.String()), nil
}

// Append writes one note to a tree's log, creating the tree lazily.
//
// The write is a single `O_APPEND` write of one whole line. That is what makes
// it crash-safe enough for this format without a temp-and-rename: appending
// through a rename would have to read the whole log back and rewrite it, which
// turns every note into a read-modify-write over a file that grows all campaign.
// A crash mid-write can leave a torn FINAL line, and `ReadNotes` tolerates
// exactly that.
func Append(notesPath string, note Note) error {
	line, err := EncodeNote(note)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(notesPath), dirPerm); err != nil {
		return fmt.Errorf("create workflow memory directory: %w", err)
	}
	file, err := os.OpenFile(notesPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return fmt.Errorf("open workflow memory log %q: %w", notesPath, err)
	}
	unterminated, err := endsWithoutNewline(file)
	if err != nil {
		return errors.Join(fmt.Errorf("append workflow memory note to %q: %w", notesPath, err), file.Close())
	}
	if unterminated {
		// The previous write was torn by a crash and left no line terminator.
		// Appending straight onto it would weld this note to that wreckage and
		// lose BOTH — the tear is meant to cost one note, not every note after
		// it. One leading newline heals the log for good.
		line = append([]byte{'\n'}, line...)
	}
	if _, err := file.Write(line); err != nil {
		return errors.Join(fmt.Errorf("append workflow memory note to %q: %w", notesPath, err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close workflow memory log %q: %w", notesPath, err)
	}
	return nil
}

// endsWithoutNewline reports whether the log's last byte is something other
// than a line terminator, which is the signature of a write torn by a crash.
// An empty (or brand-new) file is terminated by definition.
//
// The read is one byte at a known offset rather than a scan, and the answer can
// only go stale if another writer appends between the stat and the write — which
// would have left a terminator, so the worst case is one blank line the decoder
// already ignores.
func endsWithoutNewline(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// Skipped is one line ReadNotes could not decode.
type Skipped struct {
	// Line is the 1-based line number in the log.
	Line int `json:"line"`
	// Reason is the decode failure, for a log line the caller writes. It never
	// carries the line's content: a corrupt line may be arbitrary bytes.
	Reason string `json:"reason"`
}

// MaxLineBytes bounds one line ReadNotes will buffer. A note is capped at
// MaxTextBytes plus twenty paths plus provenance; this leaves generous room
// above that and still refuses a log that grew a line no writer of ours could
// have produced, rather than allocating for it.
const MaxLineBytes = 64 * 1024

// ReadNotes loads a tree's log, oldest first. A missing file is an empty log,
// not an error: a campaign that has recorded nothing yet is the ordinary state
// of a run's first wave.
//
// An undecodable line is SKIPPED and reported, never fatal. A crash between a
// note's bytes reaching the page cache and the file being durable leaves a torn
// final line, and the whole point of the log is that a campaign's accumulated
// memory survives the crash that truncated one note. The caller logs what was
// skipped; nothing about a skipped line is silent.
func ReadNotes(notesPath string) ([]Note, []Skipped, error) {
	file, err := os.Open(notesPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open workflow memory log %q: %w", notesPath, err)
	}
	defer file.Close()
	notes, skipped, err := DecodeNotes(file)
	if err != nil {
		return nil, nil, fmt.Errorf("read workflow memory log %q: %w", notesPath, err)
	}
	return notes, skipped, nil
}

// DecodeNotes reads an NDJSON log from any reader. It is exported so a test —
// and any future reader that already holds the bytes — shares the one decoder
// with the file path above.
func DecodeNotes(source io.Reader) ([]Note, []Skipped, error) {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 8*1024), MaxLineBytes)
	var (
		notes   []Note
		skipped []Skipped
		number  int
	)
	for scanner.Scan() {
		number++
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var note Note
		decoder := json.NewDecoder(strings.NewReader(string(line)))
		if err := decoder.Decode(&note); err != nil {
			skipped = append(skipped, Skipped{Line: number, Reason: err.Error()})
			continue
		}
		if !KnownKind(note.Kind) || strings.TrimSpace(note.Text) == "" {
			// A well-formed JSON object that is not a note. Reported like a torn
			// line rather than rendered: the digest groups by kind, and a note
			// with no group is one no reader would ever be shown.
			skipped = append(skipped, Skipped{Line: number, Reason: "line is not a workflow memory note"})
			continue
		}
		notes = append(notes, note)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// A line past MaxLineBytes stops the scanner, so the rest of the log
			// would be lost silently. Report it as the read failure it is.
			return nil, nil, fmt.Errorf("line %d exceeds %d bytes", number+1, MaxLineBytes)
		}
		return nil, nil, err
	}
	return notes, skipped, nil
}
