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
	"unsafe"
)

func TestToolDefinitionsCoverBrowserSurfaceAndCodexParity(t *testing.T) {
	want := []string{"browser_assets", "browser_clipboard", "browser_console_logs", "browser_dom", "browser_downloads", "browser_evaluate", "browser_evaluate_readonly", "browser_locator", "browser_open", "browser_page", "browser_pane", "browser_pointer", "browser_screenshot", "browser_snapshot", "browser_wait"}
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
	if _, hasWaitDefs := byName["browser_wait"]["$defs"]; hasWaitDefs {
		t.Fatal("browser_wait should not duplicate locator $defs (use browser_locator action=wait)")
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
	if err := manager.prepareArtifacts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired artifact remains: %v", err)
	}
	if got := manager.artifactBytes.Used(); got != 5 {
		t.Fatalf("artifact bytes=%d", got)
	}
	reservation, err := manager.reserveArtifacts(10)
	if err != nil {
		t.Fatalf("small reservation refused: %v", err)
	}
	reservation.Settle(3)
	if got := manager.artifactBytes.Used(); got != 8 {
		t.Fatalf("settled bytes=%d", got)
	}
	if _, err := manager.reserveArtifacts(maxArtifactRootBytes); err == nil {
		t.Fatal("over-quota reservation accepted")
	}
}

func TestMCPRoutesEveryParityTool(t *testing.T) {
	server := NewMCPServer(&fakeController{}, true)
	url := registerTestThread(t, server)
	calls := []struct {
		id, name string
		args     map[string]any
	}{
		{"open-url", "browser_open", map[string]any{"url": "https://example.com"}},
		{"open-file", "browser_open", map[string]any{"path": "/repo/a.html"}},
		{"page-list", "browser_page", map[string]any{"action": "list"}},
		{"page-new", "browser_page", map[string]any{"action": "new"}},
		{"page-select", "browser_page", map[string]any{"action": "select", "page_id": "p"}},
		{"page-label", "browser_page", map[string]any{"action": "label", "page_id": "p", "label": "preview"}},
		{"page-close", "browser_page", map[string]any{"action": "close", "page_id": "p"}},
		{"page-back", "browser_page", map[string]any{"action": "back"}},
		{"pane-session", "browser_pane", map[string]any{"action": "session", "name": "test"}},
		{"pane-visibility", "browser_pane", map[string]any{"action": "visibility"}},
		{"pane-vp-get", "browser_pane", map[string]any{"action": "viewport_get"}},
		{"pane-vp-set", "browser_pane", map[string]any{"action": "viewport_set", "width": 1024, "height": 768}},
		{"pane-vp-reset", "browser_pane", map[string]any{"action": "viewport_reset"}},
		{"snapshot", "browser_snapshot", map[string]any{}},
		{"screenshot", "browser_screenshot", map[string]any{}},
		{"locator", "browser_locator", map[string]any{"locator": map[string]any{"css": "button"}, "action": "count"}},
		{"pointer", "browser_pointer", map[string]any{"action": "move", "x": 1, "y": 1}},
		{"dom-get", "browser_dom", map[string]any{"action": "get_visible_dom"}},
		{"dom-click-sel", "browser_dom", map[string]any{"action": "click", "selector": "button"}},
		{"dom-type-sel", "browser_dom", map[string]any{"action": "type", "selector": "input", "text": "x", "clear": true}},
		{"dom-press", "browser_dom", map[string]any{"action": "keypress", "key": "Enter"}},
		{"dom-scroll-sel", "browser_dom", map[string]any{"action": "scroll", "selector": "main", "y": 100}},
		{"wait", "browser_wait", map[string]any{"milliseconds": 1}},
		{"eval-ro", "browser_evaluate_readonly", map[string]any{"expression": "document.title"}},
		{"eval", "browser_evaluate", map[string]any{"expression": "document.title"}},
		{"clipboard", "browser_clipboard", map[string]any{"action": "read"}},
		{"console", "browser_console_logs", map[string]any{}},
		{"downloads", "browser_downloads", map[string]any{"action": "list"}},
		{"assets", "browser_assets", map[string]any{"action": "list"}},
	}
	for _, tc := range calls {
		response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": tc.id, "method": "tools/call", "params": map[string]any{"name": tc.name, "arguments": tc.args}})
		if response["error"] != nil {
			t.Fatalf("%s (%s) returned RPC error: %#v", tc.id, tc.name, response)
		}
		result, _ := response["result"].(map[string]any)
		if isError, _ := result["isError"].(bool); isError {
			t.Fatalf("%s (%s) returned tool error: %#v", tc.id, tc.name, response)
		}
	}
	// Retired tool names return a clear migration hint instead of silent failure.
	for oldName, wantHint := range retiredBrowserToolHint {
		response := postRPC(t, url, map[string]any{"jsonrpc": "2.0", "id": "retired-" + oldName, "method": "tools/call", "params": map[string]any{"name": oldName, "arguments": map[string]any{}}})
		result, _ := response["result"].(map[string]any)
		if isError, _ := result["isError"].(bool); !isError {
			t.Fatalf("retired %s did not return tool error: %#v", oldName, response)
		}
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, wantHint) {
			t.Fatalf("retired %s error = %q, want hint %q", oldName, text, wantHint)
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
	dir, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dir.Close(); err != nil {
			t.Error(err)
		}
	})
	first, err := uniqueArtifactName(dir, "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteFile(first, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := uniqueArtifactName(dir, "file.txt")
	if err != nil || second != "file-1.txt" {
		t.Fatalf("collision name=%q err=%v", second, err)
	}
	for _, button := range []string{"left", "middle", "right", "back", "forward"} {
		if _, err := inputButton(button); err != nil {
			t.Fatalf("pointer button %q rejected: %v", button, err)
		}
	}
}

