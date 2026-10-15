package settings

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agent-overflow/internal/atomicfile"

	"github.com/google/uuid"
)

// Prompt files: each prompt override's text lives only in
// <configDir>/prompts/<id>.md, which the user or an agent edits as a file.
//
// The settings row keeps the id, Enabled and Models. The load fills Prompt
// from the file in memory; it never crosses the wire and a write never takes
// it from a caller. The only files the service writes are an added entry's
// empty file (and, once, the migration of inline text); it removes the file
// of an entry the write drops. Path and Problem are computed on load and
// never persisted.
//
// All of this applies once the tier store is attached. Before that, the
// user tier still lives in settings.json and the pre-database boot readers
// see the rows as they are; MigratePromptFiles moves inline text into
// files after AttachTierStore.

const (
	promptFilesDirName = "prompts"
	promptFileExt      = ".md"
	// PromptReadmeName is the reference the app writes into the prompts
	// directory at every boot (promptoverride.WriteReadme). It is not an
	// entry's file: entry files are named by UUID.
	PromptReadmeName = "README.md"
)

// promptOverrideKeys are the settings keys whose entries own prompt files.
var promptOverrideKeys = [...]string{"claudePromptOverrides", "codexPromptOverrides"}

// IsPromptOverrideKey reports whether key is one of the two prompt override
// lists.
func IsPromptOverrideKey(key string) bool {
	return slices.Contains(promptOverrideKeys[:], key)
}

// promptFileStamp records one prompt file's state at load time.
type promptFileStamp struct {
	path  string
	state fileState
}

// promptLoad is what one load learned about the prompt files: the stamps
// the cache is checked against, and the keys whose stored rows still carry
// inline text, which the next write persists stripped.
type promptLoad struct {
	stamps     []promptFileStamp
	inlineKeys []string
}

// promptEntryState is one entry's text and problem before a mutation, keyed
// by id. Taken before apply runs, because apply may edit entries in place.
type promptEntryState struct {
	prompt  string
	problem string
}

// promptFileWrite is one prompt file a mutation writes.
type promptFileWrite struct {
	path string
	text string
}

// PromptFilesDir returns <configDir>/prompts.
func (s *Service) PromptFilesDir() string {
	return s.promptDir
}

func (s *Service) promptFilePath(id string) string {
	return filepath.Join(s.promptDir, id+promptFileExt)
}

// ReloadPromptFiles drops the cached snapshot and announces both prompt
// keys as a user-tier change, so attached clients refetch. The prompts
// watcher calls it after an external edit.
func (s *Service) ReloadPromptFiles() {
	s.mu.Lock()
	s.cached = nil
	s.cachedPromptFiles = nil
	s.mu.Unlock()
	if observe := s.changeObserver(); observe != nil {
		observe([]TierChange{{Tier: TierUser, Keys: slices.Clone(promptOverrideKeys[:])}})
	}
}

// MigratePromptFiles writes the prompt file of every entry that still keeps
// its text in the settings row, minting the ids those entries lack, and
// persists the rows without the text. A no-op when nothing is pending, so
// it writes no settings file at an ordinary boot.
//
// Call it after AttachTierStore: before that the user tier is still in
// settings.json and nothing may be moved out of it.
func (s *Service) MigratePromptFiles() error {
	s.mu.Lock()
	pending := false
	if s.store != nil {
		current := s.loadFromFile()
		pending = len(s.promptLoad.inlineKeys) > 0 || promptOverridesLackID(current)
	}
	s.mu.Unlock()
	if !pending {
		return nil
	}
	_, err := s.mutate("", DeviceDesktop, func(current Settings) (Settings, error) {
		return current, nil
	})
	if err != nil {
		return fmt.Errorf("settings: migrate prompt overrides to files: %w", err)
	}
	return nil
}

func promptOverridesLackID(current Settings) bool {
	for _, entries := range [][]PromptOverride{current.ClaudePromptOverrides, current.CodexPromptOverrides} {
		for _, entry := range entries {
			if entry.ID == "" {
				return true
			}
		}
	}
	return false
}

