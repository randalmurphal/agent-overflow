package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A thread's sessions keep a record of what each was shown as memory, one
// file per provider session and part of the delivery in the thread's
// records folder (Plan.Records). A part's SessionStart run writes its
// sections; each prompt's run of the part compares its sections as they
// read now with the record and sends only the sections and lines that
// changed, nothing when none did, and the whole part when there is no
// record of it for the session (docs/specs/memory.md, "Delivery"). A run
// prints before it writes, so a record that could not be written sends the
// change again rather than losing it.

const (
	// ChangesHeading opens a delivery of changes and ChangesEnding closes
	// it. The heading is not Heading's prefix, so a provider looking for a
	// whole delivery does not count one.
	ChangesHeading = "## Changes to Agent Overflow memory"
	ChangesEnding  = "[End of changes to Agent Overflow memory]"
	// changesNoun is what a delivery of changes over Limit calls itself.
	changesNoun = "delivery of changes"
	// changesNote is the line under ChangesHeading.
	changesNote = "[Memory changed since the copy you last received, by another session, the user or you. A list shows only its new or changed lines and the files removed from it; a section shown whole replaces the earlier one.]"
	// recordsFolder holds the records folders of the threads, under the
	// app's config directory, outside every folder a session may write.
	recordsFolder = "memory-records"
	// maxRecordBytes bounds a record a run reads; a larger one is no
	// record, and the run delivers the whole memory.
	maxRecordBytes = 4 << 20
	// recordAge is how long a session's records stay after the last run
	// that wrote any of them: a session a rollback or a fork left behind.
	recordAge = 30 * 24 * time.Hour
)

// RecordsDir is the records folder of threadID's sessions under the app's
// config directory.
func RecordsDir(configDir, threadID string) (string, error) {
	if threadID == "" || threadID != filepath.Base(threadID) || threadID == "." || threadID == ".." {
		return "", fmt.Errorf("%q is not a thread id a records folder can be named by", threadID)
	}
	return filepath.Join(configDir, recordsFolder, threadID), nil
}

// PrepareRecords creates a thread's records folder, so a failure to write
// records is the app's to report before its sessions start.
func PrepareRecords(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the memory records folder %s: %w", dir, err)
	}
	return nil
}

// ForgetRecords deletes a thread's records, after something removed memory
// from its model that the records say it holds: the next prompt of each of
// its sessions delivers the whole memory.
func ForgetRecords(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete the memory records %s: %w", dir, err)
	}
	return nil
}

// section is one block of a delivery as a record keeps it. A keyed section
// changes line by line: an index's facts, keyed by file name, or an
// assistant's projects, keyed by path. Any other changes whole.
type section struct {
	ID      string  `json:"id"`
	Heading string  `json:"heading,omitempty"`
	Body    string  `json:"body,omitempty"`
	Keyed   bool    `json:"keyed,omitempty"`
	Entries []entry `json:"entries,omitempty"`
	// Notes are a keyed section's notices, which send it whole when they
	// change.
	Notes string `json:"notes,omitempty"`
	// text is the section as a whole delivery shows it.
	text string
}

// entry is one line of a keyed section, as a change shows it.
type entry struct {
	Key  string `json:"key"`
	Line string `json:"line"`
}

// wholeSection is text, a block whose first line is its heading, as a
// section that changes whole.
func wholeSection(id, text string) section {
	heading, body, _ := strings.Cut(text, "\n")
	return section{ID: id, Heading: heading, Body: body, text: text}
}

// escaped is s with text that would read as an ending, or as the stamp under
// a heading, written with parentheses for its brackets, so a provider
// looking for either finds only the delivery's own.
func (s section) escaped() section {
	s.Heading, s.Body, s.Notes, s.text = escape(s.Heading), escape(s.Body), escape(s.Notes), escape(s.text)
	s.Entries = slices.Clone(s.Entries)
	for i := range s.Entries {
		s.Entries[i].Line = escape(s.Entries[i].Line)
	}
	return s
}

