package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestToolDefinitionsCoverBrowserSurfaceAndCodexParity(t *testing.T) {
	want := []string{"browser_assets", "browser_click", "browser_clipboard", "browser_close_page", "browser_console_logs", "browser_dom", "browser_downloads", "browser_evaluate", "browser_evaluate_readonly", "browser_history", "browser_label_page", "browser_locator", "browser_new_page", "browser_open", "browser_open_file", "browser_pages", "browser_pointer", "browser_press", "browser_screenshot", "browser_select_page", "browser_session", "browser_snapshot", "browser_scroll", "browser_type", "browser_viewport", "browser_visibility", "browser_wait"}
	sort.Strings(want)
	definitions := toolDefinitions()
	got := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		name, ok := definition["name"].(string)
		if !ok || name == "" {
			t.Fatalf("invalid tool definition: %#v", definition)
		}
		got = append(got, name)
		if _, err := json.Marshal(definition["inputSchema"]); err != nil {
			t.Fatalf("schema %s is not JSON: %v", name, err)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

func TestParityToolSchemasDescribeNestedArguments(t *testing.T) {
	definitions := toolDefinitions()
	byName := make(map[string]map[string]any, len(definitions))
	for _, definition := range definitions {
		byName[definition["name"].(string)] = definition["inputSchema"].(map[string]any)
	}
	locatorSchema := byName["browser_locator"]
	defs, ok := locatorSchema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("browser_locator schema has no reusable locator definition")
	}
	locator, ok := defs["locator"].(map[string]any)
	if !ok {
		t.Fatal("browser_locator locator definition missing")
	}
	properties := locator["properties"].(map[string]any)
	for _, field := range []string{"css", "role", "name", "text", "label", "placeholder", "test_id", "frames", "scope", "has", "has_not", "and", "or", "index"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("browser_locator schema omits %s", field)
		}
	}
	waitDefs, ok := byName["browser_wait"]["$defs"].(map[string]any)
	if !ok || waitDefs["locator"] == nil {
		t.Fatal("browser_wait schema does not describe its nested locator")
	}
	clipboardProperties := byName["browser_clipboard"]["properties"].(map[string]any)
	items := clipboardProperties["items"].(map[string]any)
	item := items["items"].(map[string]any)
	entryList := item["properties"].(map[string]any)["entries"].(map[string]any)
	entry := entryList["items"].(map[string]any)
	entryProperties := entry["properties"].(map[string]any)
	for _, field := range []string{"mimeType", "text", "base64"} {
		if _, ok := entryProperties[field]; !ok {
			t.Fatalf("browser_clipboard schema omits %s", field)
		}
	}
}

func TestArtifactQuotaPrunesOnlyExpiredFilesAndReservesAtomically(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root, Config{}, ManagerOptions{})
	if err := os.MkdirAll(manager.artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(manager.artifactRoot, "old")
	recent := filepath.Join(manager.artifactRoot, "recent")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recent, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-artifactMaxAge - time.Hour)
	if err := os.Chtimes(old, expired, expired); err != nil {
		t.Fatal(err)
	}
	manager.prepareArtifacts()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired artifact remains: %v", err)
	}
	if got := manager.artifactBytes.Load(); got != 5 {
		t.Fatalf("artifact bytes=%d", got)
	}
	if !manager.reserveArtifacts(10) {
		t.Fatal("small reservation refused")
	}
	manager.settleArtifactReservation(10, 3)
	if got := manager.artifactBytes.Load(); got != 8 {
		t.Fatalf("settled bytes=%d", got)
	}
	if manager.reserveArtifacts(maxArtifactRootBytes) {
		t.Fatal("over-quota reservation accepted")
	}
}

func TestMCPRoutesEveryParityTool(t *testing.T) {
	server := NewMCPServer(&fakeController{}, true)
	url := registerTestThread(t, server)
	calls := map[string]map[string]any{
		"browser_session": {"name": "test"}, "browser_open": {"url": "https://example.com"}, "browser_open_file": {"path": "/repo/a.html"}, "browser_pages": {}, "browser_select_page": {"page_id": "p"}, "browser_label_page": {"page_id": "p", "label": "preview"}, "browser_close_page": {"page_id": "p"}, "browser_visibility": {}, "browser_viewport": {"action": "get"}, "browser_snapshot": {}, "browser_screenshot": {}, "browser_locator": {"locator": map[string]any{"css": "button"}, "action": "count"}, "browser_click": {"selector": "button"}, "browser_type": {"selector": "input", "text": "x"}, "browser_press": {"key": "Enter"}, "browser_pointer": {"action": "move", "x": 1, "y": 1}, "browser_dom": {"action": "get_visible_dom"}, "browser_scroll": {"y": 1}, "browser_wait": {"milliseconds": 1}, "browser_history": {"action": "back"}, "browser_evaluate_readonly": {"expression": "document.title"}, "browser_evaluate": {"expression": "document.title"}, "browser_clipboard": {"action": "read"}, "browser_console_logs": {}, "browser_downloads": {"action": "list"}, "browser_assets": {"action": "list"},
		"browser_new_page": {},
	}
	for name, args := range calls {
		response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": name, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		if response["error"] != nil {
			t.Fatalf("%s returned RPC error: %#v", name, response)
		}
		result, _ := response["result"].(map[string]any)
		if isError, _ := result["isError"].(bool); isError {
			t.Fatalf("%s returned tool error: %#v", name, response)
		}
	}
}

