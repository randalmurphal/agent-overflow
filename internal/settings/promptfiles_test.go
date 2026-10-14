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

// addClaudeOverrides adds entries through Update, the way Settings adds
// them, and answers the saved entries. Each starts with an empty file.
func addClaudeOverrides(t *testing.T, svc *Service, entries ...map[string]any) []PromptOverride {
	t.Helper()
	updated, err := svc.Update(claudeOverridesPatch(entries...))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return updated.ClaudePromptOverrides
}

func TestPromptOverrideTextIsStoredInItsFileAndNeverInTheRow(t *testing.T) {
	svc, store := tieredService(t)
	entry := addClaudeOverrides(t, svc,
		map[string]any{"enabled": true, "models": []string{"claude-opus-5"}, "prompt": "a caller's text"},
	)[0]
	if entry.ID == "" || entry.ID != strings.ToLower(entry.ID) {
		t.Fatalf("id = %q, want a minted canonical id", entry.ID)
	}
	wantPath := filepath.Join(filepath.Dir(svc.Path()), "prompts", entry.ID+".md")
	if entry.Path != wantPath || entry.Prompt != "" {
		t.Fatalf("entry = %+v, want path %s and no text from the caller", entry, wantPath)
	}
	if got := readText(t, wantPath); got != "" {
		t.Fatalf("an added entry's file = %q, want empty", got)
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

	writeExternally(t, wantPath, "  opus prompt  \n", time.Now().Add(time.Hour))
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "opus prompt" {
		t.Fatalf("prompt after the file was written = %q, want the trimmed file text", got)
	}

	rows := rawUserRow(t, store, "claudePromptOverrides")
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	for _, key := range []string{"prompt", "path", "problem", "blank"} {
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

// A write never takes text from the caller: a toggle that sends some keeps
// the file as it is, and an id the service did not mint is replaced, so a
// caller cannot point an entry at a file of its choosing.
func TestAWriteIgnoresTheCallersPromptAndUnknownIDs(t *testing.T) {
	svc, _ := tieredService(t)
	entry := addClaudeOverrides(t, svc, map[string]any{"enabled": true, "models": []string{"m"}})[0]
	writeExternally(t, entry.Path, "file text\n", time.Now().Add(time.Hour))

	toggled, err := svc.Update(claudeOverridesPatch(map[string]any{
		"id": entry.ID, "enabled": false, "models": []string{"m"}, "prompt": "client text", "blank": true,
	}))
	if err != nil {
		t.Fatalf("toggle Update: %v", err)
	}
	if got := readText(t, entry.Path); got != "file text\n" {
		t.Fatalf("file after a write carrying text = %q, want it untouched", got)
	}
	if got := toggled.ClaudePromptOverrides[0]; got.Prompt != "file text" || got.Enabled || got.Blank {
		t.Fatalf("toggled entry = %+v, want the file's text and the new toggle", got)
	}

	const orphanID = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c"
	orphan := svc.promptFilePath(orphanID)
	writeExternally(t, orphan, "someone else's text\n", time.Now().Add(2*time.Hour))
	added, err := svc.Update(claudeOverridesPatch(
		map[string]any{"id": entry.ID, "enabled": false, "models": []string{"m"}},
		map[string]any{"id": orphanID, "enabled": true, "models": []string{"m"}, "prompt": "client text"},
	))
	if err != nil {
		t.Fatalf("add Update: %v", err)
	}
	second := added.ClaudePromptOverrides[1]
	if second.ID == orphanID || second.ID == "" {
		t.Fatalf("added entry id = %q, want a fresh id rather than the caller's", second.ID)
	}
	if got := readText(t, second.Path); got != "" {
		t.Fatalf("added entry's file = %q, want empty", got)
	}
	if got := readText(t, orphan); got != "someone else's text\n" {
		t.Fatalf("the file the caller named = %q, want it untouched", got)
	}
}

func TestPromptOverrideIDsAreValidatedAsFilenames(t *testing.T) {
	const id = "0b7a2c4e-9f1d-4e3a-8c5b-2d6f7a8e9b0c"
	cases := map[string]map[string]any{
		"not a uuid": claudeOverridesPatch(map[string]any{"id": "../escape"}),
		"duplicate": claudeOverridesPatch(
			map[string]any{"id": id},
			map[string]any{"id": id},
		),
		"shared across providers": {
			"claudePromptOverrides": []map[string]any{{"id": id}},
			"codexPromptOverrides":  []map[string]any{{"id": id}},
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

	// A non-canonical spelling of an entry's id names the same entry.
	svc, _ := tieredService(t)
	entry := addClaudeOverrides(t, svc, map[string]any{"enabled": true})[0]
	updated, err := svc.Update(claudeOverridesPatch(map[string]any{"id": "{" + strings.ToUpper(entry.ID) + "}"}))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := updated.ClaudePromptOverrides[0].ID; got != entry.ID {
		t.Fatalf("id = %q, want %q", got, entry.ID)
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
	path := addClaudeOverrides(t, svc, map[string]any{"enabled": true, "models": []string{"m"}})[0].Path
	writeExternally(t, path, "first\n", time.Now().Add(time.Hour))
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "first" {
		t.Fatalf("prompt = %q", got)
	}
	writeExternally(t, path, "edited outside\n", time.Now().Add(2*time.Hour))
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

// Adding entries creates their empty files; removing one deletes its file
// and leaves the others alone.
func TestAddingCreatesAnEmptyFileAndRemovingDeletesIt(t *testing.T) {
	svc, _ := tieredService(t)
	entries := addClaudeOverrides(t, svc, map[string]any{"enabled": true}, map[string]any{"enabled": true})
	for _, entry := range entries {
		if got := readText(t, entry.Path); got != "" {
			t.Fatalf("new entry file %s = %q, want empty", entry.Path, got)
		}
	}
	writeExternally(t, entries[0].Path, "kept\n", time.Now().Add(time.Hour))

	if _, err := svc.Update(claudeOverridesPatch(map[string]any{"id": entries[0].ID, "enabled": true})); err != nil {
		t.Fatalf("remove Update: %v", err)
	}
	if _, err := os.Stat(entries[1].Path); !os.IsNotExist(err) {
		t.Fatalf("removed entry's file still exists: %v", err)
	}
	if got := readText(t, entries[0].Path); got != "kept\n" {
		t.Fatalf("kept entry's file = %q", got)
	}

	if _, err := svc.Update(claudeOverridesPatch()); err != nil {
		t.Fatalf("remove all Update: %v", err)
	}
	if _, err := os.Stat(entries[0].Path); !os.IsNotExist(err) {
		t.Fatalf("last entry's file still exists: %v", err)
	}
	if got := svc.Get().ClaudePromptOverrides; len(got) != 0 {
		t.Fatalf("entries after removing all = %+v", got)
	}
}

// Emptying a list stores a row a read can decode. An omitempty field at its
// zero value has no projection, and the row used to be stored empty, which
// every later read logged as unreadable and dropped.
func TestEmptyingAListStoresADecodableRow(t *testing.T) {
	svc, store := tieredService(t)
	addClaudeOverrides(t, svc, map[string]any{"enabled": true})
	if _, err := svc.Update(map[string]any{"claudeHiddenModels": []string{"claude-opus-5"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.Update(map[string]any{"claudePromptOverrides": []any{}, "claudeHiddenModels": []string{}}); err != nil {
		t.Fatalf("clear Update: %v", err)
	}
	for _, key := range []string{"claudePromptOverrides", "claudeHiddenModels"} {
		value, ok := store.scopes[UserScope][key]
		if !ok || value == "" {
			t.Fatalf("%s row = %q (present %v), want the empty value's encoding", key, value, ok)
		}
	}
	fresh := NewService(filepath.Dir(svc.Path()))
	fresh.AttachTierStore(store, testBackendBucket)
	var got Settings
	if logged := captureLog(t, func() { got = fresh.Get() }); strings.Contains(logged, "unreadable") {
		t.Fatalf("reading the emptied rows logged %q", logged)
	}
	if len(got.ClaudePromptOverrides) != 0 || len(got.ClaudeHiddenModels) != 0 {
		t.Fatalf("emptied lists read back as %+v and %v", got.ClaudePromptOverrides, got.ClaudeHiddenModels)
	}
}

// Saving back exactly what was loaded, computed fields included, must move
// nothing: no announcement and no file write.
func TestARepeatSaveOfTheLoadedEntriesMovesNothing(t *testing.T) {
	svc, store := tieredService(t)
	path := addClaudeOverrides(t, svc, map[string]any{"enabled": true, "models": []string{"m"}})[0].Path
	past := time.Now().Add(-time.Hour)
	writeExternally(t, path, "p\n", past)
	var announced [][]TierChange
	fresh := NewService(filepath.Dir(svc.Path()))
	fresh.AttachTierStore(store, testBackendBucket)
	fresh.SetChangeObserver(func(changes []TierChange) { announced = append(announced, changes) })
	loaded := fresh.Get().ClaudePromptOverrides
	if _, err := fresh.Update(map[string]any{"claudePromptOverrides": PromptOverridesForWire(loaded)}); err != nil {
		t.Fatalf("repeat Update: %v", err)
	}
	if len(announced) != 0 {
		t.Fatalf("repeat save announced %+v", announced)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) || readText(t, path) != "p\n" {
		t.Fatalf("repeat save rewrote the prompt file (mtime %v)", info.ModTime())
	}
}

// A client cannot set the computed fields: they are recomputed.
func TestAClientPathOrProblemIsIgnored(t *testing.T) {
	svc, store := tieredService(t)
	entry := addClaudeOverrides(t, svc, map[string]any{
		"path": "/etc/passwd", "problem": "made up", "blank": true,
	})[0]
	if entry.Path != svc.promptFilePath(entry.ID) || entry.Problem != "" || entry.Blank {
		t.Fatalf("entry = %+v, want recomputed fields", entry)
	}
	if rows := rawUserRow(t, store, "claudePromptOverrides"); rows[0]["path"] != nil || rows[0]["problem"] != nil || rows[0]["blank"] != nil {
		t.Fatalf("row = %v", rows[0])
	}
}

func TestAnOversizedPromptFileIsReportedAndKept(t *testing.T) {
	svc, _ := tieredService(t)
	path := addClaudeOverrides(t, svc, map[string]any{"enabled": true, "models": []string{"m"}})[0].Path
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
		"id": entry.ID, "enabled": false, "models": []string{"m"},
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
	// Shortening the file clears the problem.
	writeExternally(t, path, "fixed\n", time.Now().Add(2*time.Hour))
	if fixed := svc.Get().ClaudePromptOverrides[0]; fixed.Problem != "" || fixed.Prompt != "fixed" {
		t.Fatalf("after the fix = %+v", fixed)
	}
}

// Files that each fit but together pass the provider total keep their
// entries, with a problem on the ones past the limit.
func TestPromptFilesPastTheProviderTotalAreReportedNotDropped(t *testing.T) {
	svc, _ := tieredService(t)
	entries := make([]map[string]any, 0, 9)
	for range 9 {
		entries = append(entries, map[string]any{"enabled": true})
	}
	added := addClaudeOverrides(t, svc, entries...)
	later := time.Now().Add(time.Hour)
	for _, entry := range added {
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
	codexPath := svc.promptFilePath(withID)
	if got := readText(t, codexPath); got != "legacy codex\n" {
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
	past := time.Now().Add(-time.Hour)
	for _, path := range []string{claude.Path, codexPath} {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.MigratePromptFiles(); err != nil {
		t.Fatalf("second MigratePromptFiles: %v", err)
	}
	if len(announced) != 0 {
		t.Fatalf("idle migration announced %+v", announced)
	}
	for _, path := range []string{claude.Path, codexPath} {
		if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(past) {
			t.Fatalf("idle migration rewrote %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(svc.Path()); !os.IsNotExist(err) {
		t.Fatalf("idle migration wrote settings.json: %v", err)
	}
}

// Before AttachTierStore the user tier is still settings.json, where a row
// from before the files keeps its text inline: a store-less service reads
// it, creates no prompt file, and migration waits for the store.
func TestAStoreLessServiceKeepsPromptTextInline(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"claudePromptOverrides":[{"enabled":true,"models":["m"],"prompt":"inline"}]}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewService(dir)
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
	path := addClaudeOverrides(t, svc, map[string]any{"enabled": true})[0].Path
	writeExternally(t, path, "first\n", time.Now().Add(time.Hour))
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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "first" {
		t.Fatalf("prompt = %q", got)
	}
	writeExternally(t, path, "other\n", info.ModTime())
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "first" {
		t.Fatalf("a same-stamp edit was visible without a reload: %q", got)
	}
	svc.ReloadPromptFiles()
	if got := svc.Get().ClaudePromptOverrides[0].Prompt; got != "other" {
		t.Fatalf("prompt after ReloadPromptFiles = %q", got)
	}
}
