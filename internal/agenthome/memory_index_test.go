package agenthome

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// indexCheck is what the delivered memory index carries after the index:
// its check's notices, "" for none.
func indexCheck(t *testing.T, home string) (string, error) {
	t.Helper()
	text, problem := readContext(t, home, MemoryIndexFile)
	var notices []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, IndexCheckPrefix) {
			notices = append(notices, line)
		}
	}
	return strings.Join(notices, "\n"), problem
}

func TestTheIndexCheckIsQuietForAMatchingIndex(t *testing.T) {
	t.Parallel()
	for name, files := range map[string]map[string]string{
		"links and files": {
			MemoryIndexFile:    "- [A](memory/a.md): a\n- [B](<memory/b c.md>): b\n- [C](./memory/c%20d.md#top): c\n- [A again](memory/a.md?x)\n",
			"memory/a.md":      "a",
			"memory/b c.md":    "b",
			"memory/c d.md":    "c",
			"memory/notes.txt": "not a memory file",
			"memory/sub/x.md":  "below memory/, not in it",
		},
		"no memory dir": {MemoryIndexFile: "- [Site](https://example.com) and [readme](README.md)\n"},
		"no index":      {InstructionsFile: "role"},
	} {
		home := writeHome(t, files)
		if notices, problem := indexCheck(t, home); notices != "" || problem != nil {
			t.Errorf("%s: %q, %v", name, notices, problem)
		}
	}
}

func TestTheIndexCheckNamesUnlinkedFilesAndDanglingLinks(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{
		MemoryIndexFile: "- [A](memory/a.md)\n- [Gone](memory/gone.md)\n- [Deep](memory/sub/gone.md)\n- [Out](../memory/x.md)\n",
		"memory/a.md":   "a",
		"memory/b.md":   "b",
		"memory/c.md":   "c",
	})
	notices, problem := indexCheck(t, home)
	want := "[Index check: memory/ holds 2 files MEMORY.md does not link: memory/b.md and memory/c.md.]\n" +
		"[Index check: MEMORY.md links 2 files that do not exist: memory/gone.md and memory/sub/gone.md.]"
	if problem != nil || notices != want {
		t.Fatalf("notices = %q, %v\nwant %q", notices, problem, want)
	}

	home = writeHome(t, map[string]string{MemoryIndexFile: "- [Gone](memory/gone.md)\n", "memory/b.md": "b"})
	notices, _ = indexCheck(t, home)
	want = "[Index check: memory/ holds 1 file MEMORY.md does not link: memory/b.md.]\n" +
		"[Index check: MEMORY.md links 1 file that does not exist: memory/gone.md.]"
	if notices != want {
		t.Fatalf("notices = %q\nwant %q", notices, want)
	}
}

// A missing index still reports the files it would have to link.
func TestTheIndexCheckOfAMissingIndex(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{"memory/a.md": "a"})
	text, problem := readContext(t, home, MemoryIndexFile)
	if want := memoryIndexHeading + "\n\n[MEMORY.md does not exist yet.]\n[Index check: memory/ holds 1 file MEMORY.md does not link: memory/a.md.]"; problem != nil || text != want {
		t.Fatalf("text = %q, %v\nwant %q", text, problem, want)
	}
}

// The check reads the whole index, not the head a session receives: a link
// past the cap still counts.
func TestTheIndexCheckReadsPastTheCap(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{
		MemoryIndexFile:  strings.Repeat("- filler line\n", MemoryIndexCap/10) + "- [Late](memory/late.md)\n",
		"memory/late.md": "late",
	})
	text, _ := readContext(t, home, MemoryIndexFile)
	if strings.Contains(text, IndexCheckPrefix) || !strings.Contains(text, "[MEMORY.md stops here, ") || strings.Contains(text, "memory/late.md") {
		t.Fatalf("text = %q", text[len(text)-400:])
	}
}

// A notice names at most maxListedNames names, each cleaned and cut to
// maxListedNameBytes on a rune boundary, and counts the rest.
func TestTheIndexCheckBoundsTheNamesItLists(t *testing.T) {
	t.Parallel()
	files := map[string]string{MemoryIndexFile: "- index\n"}
	for i := range maxListedNames + 3 {
		files["memory/"+strconv.Itoa(10+i)+".md"] = "x"
	}
	notices, _ := indexCheck(t, writeHome(t, files))
	if want := "[Index check: memory/ holds 13 files MEMORY.md does not link: memory/10.md, memory/11.md, memory/12.md, memory/13.md, memory/14.md, memory/15.md, memory/16.md, memory/17.md, memory/18.md, memory/19.md and 3 more.]"; notices != want {
		t.Fatalf("notices = %q\nwant %q", notices, want)
	}

	long := "memory/" + strings.Repeat("é", maxListedNameBytes) + "​.md"
	notices, _ = indexCheck(t, writeHome(t, map[string]string{MemoryIndexFile: "- [L](<" + long + ">)\n"}))
	quoted := strings.TrimSuffix(strings.TrimPrefix(notices, "[Index check: MEMORY.md links 1 file that does not exist: "), ".]")
	if len(quoted) > maxListedNameBytes+len("…") || !strings.HasSuffix(quoted, "…") || strings.ContainsAny(quoted, "​�") || !strings.HasPrefix(quoted, "memory/é") {
		t.Fatalf("quoted name = %q (%d bytes) in %q", quoted, len(quoted), notices)
	}
}

