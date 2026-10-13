package agenthome

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for name, text := range files {
		path := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestSessionBlockCarriesBothFilesUnderTheirHeadings(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{
		InstructionsFile: "Release lead for the app.\n\nGrants: none.\n",
		MemoryIndexFile:  "# Memory\n\n- [Rule](memory/rule.md): hook\n",
	})
	block := ReadSessionBlock(home)
	if len(block.Problems) > 0 {
		t.Fatalf("problems: %v", block.Problems)
	}
	want := instructionsHeading + "\n\nRelease lead for the app.\n\nGrants: none.\n\n" +
		memoryIndexHeading + "\n\n# Memory\n\n- [Rule](memory/rule.md): hook"
	if block.Text != want {
		t.Fatalf("block:\n%s\nwant:\n%s", block.Text, want)
	}
	if again := ReadSessionBlock(home); again.Text != block.Text {
		t.Fatalf("two reads of unchanged files differ:\n%s\n---\n%s", block.Text, again.Text)
	}
}

func TestSessionBlockOfAMissingHomeOrFilesIsEmpty(t *testing.T) {
	t.Parallel()
	if block := ReadSessionBlock(filepath.Join(t.TempDir(), "gone")); block.Text != "" || len(block.Problems) > 0 {
		t.Fatalf("missing home: %+v", block)
	}
	home := writeHome(t, map[string]string{"README.md": "readme"})
	if block := ReadSessionBlock(home); block.Text != "" || len(block.Problems) > 0 {
		t.Fatalf("home with neither file: %+v", block)
	}
	only := writeHome(t, map[string]string{MemoryIndexFile: "index"})
	if block := ReadSessionBlock(only); block.Text != memoryIndexHeading+"\n\nindex" {
		t.Fatalf("index only: %q", block.Text)
	}
}

func TestSessionBlockLeavesOutTheArchive(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{
		MemoryIndexFile: "- [Live](memory/live.md): kept\n\n  " + ArchiveMarker + "  \n- [Old](memory/old.md): demoted\n",
	})
	text := ReadSessionBlock(home).Text
	if !strings.Contains(text, "memory/live.md") || strings.Contains(text, "memory/old.md") || strings.Contains(text, ArchiveMarker) {
		t.Fatalf("block:\n%s", text)
	}
	// The marker only counts as a line of its own.
	inline := writeHome(t, map[string]string{
		MemoryIndexFile: "- see " + ArchiveMarker + " in the skill\n- [Old](memory/old.md): kept\n",
	})
	if text := ReadSessionBlock(inline).Text; !strings.Contains(text, "memory/old.md") {
		t.Fatalf("an inline mention cut the index:\n%s", text)
	}
}

func TestSessionBlockCapsEachFileAndSaysWhereItStopped(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", 99) + "\n"
	instructions := strings.Repeat(line, 100) // 10000 bytes
	index := strings.Repeat(line, 50)         // 5000 bytes
	home := writeHome(t, map[string]string{InstructionsFile: instructions, MemoryIndexFile: index})
	text := ReadSessionBlock(home).Text
	instructionsCut := (InstructionsCap / 100) * 100
	indexCut := (MemoryIndexCap / 100) * 100
	for _, want := range []string{
		"[instructions.md stops here, at byte " + itoa(instructionsCut) + ": the session start injects at most 8 KiB of it.",
		"[MEMORY.md stops here, at byte " + itoa(indexCut) + ": the session start injects at most 4 KiB of it.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("block has no %q:\n%s", want, text[len(text)-600:])
		}
	}
	if got := strings.Count(text, strings.Repeat("x", 99)); got != instructionsCut/100+indexCut/100 {
		t.Fatalf("block carries %d lines, want %d", got, instructionsCut/100+indexCut/100)
	}
	// A file with no line break is cut on a rune boundary.
	wide := writeHome(t, map[string]string{InstructionsFile: strings.Repeat("é", InstructionsCap)})
	head, _, _ := strings.Cut(ReadSessionBlock(wide).Text, "\n[instructions.md stops here, at byte ")
	if strings.ContainsRune(head, '\uFFFD') {
		t.Fatal("the cut split a character")
	}
	// The archive marker past the cap still leaves a capped head.
	late := writeHome(t, map[string]string{MemoryIndexFile: index + ArchiveMarker + "\nold\n"})
	if text := ReadSessionBlock(late).Text; !strings.Contains(text, "MEMORY.md stops here") || strings.Contains(text, "old") {
		t.Fatalf("late archive:\n%s", text)
	}
}

func TestSessionBlockReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{MemoryIndexFile: "index"})
	if err := os.Mkdir(filepath.Join(home, InstructionsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	block := ReadSessionBlock(home)
	if len(block.Problems) != 1 || !strings.Contains(block.Problems[0].Error(), filepath.Join(home, InstructionsFile)) {
		t.Fatalf("problems = %v", block.Problems)
	}
	if !strings.Contains(block.Text, "[instructions.md could not be read: not a regular file. Read it yourself.]") ||
		!strings.Contains(block.Text, "index") {
		t.Fatalf("block:\n%s", block.Text)
	}
}

func TestSessionBlockDoesNotFollowALinkOutOfTheHome(t *testing.T) {
	t.Parallel()
	outside := writeHome(t, map[string]string{"secret.md": "secret"})
	home := writeHome(t, nil)
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(home, InstructionsFile)); err != nil {
		t.Skip("no symlinks here:", err)
	}
	block := ReadSessionBlock(home)
	if strings.Contains(block.Text, "secret\n") || strings.HasSuffix(block.Text, "secret") || len(block.Problems) != 1 {
		t.Fatalf("followed a link out of the home: %+v", block)
	}
}

