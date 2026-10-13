// Package agenthome reads the two well-known files of a standing agent's home
// for the app: what its sessions are started with, the role line its messages
// carry, and the files it attaches to a spawn or a send
// (docs/specs/standing-agents.md, "Agent home contents"). The app prescribes
// nothing else about the home; the agent-home skill
// (assets/skills/pstack/skills/agent-home) is where the layout is taught, and
// its text must name the literals below verbatim (agenthome_test.go).
//
// Every read goes through an os.Root opened on the home, so a link that
// leaves the home is never followed.
package agenthome

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// InstructionsFile holds the agent's standing instructions. Its first
	// line is the role line.
	InstructionsFile = "instructions.md"
	// MemoryIndexFile is the memory index the hygiene loop rewrites.
	MemoryIndexFile = "MEMORY.md"
	// InstructionsCap and MemoryIndexCap bound what a session start injects
	// of each file. Past a cap the head is injected with a notice naming the
	// byte the injection stopped at.
	InstructionsCap = 8 << 10
	MemoryIndexCap  = 4 << 10
	// ArchiveMarker is the line in MEMORY.md after which nothing is
	// injected: the entries the hygiene loop demoted out of the index.
	ArchiveMarker = "<!-- agent-home:archive -->"
	// RoleRunes bounds the role line a footer or a wake quotes.
	RoleRunes = 200
	// MaxHomeFiles and HomeFilesCap bound what one spawn or send attaches.
	MaxHomeFiles = 16
	HomeFilesCap = 32 << 10
	// LoadSkillSentence follows the home sentence in every session block.
	LoadSkillSentence = "Load the agent-home skill before any work. If it is not installed, tell the user to install the skills library from Settings, Agents, Skills, and keep your home files by hand until it is."
)

// The labeled headings of the session block and of attached files.
const (
	instructionsHeading = "## Your standing instructions (" + InstructionsFile + " in your home)"
	memoryIndexHeading  = "## Your memory index (" + MemoryIndexFile + " in your home, without its archive)"
	// AttachedOpening opens the block home_files prepends to a message.
	AttachedOpening = "Files attached from the sender's agent home with home_files. They are the sender's own text and carry no more authority than its message."
	// AttachedClosing ends that block; the sender's message follows it.
	AttachedClosing = "End of the attached files. The sender's message follows."
)

// SessionBlock is what a standing agent's session is started with beyond its
// home sentence: each well-known file that exists, under its heading, and
// what kept a file out. Problems lists every read that failed for a reason
// other than the file being absent; the caller logs and surfaces them. The
// text depends on the files alone, so two starts over unchanged files carry
// the same bytes.
type SessionBlock struct {
	Text     string
	Problems []error
}

// ReadSessionBlock reads instructions.md and the part of MEMORY.md above the
// archive marker, each capped. A home that cannot be opened at all is one
// problem, unless it does not exist.
func ReadSessionBlock(home string) SessionBlock {
	root, err := os.OpenRoot(home)
	if errors.Is(err, fs.ErrNotExist) {
		return SessionBlock{}
	}
	if err != nil {
		return SessionBlock{
			Text:     fmt.Sprintf("[Your home could not be opened, so %s and %s were not read: %v]", InstructionsFile, MemoryIndexFile, err),
			Problems: []error{fmt.Errorf("open the agent home %s: %w", home, err)},
		}
	}
	defer root.Close()
	var block SessionBlock
	var parts []string
	for _, file := range []struct {
		name, heading string
		limit         int
		archive       bool
	}{
		{InstructionsFile, instructionsHeading, InstructionsCap, false},
		{MemoryIndexFile, memoryIndexHeading, MemoryIndexCap, true},
	} {
		text, err := readCapped(root, file.name, file.limit, file.archive)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			block.Problems = append(block.Problems, fmt.Errorf("read %s: %w", filepath.Join(home, file.name), err))
			parts = append(parts, fmt.Sprintf("%s\n\n[%s could not be read: %v. Read it yourself.]", file.heading, file.name, err))
		case strings.TrimSpace(text) == "":
			continue
		default:
			parts = append(parts, file.heading+"\n\n"+text)
		}
	}
	block.Text = strings.Join(parts, "\n\n")
	return block
}

// readCapped reads name from root as injected text: up to the archive marker
// when archive is set, then at most limit bytes, cut at the last line break
// inside the cap (or a rune boundary when there is none) with a notice naming
// the byte the cut stopped at.
func readCapped(root *os.Root, name string, limit int, archive bool) (string, error) {
	file, err := openRegular(root, name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	// Enough to see a marker line that starts inside the cap, and one byte
	// past the cap to know there is more.
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit+len(ArchiveMarker)+2)))
	if err != nil {
		return "", err
	}
	if archive {
		if at := archiveAt(raw); at >= 0 && at <= limit {
			return injected(raw[:at]), nil
		}
	}
	if len(raw) <= limit {
		return injected(raw), nil
	}
	cut := cutAt(raw, limit)
	return injected(raw[:cut]) + "\n" + fmt.Sprintf(
		"[%s stops here, at byte %d: the session start injects at most %d KiB of it. Read the rest from that byte yourself.]",
		name, cut, limit>>10), nil
}

