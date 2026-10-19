// Package agenthome reads the two well-known files of a standing agent's home
// for the app: what the SessionStart hooks deliver to its sessions (RunHook),
// with the memory index checked against memory/ (memory_index.go), the role
// line its messages carry, and the files it attaches to a spawn or a send
// (docs/specs/standing-agents.md, "Agent home contents"). The app prescribes
// nothing else about the home; the agent-home skill
// (assets/skills/pstack/skills/agent-home) is where the layout is taught, and
// its text must name the literals below verbatim (agenthome_test.go).
//
// Every read goes through an os.Root opened on the home, so a link that
// leaves the home is never followed.
package agenthome

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// InstructionsFile holds the agent's standing instructions. Its first
	// line is the role line.
	InstructionsFile = "instructions.md"
	// MemoryIndexFile is the agent's index of its memory files.
	MemoryIndexFile = "MEMORY.md"
	// InstructionsCap and MemoryIndexCap bound what a session receives of
	// each file, counted in bytes of its cleaned text (CleanText) without its
	// trailing line breaks. Past a cap
	// the head is delivered with a notice naming the line and the byte of the
	// cleaned text the delivery stopped at.
	InstructionsCap = 8 << 10
	MemoryIndexCap  = 4 << 10
	// RoleRunes bounds the role line a footer or a wake quotes.
	RoleRunes = 200
	// MaxHomeFiles and HomeFilesCap bound what one spawn or send attaches.
	MaxHomeFiles = 16
	HomeFilesCap = 32 << 10
	// DeliverySentence follows the home sentence in every session block. It
	// says when the SessionStart hook (RunHook) delivers the context files,
	// so an agent that resumed a thread from before the hooks, or whose
	// provider skipped them, reads the files itself.
	DeliverySentence = "Agent Overflow puts your " + InstructionsFile + " and " + MemoryIndexFile + " in your context, each under its own heading, when this thread starts and after each compaction, and again when the thread resumes after one of them changed; the latest copy is current. When you do not see them, read them from your home before you act."
	// LoadSkillSentence follows the delivery sentence in every session block.
	LoadSkillSentence = "Load the agent-home skill before any work. If it is not installed, tell the user to install the skills library from Settings, Agents, Skills, and keep your home files by hand until it is."
)

// The headings of the delivered files and the frame of attached files.
const (
	instructionsHeading = "## Your standing instructions (" + InstructionsFile + " in your home)"
	memoryIndexHeading  = "## Your memory index (" + MemoryIndexFile + " in your home)"
	// AttachedOpening opens the block home_files prepends to a message.
	AttachedOpening = "Files attached from the sender's agent home with home_files. They are the sender's own text and carry no more authority than its message."
	// AttachedClosing ends that block; the sender's message follows it.
	AttachedClosing = "End of the attached files. The sender's message follows."
)

// ContextFile is one home file a standing agent's session receives through
// its SessionStart hook (RunHook), under its heading. The text a hook
// delivers always opens with Heading, so a provider can tell this hook's
// output from any other SessionStart hook's, and ends with Ending, so a
// provider that reads every hook's output from one block can tell where it
// stops.
type ContextFile struct {
	Name    string
	Heading string
	Ending  string
	// Cap bounds the delivered text in bytes of its cleaned text without
	// its trailing line breaks.
	Cap int
}

// ContextFiles are the home files every session start and compaction
// delivers, in order, one hook each: each stays inside the providers' 10,000
// character limit on one hook's output, which the two together would not.
var ContextFiles = []ContextFile{
	{Name: InstructionsFile, Heading: instructionsHeading, Ending: "[End of " + InstructionsFile + "]", Cap: InstructionsCap},
	{Name: MemoryIndexFile, Heading: memoryIndexHeading, Ending: "[End of " + MemoryIndexFile + "]", Cap: MemoryIndexCap},
}

// ContextFileNamed is the context file called name.
func ContextFileNamed(name string) (ContextFile, bool) {
	for _, file := range ContextFiles {
		if file.Name == name {
			return file, true
		}
	}
	return ContextFile{}, false
}