func TestMCPRejectsUnknownArguments(t *testing.T) {
	fake := &fakeController{}
	server := NewMCPServer(fake, true)
	url := registerTestThread(t, server)
	response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "browser_open", "arguments": map[string]any{"url": "https://example.com", "typo": true}}})
	result := response["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("response=%#v", response)
	}
	if fake.openedURL != "" {
		t.Fatalf("unknown argument still dispatched: %q", fake.openedURL)
	}
}

func TestParityBoundsAndPureHelpers(t *testing.T) {
	if err := validateLocator(Locator{CSS: "button"}, 0); err != nil {
		t.Fatal(err)
	}
	bad := -1
	if err := validateLocator(Locator{CSS: "button", Index: &bad}, 0); err == nil {
		t.Fatal("negative locator index accepted")
	}
	matcher, err := globMatcher("https://example.com/**/done")
	if err != nil || !matcher.MatchString("https://example.com/a/b/done") || matcher.MatchString("https://other.test/a/done") {
		t.Fatalf("glob matcher=%v err=%v", matcher, err)
	}
	if got := safeArtifactName("../bad:name?.txt", ""); got != "bad_name_.txt" {
		t.Fatalf("safe name=%q", got)
	}
	if got := safeArtifactName("CON.txt", "download"); got != "_CON.txt" {
		t.Fatalf("Windows device name=%q", got)
	}
	dir := t.TempDir()
	first, err := uniqueArtifactPath(dir, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := uniqueArtifactPath(dir, "file.txt")
	if err != nil || filepath.Base(second) != "file-1.txt" {
		t.Fatalf("collision path=%q err=%v", second, err)
	}
	if !pathInside(dir, filepath.Join(dir, "nested", "file")) || pathInside(dir, filepath.Join(dir, "..", "escape")) {
		t.Fatal("artifact path containment failed")
	}
	for _, button := range []string{"left", "middle", "right", "back", "forward"} {
		if _, err := inputButton(button); err != nil {
			t.Fatalf("pointer button %q rejected: %v", button, err)
		}
	}
}

func TestSessionNameLimitCountsUnicodeCharacters(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{}, ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	if _, err := manager.NameSession(t.Context(), access, strings.Repeat("🔎", 120)); err != nil {
		t.Fatalf("120-character session name rejected: %v", err)
	}
	if _, err := manager.NameSession(t.Context(), access, strings.Repeat("🔎", 121)); err == nil {
		t.Fatal("121-character session name accepted")
	}
}

func TestConsoleRingIsBounded(t *testing.T) {
	p := &managedPage{}
	for i := 0; i < maxConsoleEntries+20; i++ {
		p.appendLog(ConsoleLog{Level: "log", Message: "entry"})
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if len(p.logs) != maxConsoleEntries {
		t.Fatalf("logs=%d", len(p.logs))
	}
}

func TestDOMNodeReferencesAreOpaqueAndBounded(t *testing.T) {
	p := &managedPage{nodeRefs: make(map[string]nodeReference)}
	first := p.rememberNode(nodeReference{Selector: "#first"})
	for i := 0; i < maxNodeReferences; i++ {
		p.rememberNode(nodeReference{Selector: "button"})
	}
	if len(p.nodeRefs) != maxNodeReferences {
		t.Fatalf("node refs=%d", len(p.nodeRefs))
	}
	if _, err := p.nodeReference(first); err == nil {
		t.Fatal("old node reference did not expire")
	}
	if _, err := p.nodeReference("css:#first"); err == nil {
		t.Fatal("forged selector was accepted as node reference")
	}
}

func TestReadOnlyPromiseUnwrapIsNarrow(t *testing.T) {
	if got := unwrapReadOnlyPromise(`Promise.resolve(document.title)`); got != "document.title" {
		t.Fatalf("unwrap=%q", got)
	}
	for _, expression := range []string{`Promise.resolve(document.title).then(String)`, `Promise.resolve((document.title)) + "x"`, `document.title`} {
		if got := unwrapReadOnlyPromise(expression); got != expression {
			t.Fatalf("unexpected unwrap %q => %q", expression, got)
		}
	}
}
