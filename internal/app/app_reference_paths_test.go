package app

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// localPathPatterns match what a capture carries from the machine that
// recorded it. A match whose name group is allow is a placeholder or a
// token the references use for something else.
var localPathPatterns = []struct {
	re *regexp.Regexp
	// name is the submatch compared with allow, or 0 when any match leaks.
	name  int
	allow string
}{
	// A home directory, before a separator or at the end of the path.
	{regexp.MustCompile(`/(home|Users)/([^/\s"'\\]+)(?:[/\\"'\s]|$)`), 2, "user"},
	{regexp.MustCompile(`(?i)\b[a-z]:(?:\\+|/)users(?:\\+|/)([^\\/\s"']+)`), 1, "user"},
	// Claude encodes a cwd into a directory name with dashes.
	{regexp.MustCompile(`-(home|Users)-([A-Za-z0-9_.]+)-`), 2, "user"},
	// Claude's per-user temp root, /tmp/claude-<uid>, also under
	// /private/tmp, dash-encoded or in a Windows Temp directory. A version
	// anchor or model name (claude-4-7) is not preceded by these.
	{regexp.MustCompile(`(?i)(?:\bte?mp[/\\-]+|[/\\])claude-[0-9]+\b`), 0, ""},
	// An asset-tag host name. An 8-digit run is a date stamp
	// (bg-20260423), and claude-<n> is a version anchor.
	{regexp.MustCompile(`(?i)\b([a-z]{2,6})-[0-9]{5,7}\b`), 1, "claude"},
}

// localPathLeaks returns the start and end of each leak in text.
func localPathLeaks(text string) [][2]int {
	var leaks [][2]int
	for _, p := range localPathPatterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			if p.name == 0 || !strings.EqualFold(text[m[2*p.name]:m[2*p.name+1]], p.allow) {
				leaks = append(leaks, [2]int{m[0], m[1]})
			}
		}
	}
	return leaks
}

// streamedBlock is one content block's deltas joined, with the end offset
// and line of each delta.
type streamedBlock struct {
	text  strings.Builder
	ends  []int
	lines []int
}

// streamDelta is the part of a Claude stream_event line a block is
// joined from.
type streamDelta struct {
	Type   string `json:"type"`
	Parent string `json:"parent_tool_use_id"`
	Event  struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
		} `json:"delta"`
	} `json:"event"`
}

// scanLocalPaths reports every line under root that carries a local path
// or host name, as file:line: match. A streamed delta is scanned in its
// content block's joined text, so a leak split across deltas is found and
// a fragment ending mid-name is not; a leak is reported at the line of
// the delta it starts in.
func scanLocalPaths(root string) (found []string, files int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		blocks := map[string]*streamedBlock{}
		flush := func(key string) {
			b := blocks[key]
			delete(blocks, key)
			if b == nil {
				return
			}
			text := b.text.String()
			for _, leak := range localPathLeaks(text) {
				i := sort.SearchInts(b.ends, leak[0]+1)
				found = append(found, fmt.Sprintf("%s:%d: %s", path, b.lines[i], text[leak[0]:leak[1]]))
			}
		}
		for i, line := range strings.Split(string(src), "\n") {
			var ev streamDelta
			if strings.Contains(line, `"stream_event"`) && json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "stream_event" {
				key := fmt.Sprintf("%s/%d", ev.Parent, ev.Event.Index)
				switch ev.Event.Type {
				case "content_block_delta":
					b := blocks[key]
					if b == nil {
						b = &streamedBlock{}
						blocks[key] = b
					}
					delta := ev.Event.Delta
					b.text.WriteString(delta.Text + delta.PartialJSON + delta.Thinking)
					b.ends = append(b.ends, b.text.Len())
					b.lines = append(b.lines, i+1)
					continue
				case "content_block_stop":
					flush(key)
				case "message_start":
					// An abandoned stream's blocks never stop.
					for open := range blocks {
						if strings.HasPrefix(open, ev.Parent+"/") {
							flush(open)
						}
					}
				}
			}
			for _, leak := range localPathLeaks(line) {
				found = append(found, fmt.Sprintf("%s:%d: %s", path, i+1, line[leak[0]:leak[1]]))
			}
		}
		keys := make([]string, 0, len(blocks))
		for key := range blocks {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			flush(key)
		}
		return nil
	})
	return found, files, err
}