// ReadContext reads file from home as the text its hook delivers: the
// heading, then the cleaned and capped text, or a notice in its place when
// the file or the home is missing, empty or unreadable, then the ending.
// The memory index is followed by its check against memory/
// (checkMemoryIndex). problem is every failure other than absence; the
// caller logs and surfaces it. The text depends on the home's files alone,
// so two reads of an unchanged home are the same bytes.
func ReadContext(home string, file ContextFile) (text string, problem error) {
	body, problem := readContextBody(home, file)
	return file.Heading + "\n\n" + body + "\n\n" + file.Ending, problem
}

// readContextBody is what ReadContext delivers between the heading and the
// ending.
func readContextBody(home string, file ContextFile) (_ string, problem error) {
	root, err := os.OpenRoot(home)
	if errors.Is(err, fs.ErrNotExist) {
		return "[Your home does not exist, so " + file.Name + " was not read.]", nil
	}
	if err != nil {
		return fmt.Sprintf("[Your home could not be opened, so %s was not read: %v]", file.Name, err),
			fmt.Errorf("open the agent home %s: %w", home, err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			problem = errors.Join(problem, fmt.Errorf("close the agent home %s: %w", home, err))
		}
	}()
	// The index is read whole for its check, and capped after.
	keep := file.Cap
	if file.Name == MemoryIndexFile {
		keep = MaxRawRead
	}
	text, end, err := readCleanedFile(root, file.Name, keep)
	var body string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		body = "[" + file.Name + " does not exist yet.]"
	case err != nil:
		return fmt.Sprintf("[%s could not be read: %v. Read it yourself.]", file.Name, err),
			fmt.Errorf("read %s: %w", filepath.Join(home, file.Name), err)
	default:
		body = capText(file.Name, text, end, file.Cap)
		if strings.TrimSpace(body) == "" {
			body = "[" + file.Name + " is empty.]"
		}
	}
	if file.Name != MemoryIndexFile {
		return body, nil
	}
	notice, err := checkMemoryIndex(root, text)
	if err != nil {
		problem = fmt.Errorf("check %s against %s: %w", filepath.Join(home, MemoryIndexFile), filepath.Join(home, memoryDir), err)
	}
	return joinNotice(body, notice), problem
}

// ContextDigest identifies a context a hook delivered, or would deliver:
// the SHA-256 of its text without its stamp, in hex, so a delivery and a
// read of the same files agree.
func ContextDigest(text string) string {
	sum := sha256.Sum256([]byte(withoutStamp(text)))
	return hex.EncodeToString(sum[:])
}

// StampPrefix opens the line RunHook puts under a context's heading, before
// the delivery's time (stampLayout). Claude Code leaves out a resumed
// session's SessionStart context whose text its transcript already holds
// (docs/references/claude-wire.md, SessionStart hooks), so a file changed
// back to a copy the session had before would not reach the model; the time
// makes each delivery its own text, and tells claude-tui which run a context
// in a model request came from (provider.SessionStartHook.Stamp).
const StampPrefix = "[Delivered at "

// stampLayout is the delivery's time in a stamp: RFC 3339 in UTC, to the
// millisecond, so two runs a moment apart differ.
const stampLayout = "2006-01-02T15:04:05.000Z07:00"

// stamp is text, which ReadContext read for file, with the stamp of a
// delivery at at under its heading.
func stamp(text string, file ContextFile, at time.Time) string {
	return file.Heading + "\n" + StampPrefix + at.UTC().Format(stampLayout) +
		"; it replaces any earlier copy of " + file.Name + " in this conversation.]" +
		strings.TrimPrefix(text, file.Heading)
}

// withoutStamp is text without the stamp under its heading. ReadContext
// follows the heading with a blank line, so a second line that opens with
// StampPrefix is always a stamp.
func withoutStamp(text string) string {
	heading, rest, ok := strings.Cut(text, "\n")
	if !ok || !strings.HasPrefix(rest, StampPrefix) {
		return text
	}
	_, rest, ok = strings.Cut(rest, "\n")
	if !ok {
		return text
	}
	return heading + "\n" + rest
}

