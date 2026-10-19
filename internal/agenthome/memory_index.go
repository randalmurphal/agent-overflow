package agenthome

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/stringsx"
)

// The memory index check: the delivered MEMORY.md ends with a notice naming
// the memory files it does not link and the links that name no file, so an
// index that drifted from memory/ is fixed in the session that reads it.
// The check reads memory/ through the home's root like every other read.
const (
	// memoryDir holds the agent's memory files, one fact each.
	memoryDir = "memory"
	// IndexCheckPrefix opens each notice of the check.
	IndexCheckPrefix = "[Index check: "
	// maxMemoryEntries bounds the entries of memory/ the check reads.
	maxMemoryEntries = 1000
	// maxIndexLinks bounds the distinct links into memory/ the check reads
	// from the index and looks up.
	maxIndexLinks = 500
	// maxListedNames and maxListedNameBytes bound what a notice names, so
	// the index stays inside the providers' limit on one hook's output.
	maxListedNames     = 10
	maxListedNameBytes = 80
)

// memoryLinkPattern finds the target of a Markdown link: `](target)` or
// `](<target>)`.
var memoryLinkPattern = regexp.MustCompile(`\]\(\s*(?:<([^>\n]*)>|([^)\s]+))`)

// checkMemoryIndex compares the links of index, MEMORY.md's cleaned text,
// with the Markdown files in memory/ and returns the notices for what
// differs, "" when nothing does. A memory/ that cannot be read is a
// notice and the error.
//
// A file and a link match by cleaned name, or when the link resolves to the
// file under another spelling (a case-insensitive file system, a symbolic
// link). A link the root cannot resolve for another reason than absence,
// such as one out of the home, is neither reported nor matched.
func checkMemoryIndex(root *os.Root, index string) (string, error) {
	files, moreFiles, err := memoryFiles(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("%s%s/ could not be read: %v.]", IndexCheckPrefix, memoryDir, err), err
	}
	links, moreLinks := indexedMemoryFiles(index)
	listed := make(map[string]bool, len(files))
	for _, file := range files {
		listed[file.name] = true
	}
	linked := make(map[string]bool, len(links))
	var gone []string
	// The files links resolve to under a name the listing does not hold.
	var resolved []fs.FileInfo
	for _, link := range links {
		if listed[link] {
			linked[link] = true
			continue
		}
		info, err := root.Stat(link)
		switch {
		case err == nil:
			resolved = append(resolved, info)
		case errors.Is(err, fs.ErrNotExist):
			gone = append(gone, link)
		}
	}
	var unlinked []string
	for _, file := range files {
		if !linked[file.name] && !resolvesTo(file.entry, resolved) {
			unlinked = append(unlinked, file.name)
		}
	}
	var notices []string
	if len(unlinked) > 0 {
		notices = append(notices, fmt.Sprintf("%s%s/ holds %s %s does not link: %s.]",
			IndexCheckPrefix, memoryDir, plural(len(unlinked), "file", "files"), MemoryIndexFile, listNames(unlinked)))
	}
	if moreFiles {
		notices = append(notices, fmt.Sprintf("%sonly the first %d entries of %s/ were read.]", IndexCheckPrefix, maxMemoryEntries, memoryDir))
	}
	if len(gone) > 0 {
		verb := "does"
		if len(gone) > 1 {
			verb = "do"
		}
		notices = append(notices, fmt.Sprintf("%s%s links %s that %s not exist: %s.]",
			IndexCheckPrefix, MemoryIndexFile, plural(len(gone), "file", "files"), verb, listNames(gone)))
	}
	if moreLinks {
		notices = append(notices, fmt.Sprintf("%sonly the first %d links into %s/ were checked.]", IndexCheckPrefix, maxIndexLinks, memoryDir))
	}
	return strings.Join(notices, "\n"), nil
}