func escape(text string) string {
	return escaper.Replace(text)
}

var escaper = strings.NewReplacer(
	Ending, "("+strings.Trim(Ending, "[]")+")",
	ChangesEnding, "("+strings.Trim(ChangesEnding, "[]")+")",
	StampPrefix, "("+strings.TrimPrefix(StampPrefix, "["),
)

// same reports whether s reads as other does.
func (s section) same(other section) bool {
	return s.ID == other.ID && s.Heading == other.Heading && s.Body == other.Body && s.Keyed == other.Keyed &&
		s.Notes == other.Notes && slices.Equal(s.Entries, other.Entries)
}

// changes is what a session shown before must be shown to hold now, ""
// when nothing changed.
func changes(before, now []section) string {
	prior := make(map[string]section, len(before))
	for _, s := range before {
		prior[s.ID] = s
	}
	var blocks []string
	for _, s := range now {
		old, ok := prior[s.ID]
		delete(prior, s.ID)
		if block := s.changesFrom(old, ok); block != "" {
			blocks = append(blocks, block)
		}
	}
	for _, s := range before {
		if _, gone := prior[s.ID]; !gone {
			continue
		}
		blocks = append(blocks, s.Heading+"\n[This is no longer part of your memory.]")
	}
	if len(blocks) == 0 {
		return ""
	}
	body := strings.Join(blocks, "\n\n") + "\n\n" + ChangesEnding
	text := ChangesHeading + "\n" + changesNote + "\n\n" + body
	if len(text) > Limit {
		text = ChangesHeading + "\n" + changesNote + "\n" + overNotice(noticedSize(len(text), len("\n"), changesNoun), changesNoun) + "\n\n" + body
	}
	return text
}

// changesFrom is the block that changes old, which ok says the session was
// shown, into s: s whole when it is new, changes whole or changed its
// heading or notices; its new or changed lines and removed keys when only
// lines changed; "" when nothing did.
func (s section) changesFrom(old section, ok bool) string {
	if ok && s.same(old) {
		return ""
	}
	if !ok || !s.Keyed || !old.Keyed || s.Heading != old.Heading || s.Notes != old.Notes {
		return s.text
	}
	was := make(map[string]string, len(old.Entries))
	for _, e := range old.Entries {
		was[e.Key] = e.Line
	}
	var changed []string
	for _, e := range s.Entries {
		if line, ok := was[e.Key]; !ok || line != e.Line {
			changed = append(changed, e.Line)
		}
		delete(was, e.Key)
	}
	var removed []string
	for _, e := range old.Entries {
		if _, gone := was[e.Key]; gone {
			removed = append(removed, removedKey(e.Key))
		}
	}
	block := s.Heading
	if len(changed) > 0 {
		block += "\nNew or changed:\n" + strings.Join(changed, "\n")
	}
	if len(removed) > 0 {
		block += "\nRemoved: " + strings.Join(removed, ", ")
	}
	return block
}

// removedKey is a key as the Removed line names it: a file name or a
// project path is the user's or an agent's text, so it is escaped as the
// lines are and kept on the line.
func removedKey(key string) string {
	return escape(strings.Join(strings.Fields(key), " "))
}

// record is what one provider session was shown of a part.
type record struct {
	Session  string    `json:"session"`
	Part     Part      `json:"part"`
	Sections []section `json:"sections"`
}

// recordName is the file of the record of session's part in a records
// folder: a hash of the session id, which is the provider's, then the part,
// so a session's records are told apart from another's by name.
func recordName(session string, part Part) string {
	sum := sha256.Sum256([]byte(session))
	return hex.EncodeToString(sum[:16]) + "-" + string(part) + ".json"
}

