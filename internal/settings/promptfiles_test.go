package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func claudeOverridesPatch(entries ...map[string]any) map[string]any {
	return map[string]any{"claudePromptOverrides": entries}
}

// rawUserRow decodes the stored user-tier row of one prompt override key as
// generic objects, so the test sees exactly what is persisted.
func rawUserRow(t *testing.T, store *fakeTierStore, key string) []map[string]any {
	t.Helper()
	value, ok := store.scopes[UserScope][key]
	if !ok {
		t.Fatalf("no %s row in %s: %v", key, UserScope, store.scopes[UserScope])
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(value), &rows); err != nil {
		t.Fatalf("decode %s row %s: %v", key, value, err)
	}
	return rows
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// writeExternally replaces a prompt file the way an editor would and moves
// its mtime, so the edit is visible even on a filesystem whose timestamp
// granularity is coarser than the test.
func writeExternally(t *testing.T, path, text string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestPromptOverrideTextIsStoredInItsFileAndNeverInTheRow(t *testing.T) {
	svc, store := tieredService(t)
	updated, err := svc.Update(claudeOverridesPatch(
		map[string]any{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "  opus prompt  "},
	))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	entry := updated.ClaudePromptOverrides[0]
	if entry.ID == "" || entry.ID != strings.ToLower(entry.ID) {
		t.Fatalf("id = %q, want a minted canonical id", entry.ID)
	}
	wantPath := filepath.Join(filepath.Dir(svc.Path()), "prompts", entry.ID+".md")
	if entry.Path != wantPath || entry.Prompt != "opus prompt" {
		t.Fatalf("entry = %+v, want path %s and the trimmed prompt", entry, wantPath)
	}
	if got := readText(t, wantPath); got != "opus prompt\n" {
		t.Fatalf("prompt file = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(wantPath)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("prompt file mode = %o, want 600", mode)
		}
		dirInfo, err := os.Stat(filepath.Dir(wantPath))
		if err != nil {
			t.Fatal(err)
		}
		if mode := dirInfo.Mode().Perm(); mode != 0o700 {
			t.Errorf("prompts dir mode = %o, want 700", mode)
		}
	}

	rows := rawUserRow(t, store, "claudePromptOverrides")
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	for _, key := range []string{"prompt", "path", "problem"} {
		if _, present := rows[0][key]; present {
			t.Errorf("persisted row carries %q: %v", key, rows[0])
		}
	}
	if rows[0]["id"] != entry.ID {
		t.Errorf("persisted id = %v, want %s", rows[0]["id"], entry.ID)
	}

	// A fresh service over the same directory and store reads the text back
	// from the file.
	fresh := NewService(filepath.Dir(svc.Path()))
	fresh.AttachTierStore(store, testBackendBucket)
	reread := fresh.Get().ClaudePromptOverrides
	if len(reread) != 1 || reread[0].Prompt != "opus prompt" || reread[0].Path != wantPath {
		t.Fatalf("reread = %+v", reread)
	}
}

func TestPromptOverrideIDsAreValidatedAsFilenames(t *testing.T) {
	const id = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c"
	cases := map[string]map[string]any{
		"not a uuid": claudeOverridesPatch(map[string]any{"id": "../escape", "prompt": "p"}),
		"duplicate": claudeOverridesPatch(
			map[string]any{"id": id, "prompt": "a"},
			map[string]any{"id": id, "prompt": "b"},
		),
		"shared across providers": {
			"claudePromptOverrides": []map[string]any{{"id": id, "prompt": "a"}},
			"codexPromptOverrides":  []map[string]any{{"id": id, "prompt": "b"}},
		},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _ := tieredService(t)
			if _, err := svc.Update(patch); err == nil {
				t.Fatal("Update accepted it")
			}
			if _, err := os.Stat(svc.PromptFilesDir()); !os.IsNotExist(err) {
				t.Fatalf("a rejected write touched the prompts dir: %v", err)
			}
		})
	}

	// A non-canonical spelling of a valid UUID is stored canonically.
	svc, _ := tieredService(t)
	updated, err := svc.Update(claudeOverridesPatch(map[string]any{"id": "{" + strings.ToUpper(id) + "}", "prompt": "p"}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := updated.ClaudePromptOverrides[0].ID; got != id {
		t.Fatalf("id = %q, want %q", got, id)
	}
}

// The lenient pair of the strict check above: a hand-edited row with a bad
// or duplicate id loses the id, logged, and is never read from that path.
func TestSanitizeClearsPromptOverrideIDsThatCannotNameAFile(t *testing.T) {
	const id = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c"
	var sanitized Settings
	logged := captureLog(t, func() {
		sanitized = sanitizeLoadedSettings(Settings{
			TimestampFormat:                  DefaultSettings.TimestampFormat,
			ClaudeAutoCompactStandardPercent: 90,
			ClaudePromptOverrides: []PromptOverride{
				{ID: "../../etc/passwd"},
				{ID: id},
				{ID: id},
			},
			CodexPromptOverrides: []PromptOverride{{ID: id}},
		})
	})
	got := []string{
		sanitized.ClaudePromptOverrides[0].ID,
		sanitized.ClaudePromptOverrides[1].ID,
		sanitized.ClaudePromptOverrides[2].ID,
		sanitized.CodexPromptOverrides[0].ID,
	}
	if !slices.Equal(got, []string{"", id, "", ""}) {
		t.Fatalf("ids = %q", got)
	}
	if strings.Count(logged, "clearing it") != 3 {
		t.Fatalf("log = %q, want one line per cleared id", logged)
	}
}

func TestAnExternalPromptFileEditIsSeenByTheNextGet(t *testing.T) {
	svc, _ := tieredService(t)
	updated, err := svc.Update(claudeOverridesPatch(map[string]any{"enabled": true, "models": []string{"m"}, "prompt": "first"}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	path := updated.ClaudePromptOverrides[0].Path
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "first" {
		t.Fatalf("cached prompt = %q", got)
	}
	writeExternally(t, path, "edited outside\n", time.Now().Add(time.Hour))
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "edited outside" {
		t.Fatalf("prompt after external edit = %q, want the file's text", got)
	}
	// Deleting the file is a change too.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "" {
		t.Fatalf("prompt after the file was removed = %q, want empty", got)
	}
}

func TestPromptFileWritesAreAnnouncedBeforeTheyLand(t *testing.T) {
	svc, _ := tieredService(t)
	type note struct {
		path    string
		content string
		exists  bool
	}
	var notes []note
	svc.SetPromptFileWriteObserver(func(path string) {
		data, err := os.ReadFile(path)
		notes = append(notes, note{path: path, content: string(data), exists: err == nil})
	})
	first, err := svc.Update(claudeOverridesPatch(
		map[string]any{"prompt": "one"},
		map[string]any{"prompt": "two"},
	))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(notes) != 2 || notes[0].exists || notes[1].exists {
		t.Fatalf("notes = %+v, want two paths reported before their files existed", notes)
	}
	entries := first.ClaudePromptOverrides

	// Editing one entry rewrites only its file, announced while the old
	// text is still there.
	notes = nil
	if _, err := svc.Update(claudeOverridesPatch(
		map[string]any{"id": entries[0].ID, "prompt": "one edited"},
		map[string]any{"id": entries[1].ID, "prompt": "two"},
	)); err != nil {
		t.Fatalf("edit Update: %v", err)
	}
	if len(notes) != 1 || notes[0].path != entries[0].Path || notes[0].content != "one\n" {
		t.Fatalf("notes = %+v, want only the edited file, before the write", notes)
	}
	if got := readText(t, entries[0].Path); got != "one edited\n" {
		t.Fatalf("edited file = %q", got)
	}

	// Dropping an entry removes its file, announced first.
	notes = nil
	if _, err := svc.Update(claudeOverridesPatch(
		map[string]any{"id": entries[0].ID, "prompt": "one edited"},
	)); err != nil {
		t.Fatalf("remove Update: %v", err)
	}
	if len(notes) != 1 || notes[0].path != entries[1].Path || !notes[0].exists {
		t.Fatalf("notes = %+v, want the removed entry's file before removal", notes)
	}
	if _, err := os.Stat(entries[1].Path); !os.IsNotExist(err) {
		t.Fatalf("removed entry's file still exists: %v", err)
	}
	if _, err := os.Stat(entries[0].Path); err != nil {
		t.Fatalf("kept entry's file: %v", err)
	}
}

// Saving back exactly what GetSettings answered, computed fields included,
// must move nothing: no announcement and no file write.
func TestARepeatSaveOfTheLoadedEntriesMovesNothing(t *testing.T) {
	svc, store := tieredService(t)
	if _, err := svc.Update(claudeOverridesPatch(map[string]any{"enabled": true, "models": []string{"m"}, "prompt": "p"})); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var announced [][]TierChange
	var written []string
	fresh := NewService(filepath.Dir(svc.Path()))
	fresh.AttachTierStore(store, testBackendBucket)
	fresh.SetChangeObserver(func(changes []TierChange) { announced = append(announced, changes) })
	fresh.SetPromptFileWriteObserver(func(path string) { written = append(written, path) })
	loaded := fresh.Get().ClaudePromptOverrides
	if _, err := fresh.Update(map[string]any{"claudePromptOverrides": loaded}); err != nil {
		t.Fatalf("repeat Update: %v", err)
	}
	if len(announced) != 0 || len(written) != 0 {
		t.Fatalf("repeat save announced %+v and wrote %v", announced, written)
	}
}

// A client cannot set the computed fields: they are recomputed.
func TestAClientPathOrProblemIsIgnored(t *testing.T) {
	svc, store := tieredService(t)
	updated, err := svc.Update(claudeOverridesPatch(map[string]any{
		"prompt": "p", "path": "/etc/passwd", "problem": "made up",
	}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	entry := updated.ClaudePromptOverrides[0]
	if entry.Path != svc.promptFilePath(entry.ID) || entry.Problem != "" {
		t.Fatalf("entry = %+v, want recomputed fields", entry)
	}
	if rows := rawUserRow(t, store, "claudePromptOverrides"); rows[0]["path"] != nil || rows[0]["problem"] != nil {
		t.Fatalf("row = %v", rows[0])
	}
}

func TestAnOversizedPromptFileIsReportedAndKept(t *testing.T) {
	svc, _ := tieredService(t)
	updated, err := svc.Update(claudeOverridesPatch(map[string]any{"enabled": true, "models": []string{"m"}, "prompt": "short"}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	path := updated.ClaudePromptOverrides[0].Path
	big := strings.Repeat("x", MaxPromptOverrideLen+1)
	writeExternally(t, path, big, time.Now().Add(time.Hour))

	var entry PromptOverride
	logged := captureLog(t, func() { entry = svc.Get().ClaudePromptOverrides[0] })
	if entry.Prompt != "" {
		t.Fatalf("an oversized file's text was applied (%d bytes)", len(entry.Prompt))
	}
	for _, want := range []string{path, "64001", "64000"} {
		if !strings.Contains(entry.Problem, want) {
			t.Errorf("problem %q does not name %q", entry.Problem, want)
		}
	}
	if strings.Count(logged, path) != 1 {
		t.Fatalf("log = %q, want the problem logged once per load", logged)
	}
	// Cached reads do not log it again.
	if again := captureLog(t, func() { svc.Get() }); again != "" {
		t.Fatalf("a cached read logged %q", again)
	}

	// Toggling the entry leaves the user's file alone and keeps the problem.
	toggled, err := svc.Update(claudeOverridesPatch(map[string]any{
		"id": entry.ID, "enabled": false, "models": []string{"m"}, "prompt": "",
	}))
	if err != nil {
		t.Fatalf("toggle Update: %v", err)
	}
	if got := readText(t, path); got != big {
		t.Fatalf("toggle rewrote the oversized file (%d bytes)", len(got))
	}
	if toggled.ClaudePromptOverrides[0].Problem == "" {
		t.Fatal("toggle dropped the problem")
	}
	// Typing new text replaces the file and clears the problem.
	fixed, err := svc.Update(claudeOverridesPatch(map[string]any{"id": entry.ID, "prompt": "fixed"}))
	if err != nil {
		t.Fatalf("fix Update: %v", err)
	}
	if fixed.ClaudePromptOverrides[0].Problem != "" || readText(t, path) != "fixed\n" {
		t.Fatalf("fix = %+v, file %q", fixed.ClaudePromptOverrides[0], readText(t, path))
	}
}

// Files that each fit but together pass the provider total keep their
// entries, with a problem on the ones past the limit.
func TestPromptFilesPastTheProviderTotalAreReportedNotDropped(t *testing.T) {
	svc, _ := tieredService(t)
	entries := make([]map[string]any, 0, 9)
	for range 9 {
		entries = append(entries, map[string]any{"prompt": "x"})
	}
	updated, err := svc.Update(claudeOverridesPatch(entries...))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	later := time.Now().Add(time.Hour)
	for _, entry := range updated.ClaudePromptOverrides {
		writeExternally(t, entry.Path, strings.Repeat("y", MaxPromptOverrideLen), later)
	}
	loaded := svc.Get().ClaudePromptOverrides
	if len(loaded) != 9 {
		t.Fatalf("entries = %d, want all 9 kept", len(loaded))
	}
	for i, entry := range loaded {
		applied := i < MaxPromptOverridesTotalLen/MaxPromptOverrideLen
		if applied != (entry.Prompt != "") || applied != (entry.Problem == "") {
			t.Errorf("entry %d: prompt %d bytes, problem %q", i, len(entry.Prompt), entry.Problem)
		}
	}
}

func TestMigratePromptFilesMovesInlineTextOutOfTheRow(t *testing.T) {
	const withID = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c"
	svc, store := tieredService(t)
	store.scopes[UserScope] = map[string]string{
		// A pre-migration row (no id), and a row with an id whose file is
		// missing but whose text is still inline.
		"claudePromptOverrides": `[{"enabled":true,"models":["m"],"prompt":"legacy claude"}]`,
		"codexPromptOverrides":  `[{"id":"` + withID + `","enabled":true,"models":["g"],"prompt":"legacy codex"}]`,
	}
	svc.InvalidateTierCache()
	before := svc.Get()
	if before.ClaudePromptOverrides[0].Prompt != "legacy claude" || before.ClaudePromptOverrides[0].Path != "" {
		t.Fatalf("pre-migration read = %+v", before.ClaudePromptOverrides[0])
	}
	if before.CodexPromptOverrides[0].Prompt != "legacy codex" {
		t.Fatalf("inline text of an id row with no file = %+v", before.CodexPromptOverrides[0])
	}

	var announced [][]TierChange
	svc.SetChangeObserver(func(changes []TierChange) { announced = append(announced, changes) })
	if err := svc.MigratePromptFiles(); err != nil {
		t.Fatalf("MigratePromptFiles: %v", err)
	}
	for _, key := range promptOverrideKeys {
		for _, row := range rawUserRow(t, store, key) {
			if _, present := row["prompt"]; present {
				t.Errorf("%s row still carries its text: %v", key, row)
			}
			if row["id"] == "" || row["id"] == nil {
				t.Errorf("%s row has no id: %v", key, row)
			}
		}
	}
	after := svc.Get()
	claude := after.ClaudePromptOverrides[0]
	if claude.Prompt != "legacy claude" || readText(t, claude.Path) != "legacy claude\n" {
		t.Fatalf("migrated claude entry = %+v", claude)
	}
	if got := readText(t, svc.promptFilePath(withID)); got != "legacy codex\n" {
		t.Fatalf("migrated codex file = %q", got)
	}
	if len(announced) != 1 || announced[0][0].Tier != TierUser {
		t.Fatalf("announced = %+v", announced)
	}
	if _, err := os.Stat(svc.Path()); !os.IsNotExist(err) {
		t.Fatalf("migration wrote settings.json: %v", err)
	}

	// Nothing pending: a second call writes and announces nothing.
	announced = nil
	var written []string
	svc.SetPromptFileWriteObserver(func(path string) { written = append(written, path) })
	if err := svc.MigratePromptFiles(); err != nil {
		t.Fatalf("second MigratePromptFiles: %v", err)
	}
	if len(announced) != 0 || len(written) != 0 {
		t.Fatalf("idle migration announced %+v and wrote %v", announced, written)
	}
	if _, err := os.Stat(svc.Path()); !os.IsNotExist(err) {
		t.Fatalf("idle migration wrote settings.json: %v", err)
	}
}

// Before AttachTierStore the user tier is still settings.json: a store-less
// write keeps the text inline and creates no prompt file, and migration
// waits for the store. The harness seeder and the boot readers are this
// service.
func TestAStoreLessServiceKeepsPromptTextInline(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	if _, err := svc.Update(map[string]any{"claudeBinaryPath": "/bin/true"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.Update(claudeOverridesPatch(map[string]any{"prompt": "inline"})); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.MigratePromptFiles(); err != nil {
		t.Fatalf("MigratePromptFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prompts")); !os.IsNotExist(err) {
		t.Fatalf("a store-less service created the prompts dir: %v", err)
	}
	if got := svc.Get().ClaudePromptOverrides[0]; got.Prompt != "inline" || got.ID != "" || got.Path != "" {
		t.Fatalf("store-less entry = %+v", got)
	}

	// The store arrives: seeding copies the inline row, migration files it.
	store := newFakeTierStore()
	svc.AttachTierStore(store, testBackendBucket)
	if err := svc.MigratePromptFiles(); err != nil {
		t.Fatalf("MigratePromptFiles: %v", err)
	}
	entry := svc.Get().ClaudePromptOverrides[0]
	if entry.ID == "" || entry.Prompt != "inline" || readText(t, entry.Path) != "inline\n" {
		t.Fatalf("migrated entry = %+v", entry)
	}
	if _, present := rawUserRow(t, store, "claudePromptOverrides")[0]["prompt"]; present {
		t.Fatal("the seeded row kept its text after migration")
	}
}

func TestReloadPromptFilesAnnouncesBothKeysAndDropsTheCache(t *testing.T) {
	svc, _ := tieredService(t)
	updated, err := svc.Update(claudeOverridesPatch(map[string]any{"prompt": "first"}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	var announced [][]TierChange
	svc.SetChangeObserver(func(changes []TierChange) { announced = append(announced, changes) })
	svc.ReloadPromptFiles()
	want := []TierChange{{Tier: TierUser, Keys: []string{"claudePromptOverrides", "codexPromptOverrides"}}}
	if len(announced) != 1 || len(announced[0]) != 1 || announced[0][0].Tier != want[0].Tier ||
		!slices.Equal(announced[0][0].Keys, want[0].Keys) {
		t.Fatalf("announced = %+v, want %+v", announced, want)
	}
	// Same size and an mtime restored to the cached one: only the dropped
	// cache can make this edit visible.
	path := updated.ClaudePromptOverrides[0].Path
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	svc.Get()
	writeExternally(t, path, "other\n", info.ModTime())
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "first" {
		t.Fatalf("a same-stamp edit was visible without a reload: %q", got)
	}
	svc.ReloadPromptFiles()
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "other" {
		t.Fatalf("prompt after ReloadPromptFiles = %q", got)
	}
}