// Problems reads every context file of home and returns the failures other
// than absence, which the app reports when a session starts.
func Problems(home string) []error {
	var problems []error
	for _, file := range ContextFiles {
		if _, problem := ReadContext(home, file); problem != nil {
			problems = append(problems, problem)
		}
	}
	return problems
}

// readCleanedFile reads name from root as cleaned text (readCleaned) until
// it holds more than keep bytes.
func readCleanedFile(root *os.Root, name string, keep int) (_ string, _ readEnd, err error) {
	file, err := openRegular(root, name)
	if err != nil {
		return "", readWhole, err
	}
	defer closeInto(file, &err)
	return readCleaned(file, keep)
}

// capText is cleaned text read until end as delivered text: held to limit
// bytes without its trailing line breaks, cut after the last line break
// inside the cap (or on a rune boundary when the cap falls inside one line)
// with a notice naming the line and the byte of the cleaned text the cut
// stopped at.
func capText(name, text string, end readEnd, limit int) string {
	if head := strings.TrimRight(text, "\n"); len(head) <= limit {
		if end != readRawBound {
			return head
		}
		// The raw bound stopped the read inside the cap: what was read is
		// all of it the session gets, and the agent is told where to go on.
		return joinNotice(head, fmt.Sprintf(
			"[%s stops here, inside line %d: Agent Overflow reads at most the first %d MiB of a file and the rest of it was not read. Read it yourself from line %d.]",
			name, strings.Count(text, "\n")+1, MaxRawRead>>20, strings.Count(text, "\n")+1))
	}
	cut, whole := cutAt(text, limit)
	line := strings.Count(text[:cut], "\n")
	where := fmt.Sprintf("after line %d", line)
	from := line + 1
	if !whole {
		line++
		where, from = fmt.Sprintf("inside line %d", line), line
	}
	return joinNotice(strings.TrimRight(text[:cut], "\n"), fmt.Sprintf(
		"[%s stops here, %s (byte %d of its text with hidden characters removed): Agent Overflow delivers at most %d KiB of it. Read it yourself from line %d.]",
		name, where, cut, limit>>10, from))
}

// joinNotice puts a notice on the line after what was delivered; an empty
// notice adds nothing.
func joinNotice(head, notice string) string {
	switch {
	case notice == "":
		return head
	case head == "":
		return notice
	}
	return head + "\n" + notice
}

// cutAt is where a head of at most limit bytes of text, which is longer than
// limit, ends: after the last line break inside it or right after it, which
// the head does not carry (whole is true), else on the last rune boundary.
func cutAt(text string, limit int) (cut int, whole bool) {
	if at := strings.LastIndexByte(text[:limit+1], '\n'); at > 0 {
		return at + 1, true
	}
	cut = limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut, false
}