// resolvesTo reports whether entry is one of the files in resolved.
func resolvesTo(entry fs.DirEntry, resolved []fs.FileInfo) bool {
	if len(resolved) == 0 {
		return false
	}
	info, err := entry.Info()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(resolved, func(other fs.FileInfo) bool { return os.SameFile(info, other) })
}

// indexedMemoryFiles are the paths under memory/ that index links, sorted
// and without repeats, at most maxIndexLinks of them; more is set when the
// index links more. Each target has its fragment or query dropped, its
// percent escapes decoded and its path cleaned.
func indexedMemoryFiles(index string) (links []string, more bool) {
	seen := map[string]bool{}
	for rest := index; ; {
		at := memoryLinkPattern.FindStringSubmatchIndex(rest)
		if at == nil {
			break
		}
		target := submatch(rest, at, 1) + submatch(rest, at, 2)
		rest = rest[at[1]:]
		if cut := strings.IndexAny(target, "#?"); cut >= 0 {
			target = target[:cut]
		}
		if decoded, err := url.PathUnescape(target); err == nil {
			target = decoded
		}
		target = path.Clean(target)
		if !strings.HasPrefix(target, memoryDir+"/") || seen[target] {
			continue
		}
		if len(links) == maxIndexLinks {
			more = true
			break
		}
		seen[target] = true
		links = append(links, target)
	}
	slices.Sort(links)
	return links, more
}

// submatch is the text of group i of the match at in text, "" when the
// group did not take part.
func submatch(text string, at []int, i int) string {
	if at[2*i] < 0 {
		return ""
	}
	return text[at[2*i]:at[2*i+1]]
}

// memoryFile is a Markdown file directly in memory/: its path from the home,
// with its name cleaned as the index's text is, and its directory entry.
type memoryFile struct {
	name  string
	entry fs.DirEntry
}

// memoryFiles are the Markdown files directly in memory/, sorted by name.
// more is set when memory/ holds more entries than the check reads.
func memoryFiles(root *os.Root) (_ []memoryFile, more bool, err error) {
	// Not blocking on a FIFO in the directory's place, as openRegular.
	dir, err := root.OpenFile(memoryDir, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, false, err
	}
	defer closeInto(dir, &err)
	entries, err := dir.ReadDir(maxMemoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > maxMemoryEntries {
		entries, more = entries[:maxMemoryEntries], true
	}
	var files []memoryFile
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, memoryFile{name: memoryDir + "/" + CleanText(entry.Name()), entry: entry})
		}
	}
	slices.SortFunc(files, func(a, b memoryFile) int { return strings.Compare(a.name, b.name) })
	return files, more, nil
}

// nameEscaper percent-escapes the characters that would let a quoted name
// close its notice or read as Markdown: a name is the agent's text, and the
// notice is the app's. The check decodes the escapes, so a quoted name
// still works as a link.
var nameEscaper = strings.NewReplacer("%", "%25", "[", "%5B", "]", "%5D", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E", "`", "%60")

// listNames names at most maxListedNames of names, each as boundName quotes
// it, and counts the rest.
func listNames(names []string) string {
	shown := make([]string, 0, min(len(names), maxListedNames))
	for _, name := range names[:min(len(names), maxListedNames)] {
		shown = append(shown, boundName(name))
	}
	if rest := len(names) - len(shown); rest > 0 {
		return strings.Join(shown, ", ") + " and " + strconv.Itoa(rest) + " more"
	}
	if len(shown) == 1 {
		return shown[0]
	}
	return strings.Join(shown[:len(shown)-1], ", ") + " and " + shown[len(shown)-1]
}

// boundName is a file name as a notice quotes it: cleaned, on one line,
// escaped (nameEscaper), and at most maxListedNameBytes bytes.
func boundName(name string) string {
	name = nameEscaper.Replace(strings.ReplaceAll(CleanText(name), "\n", " "))
	if len(name) <= maxListedNameBytes {
		return name
	}
	return stringsx.ClipRunes(name, maxListedNameBytes) + "…"
}