// fillPromptFiles reads each entry's text from its file. Must be called
// with s.mu held, on a sanitized value, with the store attached.
//
// A missing file keeps whatever inline text the row still carries (the
// pre-migration case) and is otherwise an empty prompt with a log line. A
// file over MaxPromptOverrideLen, or one that would carry its provider's
// list past MaxPromptOverridesTotalLen, leaves the prompt empty and names
// the reason in Problem: the entry stays in the list, so a later save
// cannot drop it, and empty entries never match.
func (s *Service) fillPromptFiles(current *Settings) promptLoad {
	var load promptLoad
	for _, list := range []struct {
		key     string
		entries *[]PromptOverride
	}{
		{"claudePromptOverrides", &current.ClaudePromptOverrides},
		{"codexPromptOverrides", &current.CodexPromptOverrides},
	} {
		entries := slices.Clone(*list.entries)
		total := 0
		inline := false
		for i := range entries {
			entry := &entries[i]
			entry.Path, entry.Problem = "", ""
			if entry.Prompt != "" {
				inline = true
			}
			if entry.ID == "" {
				total += len(entry.Prompt)
				continue
			}
			entry.Path = s.promptFilePath(entry.ID)
			text, state, problem := readPromptFile(entry.Path)
			load.stamps = append(load.stamps, promptFileStamp{path: entry.Path, state: state})
			switch {
			case problem != "":
				entry.Prompt = ""
				entry.Problem = problem
			case !state.exists:
				if entry.Prompt == "" {
					log.Printf("settings: %s[%d] prompt file %s is missing; the entry has no prompt", list.key, i, entry.Path)
				}
			default:
				entry.Prompt = text
			}
			if total+len(entry.Prompt) > MaxPromptOverridesTotalLen {
				entry.Problem = fmt.Sprintf(
					"%s would bring this provider's prompts to %d bytes, over the %d-byte total limit; the entry is not applied",
					entry.Path, total+len(entry.Prompt), MaxPromptOverridesTotalLen,
				)
				entry.Prompt = ""
			}
			if entry.Problem != "" {
				log.Printf("settings: %s[%d]: %s", list.key, i, entry.Problem)
			}
			total += len(entry.Prompt)
		}
		*list.entries = entries
		if inline {
			load.inlineKeys = append(load.inlineKeys, list.key)
		}
	}
	return load
}

// readPromptFile answers a prompt file's trimmed text, its state, and a
// problem when the text cannot be applied. An absent file answers an empty
// state and no problem. The state is the opened file's, taken before the
// read, so an edit landing during the read changes the stamp and the next
// Get reloads.
func readPromptFile(path string) (string, fileState, string) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fileState{}, ""
	}
	if err != nil {
		return "", fileState{}, fmt.Sprintf("cannot open %s: %v", path, err)
	}
	text, state, problem := readOpenPromptFile(f, path)
	if err := f.Close(); err != nil && problem == "" {
		return "", state, fmt.Sprintf("cannot close %s: %v", path, err)
	}
	return text, state, problem
}

// readOpenPromptFile is readPromptFile over an open file; the caller closes
// it and reports a close failure as the problem.
func readOpenPromptFile(f *os.File, path string) (string, fileState, string) {
	info, err := f.Stat()
	if err != nil {
		return "", fileState{}, fmt.Sprintf("cannot stat %s: %v", path, err)
	}
	state := fileState{exists: true, modTime: info.ModTime(), size: info.Size()}
	// The limit is on the trimmed text, so a file may exceed it by its
	// surrounding whitespace. One beyond twice the limit is over it by any
	// reading and is not read at all.
	if info.Size() > 2*MaxPromptOverrideLen {
		return "", state, oversizePromptProblem(path, info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, 2*MaxPromptOverrideLen+1))
	if err != nil {
		return "", state, fmt.Sprintf("cannot read %s: %v", path, err)
	}
	text := strings.TrimSpace(string(data))
	if len(text) > MaxPromptOverrideLen {
		return "", state, oversizePromptProblem(path, int64(len(text)))
	}
	return text, state, ""
}

func oversizePromptProblem(path string, size int64) string {
	return fmt.Sprintf(
		"%s is %d bytes, over the %d-byte limit for one prompt; the entry is not applied until the file is shortened",
		path, size, MaxPromptOverrideLen,
	)
}

// promptFilesUnchanged reports whether every stamped prompt file still has
// the state it was loaded with. At most MaxPromptOverrides stats per
// provider.
func promptFilesUnchanged(stamps []promptFileStamp) bool {
	for _, stamp := range stamps {
		if !stamp.state.equal(readFileState(stamp.path)) {
			return false
		}
	}
	return true
}

