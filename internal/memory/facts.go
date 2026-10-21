package memory

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/agenthome"
)

const (
	// maxEntries bounds the entries of a scope's folder a delivery reads.
	maxEntries = 1000
	// factReadBytes bounds what a delivery reads of each fact to find its
	// frontmatter.
	factReadBytes = 16 << 10
	// descriptionRunes and untilRunes bound what an index line quotes of a
	// fact's description and end condition; quoteRunes what a warning
	// quotes of an agent's word.
	descriptionRunes = 300
	untilRunes       = 200
	quoteRunes       = 80
	// dateLayout is the form of `confirmed` and `expires`.
	dateLayout = "2006-01-02"
)

// factType is a kind of fact, in index order.
type factType struct {
	name  string
	label string
}

// factTypes are the types a fact can have, in the order an index lists them
// (docs/specs/memory.md, "Facts").
var factTypes = []factType{
	{"context", "Context"},
	{"ruling", "Rulings"},
	{"verification", "Verification"},
	{"pitfall", "Pitfalls"},
	{"convention", "Conventions"},
	{"preference", "Preferences"},
	{"reference", "References"},
}

// contextType is the index of the type whose facts an index never cuts.
const contextType = 0

// requiredKeys are the frontmatter keys every fact sets.
var requiredKeys = []string{"description", "type", "source", "confirmed"}

// fact is a fact an index lists.
type fact struct {
	name        string
	kind        int
	description string
	expires     string
	until       string
}

// folderRead is what a delivery read of a scope's folder.
type folderRead struct {
	facts []fact
	// problems are the maintenance warnings about the folder's files.
	problems []string
	// more is set when the folder holds more entries than a delivery reads.
	more bool
}

// readFolder reads the facts directly in root, sorted by type and then by
// name, leaving out the malformed and the expired ones as of today. Hidden
// files, other files and anything that is not a regular file are not facts.
func readFolder(root *os.Root, dir, today string) (_ folderRead, err error) {
	opened, err := root.Open(".")
	if err != nil {
		return folderRead{}, err
	}
	defer closeInto(opened, &err)
	entries, err := opened.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return folderRead{}, err
	}
	var read folderRead
	if len(entries) > maxEntries {
		entries, read.more = entries[:maxEntries], true
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		where := filepath.Join(dir, name)
		if !showableName(name) {
			read.problems = append(read.problems, "The file "+quote(name)+" in "+folderShown(dir)+" has a name with characters an index cannot show. Rename it.")
			continue
		}
		got, problem := readFact(root, name, today)
		switch {
		case errors.Is(problem, fs.ErrNotExist):
			// Removed since the listing.
		case problem != nil:
			read.problems = append(read.problems, where+": "+problem.Error()+".")
		case got.expired:
			read.problems = append(read.problems, fmt.Sprintf("%s expired on %s. Delete it, or move expires if it still holds.", where, got.expires))
		default:
			if got.kind == contextType && got.expires == "" && got.until == "" {
				read.problems = append(read.problems, where+" is a context fact with neither expires nor until. Add one, or delete it.")
			}
			read.facts = append(read.facts, got.fact)
		}
	}
	slices.SortStableFunc(read.facts, func(a, b fact) int { return a.kind - b.kind })
	return read, nil
}

// readFactResult is a fact read from its file, and whether it expired.
type readFactResult struct {
	fact
	expired bool
}

// readFact reads the fact name in root. Its error says why the fact is
// malformed or unreadable, in words a maintenance warning carries.
func readFact(root *os.Root, name, today string) (_ readFactResult, err error) {
	file, err := agenthome.OpenRegular(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return readFactResult{}, err
	}
	if err != nil {
		return readFactResult{}, fmt.Errorf("could not be read: %w", err)
	}
	defer closeInto(file, &err)
	head, bounded, err := readHead(file)
	if err != nil {
		return readFactResult{}, fmt.Errorf("could not be read: %w", err)
	}
	got, err := parseFact(head, bounded)
	if err != nil {
		return readFactResult{}, fmt.Errorf("%w. Fix its frontmatter or delete it", err)
	}
	got.name = name
	return readFactResult{fact: got, expired: got.expires != "" && got.expires < today}, nil
}