// recordFile matches the name of a record, its session's hash the first
// group, and of the temporary file a write of one leaves when it fails
// between writing and renaming, the second group.
var recordFile = regexp.MustCompile(`^([0-9a-f]{32})-[a-z]+\.json(\.[0-9A-Za-z]+\.tmp)?$`)

// readRecord is the record of session's part in dir, nil when there is
// none.
func readRecord(dir, session string, part Part) (_ *record, err error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the memory records %s: %w", dir, err)
	}
	defer closeInto(root, &err)
	file, err := root.Open(recordName(session, part))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the memory record of session %s: %w", session, err)
	}
	defer closeInto(file, &err)
	data, err := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the memory record of session %s: %w", session, err)
	}
	if len(data) > maxRecordBytes {
		return nil, fmt.Errorf("the memory record of session %s is over %d bytes", session, maxRecordBytes)
	}
	var read record
	if err := json.Unmarshal(data, &read); err != nil {
		return nil, fmt.Errorf("decode the memory record of session %s: %w", session, err)
	}
	if read.Session != session || read.Part != part {
		return nil, fmt.Errorf("the memory record of session %s names session %q, part %q", session, read.Session, read.Part)
	}
	return &read, nil
}

// writeRecord replaces the record of session's part in dir with sections,
// and deletes the records of the sessions no run wrote for recordAge as of
// now (pruneRecords). A record another part's run writes meanwhile is a
// file of its own.
func writeRecord(dir, session string, part Part, sections []section, now time.Time) (err error) {
	data, err := json.Marshal(record{Session: session, Part: part, Sections: sections})
	if err != nil {
		return fmt.Errorf("encode the memory record of session %s: %w", session, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the memory records %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open the memory records %s: %w", dir, err)
	}
	defer closeInto(root, &err)
	name := recordName(session, part)
	temp := name + "." + rand.Text() + ".tmp"
	if err := root.WriteFile(temp, data, 0o600); err != nil {
		return errors.Join(fmt.Errorf("write the memory record of session %s: %w", session, err), removeIfThere(root, temp))
	}
	if err := root.Rename(temp, name); err != nil {
		return errors.Join(fmt.Errorf("replace the memory record of session %s: %w", session, err), removeIfThere(root, temp))
	}
	return pruneRecords(root, now)
}

// pruneRecords deletes from root the records of each session none of whose
// records a run wrote for recordAge as of now, all of them together: a
// session's records live and die together, so a long-lived session keeps
// the record of a part that has not changed. A temporary file a failed
// write left goes once it is recordAge old. Any other file is not the
// app's, and stays.
func pruneRecords(root *os.Root, now time.Time) (err error) {
	folder, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("list the memory records: %w", err)
	}
	defer closeInto(folder, &err)
	entries, err := folder.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("list the memory records: %w", err)
	}
	var errs []error
	newest := map[string]time.Time{}
	sessions := map[string][]string{}
	for _, e := range entries {
		match := recordFile.FindStringSubmatch(e.Name())
		if match == nil || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if match[2] != "" {
			if now.Sub(info.ModTime()) > recordAge {
				errs = append(errs, removeIfThere(root, e.Name()))
			}
			continue
		}
		sessions[match[1]] = append(sessions[match[1]], e.Name())
		if info.ModTime().After(newest[match[1]]) {
			newest[match[1]] = info.ModTime()
		}
	}
	for session, names := range sessions {
		if now.Sub(newest[session]) <= recordAge {
			continue
		}
		for _, name := range names {
			errs = append(errs, removeIfThere(root, name))
		}
	}
	return errors.Join(errs...)
}

// removeRecord deletes the record of session's part in dir, nil when there
// is none.
func removeRecord(dir, session string, part Part) (err error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open the memory records %s: %w", dir, err)
	}
	defer closeInto(root, &err)
	return removeIfThere(root, recordName(session, part))
}

// removeIfThere removes name from root, nil when it is gone.
func removeIfThere(root *os.Root, name string) error {
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete the memory record file %s: %w", name, err)
	}
	return nil
}