// RoleLine is the role line of instructions.md as a footer or a wake quotes
// it (BoundRole). "" when the file is absent, empty or holds no role; err is
// set only for a failure other than absence.
func RoleLine(home string) (_ string, err error) {
	root, err := os.OpenRoot(home)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer closeInto(root, &err)
	file, err := openRegular(root, InstructionsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer closeInto(file, &err)
	text, _, err := readCleaned(file, 4<<10)
	if err != nil {
		return "", err
	}
	return BoundRole(text), nil
}

// RoleCache keeps the role line of each home it read, keyed by its
// instructions file's modification time and size, so the sends, spawns and
// settlements that name a standing agent stat the file rather than read it.
// A file that changed is read again; one that is gone, or that cannot be
// read, is answered as RoleLine answers it and leaves nothing cached, so no
// stale role is served for it. The zero value is ready to use.
//
// Two writes of the same length inside one tick of the file system's
// clock leave the same key, so an entry read while its file's time was
// within roleRacyWindow of the clock is racily clean, as git calls it: it
// is read again on every call until a read finds the file older than that.
type RoleCache struct {
	mu      sync.Mutex
	entries map[string]cachedRole
	// reads counts the files Role read through RoleLine.
	reads atomic.Int64
	// now is the clock the racy rule reads; nil is time.Now.
	now func() time.Time
}

// roleRacyWindow covers the coarsest modification-time resolution a home
// can sit on (FAT's two seconds).
const roleRacyWindow = 2 * time.Second

type cachedRole struct {
	modTime time.Time
	size    int64
	role    string
	// racy is set when the file's time was within roleRacyWindow of the
	// clock at the stat the entry was read under.
	racy bool
}

// Role is RoleLine(home), read again only when the file changed.
func (c *RoleCache) Role(home string) (string, error) {
	info, err := instructionsInfo(home)
	if errors.Is(err, fs.ErrNotExist) {
		c.forget(home)
		return "", nil
	}
	if err != nil {
		c.forget(home)
		return "", err
	}
	c.mu.Lock()
	cached, ok := c.entries[home]
	c.mu.Unlock()
	if ok && !cached.racy && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return cached.role, nil
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	racy := !info.ModTime().Before(now().Add(-roleRacyWindow))
	c.reads.Add(1)
	role, err := RoleLine(home)
	if err != nil {
		c.forget(home)
		return "", err
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]cachedRole{}
	}
	// Keyed by what was stat'ed before the read: a write after it changes
	// the key, and the next call reads again.
	c.entries[home] = cachedRole{modTime: info.ModTime(), size: info.Size(), role: role, racy: racy}
	c.mu.Unlock()
	return role, nil
}

// Forget drops what the cache holds for home: the home of an assistant
// that was deleted.
func (c *RoleCache) Forget(home string) { c.forget(home) }

func (c *RoleCache) forget(home string) {
	c.mu.Lock()
	delete(c.entries, home)
	c.mu.Unlock()
}

// instructionsInfo stats a home's instructions file through the home's
// root, as RoleLine opens it.
func instructionsInfo(home string) (_ fs.FileInfo, err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer closeInto(root, &err)
	return root.Stat(InstructionsFile)
}

// BoundRole makes a role line of text: its first non-blank line after a
// leading frontmatter block, skipping bare rule lines (`---`, `***`, `___`),
// cleaned, with runs of white space folded and every leading ATX heading
// marker (a run of `#` followed by a space or the end of the line) dropped, at most RoleRunes runes. It is also what a role
// carried from another computer passes through, so it is a fixed point:
// BoundRole(BoundRole(x)) == BoundRole(x), and a role bounded on the computer
// that read it reads the same after this one bounds it again.
func BoundRole(text string) string {
	lines := strings.Split(CleanText(text), "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start < len(lines) && strings.TrimSpace(lines[start]) == "---" {
		for end := start + 1; end < len(lines); end++ {
			if closing := strings.TrimSpace(lines[end]); closing == "---" || closing == "..." {
				start = end + 1
				break
			}
		}
	}
	for _, line := range lines[start:] {
		line = stripHeadingMarkers(strings.Join(strings.Fields(line), " "))
		if line == "" || ruleLine(line) {
			continue
		}
		if utf8.RuneCountInString(line) <= RoleRunes {
			return line
		}
		runes := []rune(line)
		return string(runes[:RoleRunes]) + "\u2026"
	}
	return ""
}

// stripHeadingMarkers drops leading ATX heading markers from a line whose
// white space is already folded to single spaces, until none is left, so a
// second pass finds none. A `#` run followed by anything else is text:
// "#1 reviewer" stays as it is.
func stripHeadingMarkers(line string) string {
	for strings.HasPrefix(line, "#") {
		rest := strings.TrimLeft(line, "#")
		if rest != "" && rest[0] != ' ' {
			return line
		}
		line = strings.TrimPrefix(rest, " ")
	}
	return line
}