func TestCleanTextKeepsMarkdownAndDropsHiddenCharacters(t *testing.T) {
	t.Parallel()
	got := CleanText("# Title\r\n\tindent\x00\x1b[31m red\u202e evil\u2066\u2028x\uFEFF\xff")
	if want := "# Title\n\tindent[31m red evilx\uFFFD"; got != want {
		t.Fatalf("CleanText = %q, want %q", got, want)
	}
}

func TestRoleLine(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ text, want string }{
		"plain":   {"Release lead for the app.\nmore", "Release lead for the app."},
		"heading": {"\n\n#  Release lead\n", "Release lead"},
		"spaces":  {"a\tb   c\n", "a b c"},
		"long":    {strings.Repeat("r", RoleRunes+5), strings.Repeat("r", RoleRunes) + "\u2026"},
		"hidden":  {"lead\u202e\x07 role", "lead role"},
		"empty":   {"\n \n", ""},
	} {
		home := writeHome(t, map[string]string{InstructionsFile: tc.text})
		got, err := RoleLine(home)
		if err != nil || got != tc.want {
			t.Errorf("%s: RoleLine = %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := RoleLine(writeHome(t, nil)); got != "" || err != nil {
		t.Errorf("no file: %q, %v", got, err)
	}
	dir := writeHome(t, nil)
	if err := os.Mkdir(filepath.Join(dir, InstructionsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RoleLine(dir); err == nil {
		t.Error("an unreadable instructions file must be an error")
	}
}

func TestReadAttachedRendersEachFileUnderItsName(t *testing.T) {
	t.Parallel()
	home := writeHome(t, map[string]string{
		"verification.md":  "Run make go-test.\n",
		"memory/rule.md":   "---\nname: rule\n---\nbody\n",
		"rulings.md":       "- 2026-10-13: no em dashes\n",
		"memory/unused.md": "not attached",
	})
	got, err := ReadAttached(home, []string{"verification.md", " ./memory/rule.md "})
	if err != nil {
		t.Fatal(err)
	}
	want := AttachedOpening + "\n\n### verification.md\n\nRun make go-test.\n\n### memory/rule.md\n\n---\nname: rule\n---\nbody\n\n" + AttachedClosing
	if got != want {
		t.Fatalf("attached:\n%s\nwant:\n%s", got, want)
	}
}

func TestReadAttachedRefusals(t *testing.T) {
	t.Parallel()
	outside := writeHome(t, map[string]string{"secret.md": "secret"})
	home := writeHome(t, map[string]string{
		"a.md":   "a",
		"big.md": strings.Repeat("b", HomeFilesCap-10),
		"c.md":   strings.Repeat("c", 11),
	})
	if err := os.Mkdir(filepath.Join(home, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(home, "link.md")) == nil
	many := make([]string, MaxHomeFiles+1)
	for i := range many {
		many[i] = "a.md"
	}
	cases := map[string]struct {
		paths []string
		want  string
	}{
		"absolute":  {[]string{filepath.Join(outside, "secret.md")}, "is absolute"},
		"slash":     {[]string{"/etc/passwd"}, "is absolute"},
		"parent":    {[]string{"../" + filepath.Base(outside) + "/secret.md"}, "leaves your home"},
		"inner dot": {[]string{"memory/../../x"}, "leaves your home"},
		"home":      {[]string{"."}, "names your home itself"},
		"empty":     {[]string{" "}, "empty path"},
		"duplicate": {[]string{"a.md", "./a.md"}, "names a.md twice"},
		"missing":   {[]string{"nope.md"}, "nope.md is not in your home"},
		"directory": {[]string{"dir"}, "not a regular file"},
		"too many":  {many, "at most 16"},
		"too big":   {[]string{"big.md", "c.md"}, "more than 32 KiB"},
	}
	if linked {
		cases["link out"] = struct {
			paths []string
			want  string
		}{[]string{"link.md"}, "link.md"}
	}
	for name, tc := range cases {
		got, err := ReadAttached(home, tc.paths)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ReadAttached = %q, %v; want an error with %q", name, got, err, tc.want)
		}
		if strings.Contains(got, "secret") {
			t.Errorf("%s: read outside the home", name)
		}
	}
	// Exactly at the cap is accepted.
	if _, err := ReadAttached(home, []string{"big.md", "a.md"}); err != nil {
		t.Errorf("at the cap: %v", err)
	}
}

func TestTheSkillNamesTheLiterals(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "assets", "skills", "pstack", "skills", "agent-home", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(raw)
	for _, literal := range []string{
		InstructionsFile, MemoryIndexFile, ArchiveMarker, "8 KiB", "4 KiB", "32 KiB", "home_files",
		"name: agent-home", "disable-model-invocation: false",
	} {
		if !strings.Contains(skill, literal) {
			t.Errorf("the agent-home skill does not name %q", literal)
		}
	}
	if !strings.Contains(LoadSkillSentence, "Load the agent-home skill before any work.") {
		t.Error("the load sentence names another skill")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