// readHead reads a fact's file as cleaned text (agenthome.CleanText) through
// the line that closes its frontmatter, or through its first line when that
// does not open one, reading at most factReadBytes; bounded says the bound
// ended the read before the file did.
func readHead(r io.Reader) (head string, bounded bool, err error) {
	reader := bufio.NewReader(io.LimitReader(r, factReadBytes+1))
	var b strings.Builder
	raw, lines := 0, 0
	for {
		chunk, err := reader.ReadString('\n')
		raw += len(chunk)
		for _, line := range strings.SplitAfter(agenthome.CleanText(chunk), "\n") {
			if line == "" {
				continue
			}
			b.WriteString(line)
			lines++
			fence := strings.TrimSpace(line) == "---"
			if lines == 1 && !fence || lines > 1 && fence {
				return b.String(), false, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return b.String(), raw > factReadBytes, nil
		}
		if err != nil {
			return "", false, err
		}
	}
}

// parseFact reads a fact from the head of its cleaned text; bounded says the
// head ends where the read bound did, not where the file does.
func parseFact(text string, bounded bool) (fact, error) {
	fields, err := frontmatter(text, bounded)
	if err != nil {
		return fact{}, err
	}
	var missing []string
	for _, key := range requiredKeys {
		if fields[key] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fact{}, fmt.Errorf("its frontmatter has no %s", joinWords(missing, "or"))
	}
	kind := slices.IndexFunc(factTypes, func(t factType) bool { return strings.EqualFold(t.name, fields["type"]) })
	if kind < 0 {
		names := make([]string, len(factTypes))
		for i, t := range factTypes {
			names[i] = t.name
		}
		return fact{}, fmt.Errorf("its type %s is not one of %s", quote(fields["type"]), joinWords(names, "or"))
	}
	for _, key := range []string{"confirmed", "expires"} {
		if value := fields[key]; value != "" {
			if _, err := time.Parse(dateLayout, value); err != nil {
				return fact{}, fmt.Errorf("its %s %s is not a date written YYYY-MM-DD", key, quote(value))
			}
		}
	}
	return fact{
		kind:        kind,
		description: oneLine(fields["description"], descriptionRunes),
		expires:     fields["expires"],
		until:       oneLine(fields["until"], untilRunes),
	}, nil
}

// frontmatter reads the `key: value` lines between a first line `---` and
// the next `---` line, keys in lower case. Blank lines and lines starting
// with `#` are skipped; a value in matching quotes loses them.
func frontmatter(text string, bounded bool) (map[string]string, error) {
	lines := strings.Split(text, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return nil, errors.New("it does not open with a --- frontmatter line")
	}
	fields := map[string]string{}
	for i, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "---" {
			return fields, nil
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || !frontmatterKey(key) {
			return nil, fmt.Errorf("line %d of its frontmatter is not key: value", i+2)
		}
		key = strings.ToLower(key)
		if _, twice := fields[key]; twice {
			return nil, fmt.Errorf("its frontmatter sets %s twice", quote(key))
		}
		fields[key] = unquote(strings.TrimSpace(value))
	}
	if bounded {
		return nil, fmt.Errorf("its frontmatter does not close in the first %d KiB", factReadBytes>>10)
	}
	return nil, errors.New("its frontmatter does not close with a --- line")
}

// frontmatterKey reports a key: a letter or `_`, then letters, digits, `_`
// and `-`.
func frontmatterKey(key string) bool {
	for i, r := range key {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r == '-' || r >= '0' && r <= '9'):
		default:
			return false
		}
	}
	return key != ""
}

// unquote drops the quotes around a value in matching single or double
// quotes.
func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

// oneLine is text on one line, its white space folded, at most limit runes.
func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// quote is an agent's word as a warning quotes it: cleaned, on one line,
// bounded, in backticks.
func quote(text string) string {
	return "`" + strings.ReplaceAll(oneLine(agenthome.CleanText(text), quoteRunes), "`", "'") + "`"
}

// showableName reports a file name an index line can carry as it is: valid
// UTF-8 without the characters agenthome.CleanText removes, a tab or a line
// break.
func showableName(name string) bool {
	return agenthome.CleanText(name) == name && !strings.ContainsAny(name, "\t\n")
}

// joinWords joins words as a sentence lists them: "a", "a or b", "a, b or c".
func joinWords(words []string, conjunction string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + conjunction + " " + words[len(words)-1]
}

// openFolder and openFolderIn open a scope's folder as a root without
// waiting on a FIFO in its place (agenthome.OpenFolder).
var (
	openFolder   = agenthome.OpenFolder
	openFolderIn = agenthome.OpenFolderIn
)

// closeInto closes a handle a read is done with and reports a failure through
// the read's own error.
func closeInto(closer io.Closer, err *error) {
	if closeErr := closer.Close(); closeErr != nil {
		*err = errors.Join(*err, closeErr)
	}
}