// stampPromptFiles stats the prompt file of every entry with an id.
func (s *Service) stampPromptFiles(current Settings) []promptFileStamp {
	var stamps []promptFileStamp
	for _, entries := range [][]PromptOverride{current.ClaudePromptOverrides, current.CodexPromptOverrides} {
		for _, entry := range entries {
			if entry.ID == "" {
				continue
			}
			path := s.promptFilePath(entry.ID)
			stamps = append(stamps, promptFileStamp{path: path, state: readFileState(path)})
		}
	}
	return stamps
}

// promptEntriesByID snapshots each entry's text and problem by id.
func promptEntriesByID(current Settings) map[string]promptEntryState {
	byID := make(map[string]promptEntryState)
	for _, entries := range [][]PromptOverride{current.ClaudePromptOverrides, current.CodexPromptOverrides} {
		for _, entry := range entries {
			if entry.ID != "" {
				byID[entry.ID] = promptEntryState{prompt: entry.Prompt, problem: entry.Problem}
			}
		}
	}
	return byID
}

// settlePromptOverrides runs in mutate after apply and before the change
// projection. It mints the ids of new entries, recomputes Path, and keeps
// each kept entry's Problem, so a repeat save moves nothing.
//
// It answers the files to write and the files to remove (entries no longer
// in either list), and touches no file itself. A write never carries a
// caller's text (restoreStoredPromptTexts), so the files to write are a new
// entry's empty file and the file of a known entry that has none, written
// with the text the entry already had: empty, or the inline text of a row
// that predates the files (MigratePromptFiles).
func (s *Service) settlePromptOverrides(prior map[string]promptEntryState, next *Settings) ([]promptFileWrite, []string) {
	var writes []promptFileWrite
	kept := make(map[string]bool, len(prior))
	for _, list := range []*[]PromptOverride{&next.ClaudePromptOverrides, &next.CodexPromptOverrides} {
		entries := slices.Clone(*list)
		for i := range entries {
			entry := &entries[i]
			if entry.ID == "" {
				entry.ID = uuid.NewString()
			}
			kept[entry.ID] = true
			entry.Path = s.promptFilePath(entry.ID)
			before, known := prior[entry.ID]
			if known && before.prompt == entry.Prompt && readFileState(entry.Path).exists {
				entry.Problem = before.problem
				continue
			}
			entry.Problem = ""
			writes = append(writes, promptFileWrite{path: entry.Path, text: entry.Prompt})
		}
		*list = entries
	}
	var removes []string
	for id := range prior {
		if !kept[id] {
			removes = append(removes, s.promptFilePath(id))
		}
	}
	slices.Sort(removes)
	return writes, removes
}

// writePromptFiles writes each file atomically (0600, directory 0700).
func (s *Service) writePromptFiles(writes []promptFileWrite) error {
	for _, write := range writes {
		data := []byte(nil)
		if write.text != "" {
			data = []byte(write.text + "\n")
		}
		if err := atomicfile.Write(write.path, data); err != nil {
			return fmt.Errorf("settings: write prompt file: %w", err)
		}
	}
	return nil
}

// removePromptFiles removes the files of entries the write dropped. Runs
// after the rows are persisted, so a failed persist never leaves a row
// naming a removed file. A failure is logged: no entry names the file any
// more, so it is ignored, but it stays visible.
func (s *Service) removePromptFiles(paths []string) {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("settings: remove prompt file of a deleted entry: %v", err)
		}
	}
}

// persistedPromptOverrides answers next as it is stored: Path, Problem and
// Blank dropped always, and Prompt dropped once the store is attached,
// since the text then lives in its file.
func persistedPromptOverrides(next Settings, stripPrompt bool) Settings {
	strip := func(entries []PromptOverride) []PromptOverride {
		if entries == nil {
			return nil
		}
		out := make([]PromptOverride, len(entries))
		for i, entry := range entries {
			entry.Path, entry.Problem, entry.Blank = "", "", false
			if stripPrompt {
				entry.Prompt = ""
			}
			out[i] = entry
		}
		return out
	}
	next.ClaudePromptOverrides = strip(next.ClaudePromptOverrides)
	next.CodexPromptOverrides = strip(next.CodexPromptOverrides)
	return next
}