// The committed provider references and fixtures carry placeholders, not
// the paths or host name of the machine that captured them.
func TestReferencesCarryNoLocalPaths(t *testing.T) {
	t.Parallel()
	found, files, err := scanLocalPaths(filepath.Join("docs", "references"))
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no reference files scanned")
	}
	if len(found) > 0 {
		t.Fatalf("replace with /home/user, /tmp/ao-fixture or a neutral host:\n%s", strings.Join(found, "\n"))
	}
}

func TestScanLocalPathsFindsEachLeak(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// stream builds one Claude stream_event line.
	stream := func(event string) string { return `{"type":"stream_event","event":` + event + `}` }
	text := func(index int, text string) string {
		return stream(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text))
	}
	input := func(index int, partial string) string {
		return stream(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, index, partial))
	}
	stop := func(index int) string { return stream(fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)) }
	write("clean.ndjson", strings.Join([]string{
		`{"cwd":"/home/user/repo","dir":"-home-user-repo","mac":"/Users/user/repo","tmp":"/tmp/ao-fixture/-work-repo",` +
			`"doc":"whats-new-claude-4-7","anchor":"#flavors-claude-21202--21205","id":"bg-20260423","end":"/home/user"}`,
		`{"win":"C:\\Users\\user\\AppData\\Local\\Temp\\ao-fixture\\x"}`,
		// The placeholder split mid-name is the placeholder.
		text(0, "cd /home/us"),
		text(0, "er/repo"),
		stop(0),
	}, "\n")+"\n")
	write("leaks.ndjson", strings.Join([]string{
		`{"cwd":"/home/someone/repo"}`,
		`{"dir":"/tmp/ao-fixture/-home-someone-repo"}`,
		`{"mac":"/Users/someone/repo"}`,
		`{"out":"/tmp/claude-1000/-work-repo/tasks/a.output"}`,
		`{"host":"ABC-12345"}`,
		`{"cwd":"/private/tmp/claude-502/-private-tmp-claude-502--Users-user-repo"}`,
		`{"partial_json":"/claude-77/"}`,
		`{"cwd":"C:\\Users\\someone\\AppData\\Local\\Temp\\claude-3\\x"}`,
		`{"host":"xxx-00000.local"}`,
		`{"cwd":"/home/someone"}`,
		`cd /home/someone`,
	}, "\n")+"\n")
	// Split inside the token, with another block's deltas between, and a
	// block the capture never stopped.
	write("split.ndjson", strings.Join([]string{
		text(0, "wrote /tmp/claude"),
		input(1, `{"cwd":"/Use`),
		text(0, "-502/out"),
		stop(0),
		input(1, `rs/someone/repo"}`),
	}, "\n")+"\n")
	found, files, err := scanLocalPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if files != 3 {
		t.Fatalf("scanned %d files, want 3", files)
	}
	want := []string{
		"leaks.ndjson:1: /home/someone/",
		"leaks.ndjson:2: -home-someone-",
		"leaks.ndjson:3: /Users/someone/",
		"leaks.ndjson:4: tmp/claude-1000",
		"leaks.ndjson:5: ABC-12345",
		"leaks.ndjson:6: tmp/claude-502",
		"leaks.ndjson:6: tmp-claude-502",
		"leaks.ndjson:7: /claude-77",
		`leaks.ndjson:8: C:\\Users\\someone`,
		`leaks.ndjson:8: Temp\\claude-3`,
		"leaks.ndjson:9: xxx-00000",
		`leaks.ndjson:10: /home/someone"`,
		"leaks.ndjson:11: /home/someone",
		"split.ndjson:1: tmp/claude-502",
		"split.ndjson:2: /Users/someone/",
	}
	if len(found) != len(want) {
		t.Fatalf("found %q, want %q", found, want)
	}
	for i, w := range want {
		if w = root + string(filepath.Separator) + w; found[i] != w {
			t.Errorf("finding %d = %q, want %q", i, found[i], w)
		}
	}
}