// ruleLine reports a Markdown thematic break: three or more of one of `-`,
// `*` or `_`, spaces allowed between them.
func ruleLine(line string) bool {
	compact := strings.ReplaceAll(line, " ", "")
	if len(compact) < 3 {
		return false
	}
	mark := compact[0]
	if mark != '-' && mark != '*' && mark != '_' {
		return false
	}
	return strings.Count(compact, string(mark)) == len(compact)
}

// ReadAttached reads the files a spawn or a send attaches from the caller's
// home and renders the block prepended to its message. Every refusal is a
// sentence for the agent that asked: a path that is absolute or leaves the
// home, a duplicate, a file that is missing or not a regular file, more than
// MaxHomeFiles paths, or more than HomeFilesCap bytes of cleaned text
// together.
func ReadAttached(home string, paths []string) (_ string, err error) {
	if len(paths) == 0 {
		return "", nil
	}
	if len(paths) > MaxHomeFiles {
		return "", fmt.Errorf("home_files names %d files; at most %d can be attached", len(paths), MaxHomeFiles)
	}
	names := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range paths {
		name, err := homeRelative(raw)
		if err != nil {
			return "", err
		}
		if seen[name] {
			return "", fmt.Errorf("home_files names %s twice", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return "", fmt.Errorf("open your home to attach files: %w", err)
	}
	defer closeInto(root, &err)
	var out strings.Builder
	out.WriteString(AttachedOpening)
	left := HomeFilesCap
	for _, name := range names {
		text, err := readAttachedFile(root, name, left)
		if err != nil {
			return "", err
		}
		left -= len(text)
		fmt.Fprintf(&out, "\n\n### %s\n\n%s", name, text)
	}
	out.WriteString("\n\n")
	out.WriteString(AttachedClosing)
	return out.String(), nil
}

// readAttachedFile reads one attached file's cleaned text without its
// trailing line breaks, refusing it when it would take more than left bytes
// of what is left of HomeFilesCap.
func readAttachedFile(root *os.Root, name string, left int) (_ string, err error) {
	file, err := openRegular(root, filepath.FromSlash(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("home_files: %s is not in your home", name)
	}
	if err != nil {
		return "", fmt.Errorf("home_files: %s: %w", name, err)
	}
	defer closeInto(file, &err)
	text, end, err := readCleaned(file, left)
	if err != nil {
		return "", fmt.Errorf("home_files: %s: %w", name, err)
	}
	if end == readRawBound {
		return "", fmt.Errorf("home_files: %s goes on past the first %d MiB a read takes; attach a smaller file", name, MaxRawRead>>20)
	}
	text = strings.TrimRight(text, "\n")
	if end == readPastKeep || len(text) > left {
		return "", fmt.Errorf("home_files come to more than %d KiB; attach fewer or smaller files, or point the receiver at a path it can read", HomeFilesCap>>10)
	}
	return text, nil
}

// homeRelative checks a home_files path lexically and returns it in slash
// form. The root refuses whatever escapes through a link.
func homeRelative(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("home_files has an empty path")
	}
	slashed := filepath.ToSlash(trimmed)
	if filepath.IsAbs(trimmed) || path.IsAbs(slashed) || filepath.VolumeName(trimmed) != "" || strings.HasPrefix(slashed, "/") {
		return "", fmt.Errorf("home_files: %s is absolute; name a path relative to your home", trimmed)
	}
	for _, element := range strings.Split(slashed, "/") {
		if element == ".." {
			return "", fmt.Errorf("home_files: %s leaves your home", trimmed)
		}
	}
	name := path.Clean(slashed)
	if name == "." {
		return "", fmt.Errorf("home_files: %s names your home itself, not a file in it", trimmed)
	}
	return name, nil
}

// closeInto closes a handle the read is done with and reports a failure
// through the read's own error, so a failed close is never dropped.
func closeInto(closer io.Closer, err *error) {
	if closeErr := closer.Close(); closeErr != nil {
		*err = errors.Join(*err, closeErr)
	}
}

// errNotRegular refuses anything in the home but a regular file.
var errNotRegular = errors.New("not a regular file")

// openRegular opens name inside root for reading, refusing anything but a
// regular file. Opening a FIFO waits for a writer, so the open does not wait
// (openNonblock) and the mode is checked on the opened file, which no rename
// can swap afterwards.
func openRegular(root *os.Root, name string) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err == nil && !opened.Mode().IsRegular() {
		err = errNotRegular
	}
	if err != nil {
		closeInto(file, &err)
		return nil, err
	}
	return file, nil
}

// MaxRawRead bounds the bytes one read takes from a file, whatever cleaning
// removes from them, so a file of nothing but invisible characters is not
// read without end. Past it the rest of the file is not read.
const MaxRawRead = 1 << 20

// readEnd is why readCleaned stopped.
type readEnd int

const (
	// readWhole: the input ended.
	readWhole readEnd = iota
	// readPastKeep: the cleaned text grew past keep bytes.
	readPastKeep
	// readRawBound: MaxRawRead raw bytes were read and the input goes on.
	readRawBound
)

// readCleaned reads r as cleaned text, byte for byte what CleanText makes of
// the same input, until it holds more than keep bytes before its trailing
// line breaks, the input ends, or MaxRawRead raw bytes are read. A character
// the raw bound splits is dropped, not read as invalid UTF-8.
func readCleaned(r io.Reader, keep int) (string, readEnd, error) {
	limited := &io.LimitedReader{R: r, N: MaxRawRead}
	reader := bufio.NewReader(limited)
	var out strings.Builder
	afterCR := false
	// kept is how much of out precedes its trailing line breaks.
	kept := 0
	for kept <= keep {
		if limited.N == 0 {
			// The raw bound is all buffered. Peek of what is buffered
			// cannot fail.
			pending, _ := reader.Peek(reader.Buffered())
			if len(pending) > 0 && !utf8.FullRune(pending) {
				goesOn, err := inputGoesOn(r)
				if err != nil {
					return "", readWhole, err
				}
				if goesOn {
					return out.String(), readRawBound, nil
				}
			}
		}
		// Invalid UTF-8 reads as utf8.RuneError, which is U+FFFD.
		value, _, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			if limited.N > 0 {
				return out.String(), readWhole, nil
			}
			goesOn, err := inputGoesOn(r)
			if err != nil {
				return "", readWhole, err
			}
			if goesOn {
				return out.String(), readRawBound, nil
			}
			return out.String(), readWhole, nil
		}
		if err != nil {
			return "", readWhole, err
		}
		if afterCR && value == '\n' {
			afterCR = false
			continue
		}
		afterCR = value == '\r'
		if value == '\r' {
			value = '\n'
		}
		if hiddenRune(value) {
			continue
		}
		out.WriteRune(value)
		if value != '\n' {
			kept = out.Len()
		}
	}
	return out.String(), readPastKeep, nil
}

// inputGoesOn reports whether r holds another byte past the raw bound. The
// byte it reads is not wanted: the read stops there either way.
func inputGoesOn(r io.Reader) (bool, error) {
	var probe [1]byte
	n, err := r.Read(probe[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n > 0, nil
}

// CleanText is a file's text as a prompt carries it: invalid UTF-8 replaced,
// line ends made `\n`, and every control and format character (Unicode Cc and
// Cf, which includes zero-width characters, bidirectional controls, the soft
// hyphen, the byte order mark and the tag block) and the line and paragraph
// separators removed. Tabs and line breaks stay, so Markdown reads as written.
func CleanText(text string) string {
	// strings.Map writes U+FFFD for each invalid byte, as readCleaned does.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\r' {
			return '\n'
		}
		if hiddenRune(r) {
			return -1
		}
		return r
	}, text)
}

// hiddenRune is a character CleanText removes.
func hiddenRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r == '\u2028' || r == '\u2029' || unicode.In(r, unicode.Cc, unicode.Cf)
}