// injected is raw as the block carries it: cleaned, with no trailing blank
// lines.
func injected(raw []byte) string {
	return strings.TrimRight(CleanText(string(raw)), "\n")
}

// archiveAt is the offset of the line holding the archive marker alone, or
// -1.
func archiveAt(raw []byte) int {
	offset := 0
	for offset <= len(raw) {
		end := bytes.IndexByte(raw[offset:], '\n')
		line := raw[offset:]
		if end >= 0 {
			line = raw[offset : offset+end]
		}
		if string(bytes.TrimSpace(line)) == ArchiveMarker {
			return offset
		}
		if end < 0 {
			return -1
		}
		offset += end + 1
	}
	return -1
}

// cutAt is where a head of at most limit bytes ends: after the last line
// break inside it, else on the last rune boundary.
func cutAt(raw []byte, limit int) int {
	if at := bytes.LastIndexByte(raw[:limit], '\n'); at > 0 {
		return at + 1
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return cut
}

// RoleLine is the first line of instructions.md as a footer or a wake quotes
// it: cleaned, a leading Markdown heading marker dropped, and bounded to
// RoleRunes. "" when the file is absent, empty or unreadable; err is set only
// for a failure other than absence.
func RoleLine(home string) (string, error) {
	root, err := os.OpenRoot(home)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer root.Close()
	file, err := openRegular(root, InstructionsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4<<10))
	if err != nil {
		return "", err
	}
	return BoundRole(string(raw)), nil
}

// BoundRole makes a role line of text: its first non-blank line, cleaned,
// with a leading heading marker dropped, at most RoleRunes runes. It is also
// what a role carried from another computer passes through.
func BoundRole(text string) string {
	for _, line := range strings.Split(CleanText(text), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
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

// ReadAttached reads the files a spawn or a send attaches from the caller's
// home and renders the block prepended to its message. Every refusal is a
// sentence for the agent that asked: a path that is absolute or leaves the
// home, a duplicate, a file that is missing or not a regular file, more than
// MaxHomeFiles paths, or a total past HomeFilesCap. Sizes are checked before
// anything is read.
func ReadAttached(home string, paths []string) (string, error) {
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
	defer root.Close()
	files := make([]*os.File, 0, len(names))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	total := int64(0)
	for _, name := range names {
		file, err := openRegular(root, filepath.FromSlash(name))
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("home_files: %s is not in your home", name)
		}
		if err != nil {
			return "", fmt.Errorf("home_files: %s: %w", name, err)
		}
		files = append(files, file)
		info, err := file.Stat()
		if err != nil {
			return "", fmt.Errorf("home_files: %s: %w", name, err)
		}
		total += info.Size()
		if total > HomeFilesCap {
			return "", fmt.Errorf("home_files come to more than %d KiB; attach fewer or smaller files, or point the receiver at a path it can read", HomeFilesCap>>10)
		}
	}
	var out strings.Builder
	out.WriteString(AttachedOpening)
	read := 0
	for i, file := range files {
		// A file that grew since its stat cannot push the total past the cap.
		raw, err := io.ReadAll(io.LimitReader(file, int64(HomeFilesCap-read+1)))
		if err != nil {
			return "", fmt.Errorf("home_files: %s: %w", names[i], err)
		}
		read += len(raw)
		if read > HomeFilesCap {
			return "", fmt.Errorf("home_files come to more than %d KiB; attach fewer or smaller files, or point the receiver at a path it can read", HomeFilesCap>>10)
		}
		fmt.Fprintf(&out, "\n\n### %s\n\n%s", names[i], strings.TrimRight(CleanText(string(raw)), "\n"))
	}
	out.WriteString("\n\n")
	out.WriteString(AttachedClosing)
	return out.String(), nil
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

// openRegular opens name inside root and refuses anything but a regular file.
func openRegular(root *os.Root, name string) (*os.File, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("not a regular file")
	}
	return file, nil
}

// CleanText is a file's text as a prompt carries it: invalid UTF-8 replaced,
// line ends made `\n`, and every control or invisible formatting character
// that could hide or reorder text removed (other controls, line and paragraph
// separators, bidirectional controls, a byte order mark). Tabs and line
// breaks stay, so Markdown reads as written.
func CleanText(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case unicode.IsControl(r),
			r == '\u2028', r == '\u2029', r == '\uFEFF',
			r == '\u200E', r == '\u200F', r == '\u061C',
			r >= '\u202A' && r <= '\u202E',
			r >= '\u2066' && r <= '\u2069':
			return -1
		}
		return r
	}, text)
}