// Past maxMemoryEntries entries the check says it read only some.
func TestTheIndexCheckReadsABoundedDirectory(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{MemoryIndexFile: "- index\n"})
	dir := filepath.Join(home, memoryDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range maxMemoryEntries + 1 {
		if err := os.Mkdir(filepath.Join(dir, strconv.Itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	notices, problem := indexCheck(t, home)
	if want := "[Index check: only the first 1000 entries of memory/ were read.]"; problem != nil || notices != want {
		t.Fatalf("notices = %q, %v", notices, problem)
	}
}

// A memory/ that cannot be read is a notice and a problem.
func TestTheIndexCheckReportsAnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{MemoryIndexFile: "- [A](memory/a.md)\n", memoryDir: "a file"})
	notices, problem := indexCheck(t, home)
	if problem == nil || !strings.Contains(problem.Error(), "check "+filepath.Join(home, MemoryIndexFile)+" against "+filepath.Join(home, memoryDir)) {
		t.Fatalf("problem = %v", problem)
	}
	if !strings.HasPrefix(notices, "[Index check: memory/ could not be read: ") || strings.Count(notices, IndexCheckPrefix) != 1 {
		t.Fatalf("notices = %q", notices)
	}
	if problems := Problems(home); len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
}

// The check never follows a link out of the home: a memory/ that is a link
// to another directory is not read.
func TestTheIndexCheckDoesNotFollowALinkOutOfTheHome(t *testing.T) {
	t.Parallel()
	outside := writeHome(t, map[string]string{"secret.md": "secret"})
	home := writeHome(t, map[string]string{MemoryIndexFile: "- [S](memory/secret.md)\n"})
	if err := os.Symlink(outside, filepath.Join(home, memoryDir)); err != nil {
		t.Skip("no symlinks here:", err)
	}
	notices, problem := indexCheck(t, home)
	if problem == nil || strings.Contains(notices, "secret.md does") || !strings.HasPrefix(notices, "[Index check: memory/ could not be read: ") {
		t.Fatalf("notices = %q, %v", notices, problem)
	}
}

// Past maxIndexLinks distinct links into memory/ the check says it checked
// only some, and looks up no more.
func TestTheIndexCheckChecksBoundedLinks(t *testing.T) {
	t.Parallel()
	var index strings.Builder
	for i := range maxIndexLinks + 1 {
		// Each link twice: a repeat is not another link.
		link := "- [G](memory/" + strconv.Itoa(1000+i) + ".md)\n"
		index.WriteString(link + link)
	}
	notices, problem := indexCheck(t, writeHome(t, map[string]string{MemoryIndexFile: index.String()}))
	want := "[Index check: MEMORY.md links 500 files that do not exist: memory/1000.md, memory/1001.md, memory/1002.md, memory/1003.md, memory/1004.md, memory/1005.md, memory/1006.md, memory/1007.md, memory/1008.md, memory/1009.md and 490 more.]\n" +
		"[Index check: only the first 500 links into memory/ were checked.]"
	if problem != nil || notices != want {
		t.Fatalf("notices = %q, %v\nwant %q", notices, problem, want)
	}
}

// A quoted name has the characters that would end its notice or read as
// Markdown percent-escaped, and the escaped name still works as a link.
func TestTheIndexCheckEscapesTheNamesItQuotes(t *testing.T) {
	t.Parallel()
	name := "memory/x [1] (a) <b> `c` 100%.md"
	notices, _ := indexCheck(t, writeHome(t, map[string]string{MemoryIndexFile: "- index\n", name: "x"}))
	escaped := "memory/x %5B1%5D %28a%29 %3Cb%3E %60c%60 100%25.md"
	if want := "[Index check: memory/ holds 1 file MEMORY.md does not link: " + escaped + ".]"; notices != want {
		t.Fatalf("notices = %q\nwant %q", notices, want)
	}
	home := writeHome(t, map[string]string{MemoryIndexFile: "- [X](<" + escaped + ">)\n", name: "x"})
	if notices, problem := indexCheck(t, home); notices != "" || problem != nil {
		t.Fatalf("the escaped name as a link: %q, %v", notices, problem)
	}
}

// A file and a link match by the file's cleaned name, which is how the
// index's text spells it.
func TestTheIndexCheckMatchesCleanedNames(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{MemoryIndexFile: "- [A](memory/a.md)\n", "memory/a​.md": "a"})
	if notices, problem := indexCheck(t, home); notices != "" || problem != nil {
		t.Fatalf("notices = %q, %v", notices, problem)
	}
}

// A link that resolves to a listed file under another name, here a symbolic
// link in memory/, links that file; a link out of the home is neither
// reported nor a match.
func TestTheIndexCheckFollowsALinkToAListedFile(t *testing.T) {
	t.Parallel()
	outside := writeHome(t, map[string]string{"secret.md": "secret"})
	home := writeHome(t, map[string]string{MemoryIndexFile: "- [A](memory/alias.md)\n- [S](memory/out.md)\n", "memory/real.md": "a"})
	if err := os.Symlink("real.md", filepath.Join(home, memoryDir, "alias.md")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(home, memoryDir, "out.md")); err != nil {
		t.Fatal(err)
	}
	if notices, problem := indexCheck(t, home); notices != "" || problem != nil {
		t.Fatalf("notices = %q, %v", notices, problem)
	}
}

// On a file system that folds case, a link spelled in another case links
// the file it opens.
func TestTheIndexCheckMatchesAFoldedName(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{MemoryIndexFile: "- [A](memory/A.md)\n", "memory/a.md": "a"})
	if _, err := os.Stat(filepath.Join(home, memoryDir, "A.md")); err != nil {
		t.Skip("this file system does not fold case")
	}
	if notices, problem := indexCheck(t, home); notices != "" || problem != nil {
		t.Fatalf("notices = %q, %v", notices, problem)
	}
}