func TestSessionNameLimitCountsUnicodeCharacters(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{}, ManagerOptions{})
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
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

func TestConsoleRingKeepsOnlyTruncatedBytes(t *testing.T) {
	message := strings.Repeat("m", 4*maxConsoleMessageBytes)
	url := "data:text/javascript," + strings.Repeat("u", 4*maxBrowserURLBytes)
	p := &managedPage{}
	p.appendLog(ConsoleLog{Level: "log", Message: message, URL: url})
	p.logMu.Lock()
	defer p.logMu.Unlock()
	got := p.logs[0]
	if got.Message != message[:maxConsoleMessageBytes] || got.URL != url[:maxBrowserURLBytes] {
		t.Fatalf("message %d bytes, url %d bytes; want the leading %d and %d", len(got.Message), len(got.URL), maxConsoleMessageBytes, maxBrowserURLBytes)
	}
	if unsafe.StringData(got.Message) == unsafe.StringData(message) || unsafe.StringData(got.URL) == unsafe.StringData(url) {
		t.Fatal("a truncated console field still shares the engine's full value")
	}
}

// A truncated value is a copy, so keeping it does not keep the engine's whole
// string alive. A value within the limit is returned as is.
func TestTruncateUTF8CopiesWhatItKeeps(t *testing.T) {
	value := strings.Repeat("é", 1000)
	start := uintptr(unsafe.Pointer(unsafe.StringData(value)))
	end := start + uintptr(len(value))
	got := truncateUTF8(value, 101)
	if got != value[:100] {
		t.Fatalf("truncated to %d bytes, want the 100 before the split rune", len(got))
	}
	if at := uintptr(unsafe.Pointer(unsafe.StringData(got))); at >= start && at < end {
		t.Fatal("the truncated value points into the engine's string")
	}
	if kept := truncateUTF8(value, len(value)); unsafe.StringData(kept) != unsafe.StringData(value) {
		t.Fatal("a value within the limit was copied")
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
