package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/chromium"
)

func TestManagerCodexBrowserParityWithManagedChrome(t *testing.T) {
	if os.Getenv("AO_BROWSER_INTEGRATION") != "1" {
		t.Skip("set AO_BROWSER_INTEGRATION=1 to exercise managed Chrome")
	}
	root := t.TempDir()
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Zl1sAAAAASUVORK5CYII=")
	heldStarted := make(chan struct{}, 1)
	releaseHeld := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case releaseHeld <- struct{}{}:
		default:
		}
	})
	cross := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><button id="cross" onclick="document.title='cross-clicked'">Cross frame</button>`))
	}))
	defer cross.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/asset.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		case "/download":
			w.Header().Set("Content-Disposition", `attachment; filename="fixture.txt"`)
			_, _ = w.Write([]byte("downloaded"))
		case "/held":
			select {
			case heldStarted <- struct{}{}:
			default:
			}
			<-releaseHeld
			_, _ = w.Write([]byte("released"))
		case "/next":
			_, _ = w.Write([]byte(`<!doctype html><title>Next</title><main>navigated</main>`))
		case "/frame":
			_, _ = w.Write([]byte(`<!doctype html><label for="inside">Frame field</label><input id="inside"><button id="frame-button">Frame button</button>`))
		default:
			_, _ = w.Write([]byte(`<!doctype html><title>Parity fixture</title><style>#drop{width:80px;height:40px}button{margin:4px}</style>
<main data-testid="main"><section id="form"><label for="name">Account name</label><input id="name" placeholder="Your name"><input id="check" type="checkbox"><select id="choice"><option value="a">Alpha</option><option value="b">Beta</option></select><button id="double" ondblclick="this.dataset.done='yes'">Double me</button></section>
<div class="card"><span>First card</span><button>Choose</button></div><div class="card special"><span>Second card</span><button>Choose</button></div>
<button id="pointer" onclick="this.dataset.clicked='yes';this.dataset.shift=String(event.shiftKey)" oncontextmenu="event.preventDefault();this.dataset.context='yes'">Pointer target</button><a id="next" href="/next">Next page</a><a id="download" href="/download">Download file</a>
<div id="drag" draggable="true">Drag</div><div id="drop" ondragover="event.preventDefault()" ondrop="this.dataset.dropped='yes'">Drop</div>
<img id="asset" src="/asset.png" alt="asset"><div id="styled-asset" style="background-image:url('/asset.png')"></div><svg aria-label="mark"><circle cx="1" cy="1" r="1"/></svg>
<iframe id="same" src="/frame"></iframe><iframe id="cross-frame" src="` + cross.URL + `"></iframe>
<script>console.warn('fixture warning');setTimeout(()=>{const x=document.createElement('div');x.id='later';x.textContent='ready later';document.body.append(x)},100)</script></main>`))
		}
	}))
	defer server.Close()

	installer := chromium.NewInstaller(filepath.Join(root, "artifacts"), "", nil)
	installer.BinaryPath = strings.TrimSpace(os.Getenv("AO_BROWSER_BINARY"))
	manager := NewManager(installer, filepath.Join(root, "state"), Config{Enabled: true}, ManagerOptions{FileStateKey: true})
	manager.state = newTestStateStore(filepath.Join(root, "state"), bytes.Repeat([]byte{7}, 32))
	t.Cleanup(func() { _ = manager.Close() })
	access := Access{ThreadID: "parity", Workspace: root, ProjectRoot: root}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	opened, err := manager.Open(ctx, access, server.URL, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mcp := NewMCPServer(manager, true)
	t.Cleanup(func() { _ = mcp.Close() })
	registered, err := mcp.RegisterThread(access)
	if err != nil {
		t.Fatal(err)
	}
	mcpURL := registered[ServerName].(map[string]any)["url"].(string)
	listed := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if tools := listed["result"].(map[string]any)["tools"].([]any); len(tools) != 27 {
		t.Fatalf("real MCP tools=%d", len(tools))
	}
	backgroundCall := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": "background", "method": "tools/call", "params": map[string]any{"name": "browser_open", "arguments": map[string]any{"url": server.URL + "/next"}}})
	backgroundResult := backgroundCall["result"].(map[string]any)
	if backgroundResult["isError"] == true {
		t.Fatalf("real MCP background open=%#v", backgroundCall)
	}
	backgroundText := backgroundResult["content"].([]any)[0].(map[string]any)["text"].(string)
	var background PageInfo
	if err := json.Unmarshal([]byte(backgroundText), &background); err != nil || background.ID == "" || background.ID == opened.ID {
		t.Fatalf("real MCP background page=%#v err=%v", background, err)
	}
	labelCall := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": "label", "method": "tools/call", "params": map[string]any{"name": "browser_label_page", "arguments": map[string]any{"page_id": background.ID, "label": "parallel-worker"}}})
	if labelCall["result"].(map[string]any)["isError"] == true {
		t.Fatalf("real MCP page label=%#v", labelCall)
	}
	ambiguousCall := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": "ambiguous", "method": "tools/call", "params": map[string]any{"name": "browser_snapshot", "arguments": map[string]any{}}})
	ambiguousResult := ambiguousCall["result"].(map[string]any)
	if ambiguousResult["isError"] != true || !strings.Contains(ambiguousResult["content"].([]any)[0].(map[string]any)["text"].(string), "page_id is required") {
		t.Fatalf("real MCP ambiguous call=%#v", ambiguousCall)
	}
	if state := manager.CompanionState(access); state.Visible == nil || *state.Visible || state.ActivePageID != opened.ID {
		t.Fatalf("real MCP background page stole presentation: %#v", state)
	}
	closeCall := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": "close-background", "method": "tools/call", "params": map[string]any{"name": "browser_close_page", "arguments": map[string]any{"page_id": background.ID}}})
	if closeCall["result"].(map[string]any)["isError"] == true {
		t.Fatalf("real MCP background close=%#v", closeCall)
	}
	called := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "browser_locator", "arguments": map[string]any{"page_id": opened.ID, "locator": map[string]any{"role": "button", "name": "Choose", "exact": true}, "action": "count"}}})
	callResult := called["result"].(map[string]any)
	if callResult["isError"] == true {
		t.Fatalf("real MCP locator=%#v", called)
	}
	textResult := callResult["content"].([]any)[0].(map[string]any)["text"].(string)
	var routed LocatorResult
	if err := json.Unmarshal([]byte(textResult), &routed); err != nil || routed.Count != 2 {
		t.Fatalf("real MCP locator result=%#v err=%v", routed, err)
	}
	evaluated := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "browser_evaluate_readonly", "arguments": map[string]any{"page_id": opened.ID, "expression": "arg => arg.prefix + document.title", "argument": map[string]any{"prefix": "title: "}, "timeout_ms": 5000}}})
	evalResult := evaluated["result"].(map[string]any)
	if evalResult["isError"] == true {
		t.Fatalf("real MCP evaluate=%#v", evaluated)
	}
	evalText := evalResult["content"].([]any)[0].(map[string]any)["text"].(string)
	if evalText != `"title: Parity fixture"` {
		t.Fatalf("real MCP evaluate text=%s", evalText)
	}
	zeroArg := postRPC(t, mcpURL, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "browser_evaluate_readonly", "arguments": map[string]any{"page_id": opened.ID, "expression": "() => ({title: document.title})", "timeout_ms": 5000}}})
	zeroArgResult := zeroArg["result"].(map[string]any)
	if zeroArgResult["isError"] == true {
		t.Fatalf("real MCP zero-argument evaluate=%#v", zeroArg)
	}
	if got := zeroArgResult["content"].([]any)[0].(map[string]any)["text"].(string); got != `{"title":"Parity fixture"}` {
		t.Fatalf("real MCP zero-argument evaluate text=%s", got)
	}

	if session, err := manager.NameSession(ctx, access, "🔎 parity"); err != nil || session.Name != "🔎 parity" {
		t.Fatalf("session=%#v err=%v", session, err)
	}
	if state := manager.CompanionState(access); state.SessionName != "🔎 parity" {
		t.Fatalf("companion session name=%q", state.SessionName)
	}
	if viewport, err := manager.Viewport(ctx, access, ViewportOptions{Action: "set", Width: 900, Height: 700}); err != nil || !viewport.ViewportSet {
		t.Fatalf("viewport=%#v err=%v", viewport, err)
	}
	innerWidth, err := manager.EvaluateReadOnly(ctx, access, opened.ID, `window.innerWidth`)
	if err != nil || innerWidth != float64(900) {
		t.Fatalf("viewport width=%#v err=%v", innerWidth, err)
	}
	managed, _, err := manager.lookupOwnedPage(access, opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	managed.streamMu.Lock()
	unexpectedStream := managed.stream != nil
	managed.streamMu.Unlock()
	if unexpectedStream {
		t.Fatal("viewport override started a screencast without a mounted companion")
	}
	hidden := false
	if _, err := manager.Visibility(ctx, access, &hidden, ""); err != nil {
		t.Fatal(err)
	}
	shown := true
	if _, err := manager.Visibility(ctx, access, &shown, opened.ID); err != nil {
		t.Fatal(err)
	}

	count, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{Role: "button", Name: "Choose", Exact: true}, Action: "count"})
	if err != nil || count.Count != 2 {
		t.Fatalf("role count=%#v err=%v", count, err)
	}
	scopedCount, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{Role: "button", Scope: &Locator{CSS: ".special"}}, Action: "count"})
	if err != nil || scopedCount.Count != 1 {
		t.Fatalf("scope count=%#v err=%v", scopedCount, err)
	}
	andCount, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: ".card", And: []*Locator{{CSS: ".special"}}}, Action: "count"})
	if err != nil || andCount.Count != 1 {
		t.Fatalf("and count=%#v err=%v", andCount, err)
	}
	orCount, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#double", Or: []*Locator{{CSS: "#pointer"}}}, Action: "count"})
	if err != nil || orCount.Count != 2 {
		t.Fatalf("or count=%#v err=%v", orCount, err)
	}
	regexCount, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{Text: `second\s+card`, Regex: true, RegexFlags: "i"}, Action: "count"})
	if err != nil || regexCount.Count != 1 {
		t.Fatalf("regex count=%#v err=%v", regexCount, err)
	}
	testIDCount, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{TestID: "main"}, Action: "count"})
	if err != nil || testIDCount.Count != 1 {
		t.Fatalf("test id count=%#v err=%v", testIDCount, err)
	}
	allCardText, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: ".card"}, Action: "all_text_contents"})
	if err != nil || len(allCardText.Values) != 2 {
		t.Fatalf("all text=%#v err=%v", allCardText, err)
	}
	index := 0
	special, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: ".card", HasText: "Second", Has: &Locator{Role: "button", Name: "Choose"}, Index: &index}, Action: "inner_text"})
	if err != nil || !strings.Contains(special.Value.(string), "Second card") {
		t.Fatalf("scoped locator=%#v err=%v", special, err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{Label: "Account name", Exact: true}, Action: "fill", Value: "Ada"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{Placeholder: "Your name", Exact: true}, Action: "type", Value: " Lovelace"}); err != nil {
		t.Fatal(err)
	}
	value, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#name"}, Action: "get_attribute", Attribute: "id"})
	if err != nil || value.Value != "name" {
		t.Fatalf("attribute=%#v err=%v", value, err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#check"}, Action: "check"}); err != nil {
		t.Fatal(err)
	}
	checked, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#check"}, Action: "all"})
	if err != nil || checked.Matches[0].Checked == nil || !*checked.Matches[0].Checked {
		t.Fatalf("checked=%#v err=%v", checked, err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#check"}, Action: "uncheck"}); err != nil {
		t.Fatal(err)
	}
	wantChecked := true
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#check"}, Action: "set_checked", Checked: &wantChecked}); err != nil {
		t.Fatal(err)
	}
	b := "b"
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#choice"}, Action: "select_option", Select: []SelectArg{{Value: &b}}}); err != nil {
		t.Fatal(err)
	}
	selected, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#choice"}, Action: "all"})
	if err != nil || selected.Matches[0].Value != "b" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	alpha := "Alpha"
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#choice"}, Action: "select_option", Select: []SelectArg{{Label: &alpha}}}); err != nil {
		t.Fatal(err)
	}
	optionIndex := 1
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#choice"}, Action: "select_option", Select: []SelectArg{{Index: &optionIndex}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#double"}, Action: "double_click"}); err != nil {
		t.Fatal(err)
	}
	done, err := manager.EvaluateReadOnly(ctx, access, opened.ID, `document.querySelector('#double').dataset.done`)
	if err != nil || done != "yes" {
		t.Fatalf("double=%#v err=%v", done, err)
	}
	asyncTitle, err := manager.EvaluateReadOnly(ctx, access, opened.ID, `Promise.resolve(document.title)`)
	if err != nil || asyncTitle != "Parity fixture" {
		t.Fatalf("async read-only=%#v err=%v", asyncTitle, err)
	}
	if _, err := manager.EvaluateReadOnly(ctx, access, opened.ID, `document.body.dataset.mutated='yes'`); err == nil {
		t.Fatal("read-only evaluation allowed mutation")
	}

	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#inside", Frames: []string{"#same"}}, Action: "fill", Value: "inside"}); err != nil {
		t.Fatalf("same-origin frame: %v", err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#cross", Frames: []string{"#cross-frame"}}, Action: "click"}); err != nil {
		t.Fatalf("cross-origin frame: %v", err)
	}
	crossText, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#cross", Frames: []string{"#cross-frame"}}, Action: "inner_text"})
	if err != nil || crossText.Value != "Cross frame" {
		t.Fatalf("cross frame read=%#v err=%v", crossText, err)
	}

	if _, err := manager.WaitAdvanced(ctx, access, WaitOptions{PageID: opened.ID, Locator: &Locator{CSS: "#later"}, State: "visible", TimeoutMS: 5000}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Evaluate(ctx, access, opened.ID, `(()=>{const x=document.querySelector('#later');setTimeout(()=>x.style.display='none',25);setTimeout(()=>x.remove(),75);return true})()`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WaitAdvanced(ctx, access, WaitOptions{PageID: opened.ID, Locator: &Locator{CSS: "#later"}, State: "hidden", TimeoutMS: 5000}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WaitAdvanced(ctx, access, WaitOptions{PageID: opened.ID, Locator: &Locator{CSS: "#later"}, State: "detached", TimeoutMS: 5000}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Evaluate(ctx, access, opened.ID, `void fetch('/held'); true`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-heldStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	idleResult := make(chan error, 1)
	go func() {
		_, waitErr := manager.WaitAdvanced(ctx, access, WaitOptions{PageID: opened.ID, LoadState: "networkidle", TimeoutMS: 5000})
		idleResult <- waitErr
	}()
	select {
	case err := <-idleResult:
		t.Fatalf("networkidle returned with a request in flight: %v", err)
	default:
	}
	releaseHeld <- struct{}{}
	if err := <-idleResult; err != nil {
		t.Fatalf("networkidle after release: %v", err)
	}
	binaryItem := ClipboardItem{PresentationStyle: "attachment", Entries: []ClipboardEntry{{MIMEType: "application/octet-stream", Base64: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})}}}
	if _, err := manager.Clipboard(ctx, access, ClipboardOptions{PageID: opened.ID, Action: "write", Items: []ClipboardItem{binaryItem}}); err != nil {
		t.Fatal(err)
	}
	readItems, err := manager.Clipboard(ctx, access, ClipboardOptions{PageID: opened.ID, Action: "read"})
	if err != nil || len(readItems.([]ClipboardItem)) != 1 || readItems.([]ClipboardItem)[0].Entries[0].Base64 != binaryItem.Entries[0].Base64 {
		t.Fatalf("clipboard items=%#v err=%v", readItems, err)
	}
	if _, err := manager.Clipboard(ctx, access, ClipboardOptions{PageID: opened.ID, Action: "write_text", Text: "clipboard text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#name"}, Action: "click"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Press(ctx, access, opened.ID, "Control+V"); err != nil {
		t.Fatal(err)
	}
	pasted, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#name"}, Action: "all"})
	if err != nil || !strings.Contains(pasted.Matches[0].Value, "clipboard text") {
		t.Fatalf("pasted=%#v err=%v", pasted, err)
	}
	clip, err := manager.Clipboard(ctx, access, ClipboardOptions{PageID: opened.ID, Action: "read_text"})
	if err != nil || clip != "clipboard text" {
		t.Fatalf("clipboard=%#v err=%v", clip, err)
	}

	var rect struct{ X, Y float64 }
	// Decode through the typed result to avoid depending on JS number concrete types.
	raw, _ := manager.Evaluate(ctx, access, opened.ID, `(()=>{const r=document.querySelector('#pointer').getBoundingClientRect();return {x:r.x+r.width/2,y:r.y+r.height/2}})()`)
	coords := raw.(map[string]any)
	rect.X = coords["x"].(float64)
	rect.Y = coords["y"].(float64)
	if _, err := manager.Pointer(ctx, access, PointerOptions{PageID: opened.ID, Action: "click", X: rect.X, Y: rect.Y}); err != nil {
		t.Fatal(err)
	}
	pointerClicked, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#pointer"}, Action: "get_attribute", Attribute: "data-clicked"})
	if err != nil || pointerClicked.Value != "yes" {
		t.Fatalf("pointer click=%#v err=%v", pointerClicked, err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#pointer"}, Action: "click", Button: "right"}); err != nil {
		t.Fatal(err)
	}
	contextClicked, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#pointer"}, Action: "get_attribute", Attribute: "data-context"})
	if err != nil || contextClicked.Value != "yes" {
		t.Fatalf("right click=%#v err=%v", contextClicked, err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#pointer"}, Action: "click", Modifiers: []string{"Shift"}}); err != nil {
		t.Fatal(err)
	}
	shiftClicked, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#pointer"}, Action: "get_attribute", Attribute: "data-shift"})
	if err != nil || shiftClicked.Value != "true" {
		t.Fatalf("shift click=%#v err=%v", shiftClicked, err)
	}
	dragRaw, _ := manager.Evaluate(ctx, access, opened.ID, `(()=>{const a=document.querySelector('#drag').getBoundingClientRect(),b=document.querySelector('#drop').getBoundingClientRect();return {ax:a.x+a.width/2,ay:a.y+a.height/2,bx:b.x+b.width/2,by:b.y+b.height/2}})()`)
	dragCoords := dragRaw.(map[string]any)
	start := Point{X: dragCoords["ax"].(float64), Y: dragCoords["ay"].(float64)}
	end := Point{X: dragCoords["bx"].(float64), Y: dragCoords["by"].(float64)}
	if _, err := manager.Pointer(ctx, access, PointerOptions{PageID: opened.ID, Action: "drag", Path: []Point{start, {X: (start.X + end.X) / 2, Y: (start.Y + end.Y) / 2}, end}}); err != nil {
		t.Fatal(err)
	}
	dropped, err := manager.EvaluateReadOnly(ctx, access, opened.ID, `document.querySelector('#drop').dataset.dropped`)
	if err != nil || dropped != "yes" {
		t.Fatalf("drag result=%#v err=%v", dropped, err)
	}

	snapshot, err := manager.Snapshot(ctx, access, opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pointerNode string
	for _, element := range snapshot.Elements {
		if element.Selector == "#pointer" {
			pointerNode = element.NodeID
		}
	}
	if pointerNode == "" {
		t.Fatal("snapshot missing pointer node")
	}
	if strings.HasPrefix(pointerNode, "css:") {
		t.Fatalf("snapshot exposed forgeable selector node id %q", pointerNode)
	}
	if _, err := manager.DOMAction(ctx, access, DOMActionOptions{PageID: opened.ID, Action: "click", NodeID: pointerNode}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DOMAction(ctx, access, DOMActionOptions{PageID: opened.ID, Action: "scroll", X: 0, Y: 1}); err != nil {
		t.Fatalf("page DOM scroll: %v", err)
	}
	if _, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#name"}, Action: "click"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DOMAction(ctx, access, DOMActionOptions{PageID: opened.ID, Action: "keypress", Keys: []string{"ControlOrMeta", "a"}}); err != nil {
		t.Fatalf("DOM key chord: %v", err)
	}

	logs, err := manager.ConsoleLogs(ctx, access, ConsoleOptions{PageID: opened.ID, Levels: []string{"warn"}, Filter: "fixture", Limit: 10})
	if err != nil || len(logs) == 0 {
		t.Fatalf("logs=%#v err=%v", logs, err)
	}
	shot, err := manager.Screenshot(ctx, access, ScreenshotOptions{PageID: opened.ID, Clip: &ClipRect{X: 0, Y: 0, Width: 200, Height: 100}})
	if err != nil || len(shot) < 4 {
		t.Fatalf("clip screenshot=%d err=%v", len(shot), err)
	}

	assets, err := manager.Assets(ctx, access, AssetOptions{PageID: opened.ID, Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	inventory := assets.(AssetInventory)
	if len(inventory.Assets) == 0 || len(inventory.InlineSVGs) == 0 {
		t.Fatalf("inventory=%#v", inventory)
	}
	var assetSources []AssetSource
	for _, asset := range inventory.Assets {
		if strings.HasSuffix(asset.URL, "/asset.png") {
			assetSources = asset.Sources
			break
		}
	}
	sourceKinds := map[string]bool{}
	for _, source := range assetSources {
		sourceKinds[source.Kind] = true
		if source.Kind != "resource" && source.NodeID == "" {
			t.Fatalf("DOM asset source has no opaque node ID: %#v", source)
		}
	}
	if !sourceKinds["attribute"] || !sourceKinds["computedStyle"] {
		t.Fatalf("asset sources=%#v", assetSources)
	}
	if encoded, marshalErr := json.Marshal(inventory); marshalErr != nil || bytes.Contains(encoded, []byte(`"selector"`)) {
		t.Fatalf("inventory leaked internal selectors: %s err=%v", encoded, marshalErr)
	}
	bundleAny, err := manager.Assets(ctx, access, AssetOptions{PageID: opened.ID, Action: "bundle", InventoryID: inventory.ID, Kinds: []string{"image"}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := bundleAny.(AssetBundle)
	if len(bundle.Assets) == 0 || bundle.ManifestPath == "" || bundle.Assets[0].ContentType != "image/png" {
		t.Fatalf("bundle=%#v", bundle)
	}
	bundledPNG, err := os.ReadFile(bundle.Assets[0].Path)
	if err != nil || !bytes.Equal(bundledPNG, png) {
		t.Fatalf("bundled asset bytes=%d err=%v", len(bundledPNG), err)
	}

	downloadResult, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#download"}, Action: "click", ExpectDownload: true, TimeoutMS: 10000})
	if err != nil || downloadResult.Download == nil || downloadResult.Download.State != "completed" {
		t.Fatalf("download=%#v err=%v", downloadResult, err)
	}
	if data, readErr := os.ReadFile(downloadResult.Download.Path); readErr != nil || string(data) != "downloaded" {
		t.Fatalf("download file=%q err=%v", data, readErr)
	}

	navigation, err := manager.Locator(ctx, access, LocatorOptions{PageID: opened.ID, Locator: Locator{CSS: "#next"}, Action: "click", ExpectNavigation: true, URL: "**/next", WaitUntil: "load", TimeoutMS: 10000})
	if err != nil || !navigation.Navigated {
		t.Fatalf("navigation=%#v err=%v", navigation, err)
	}
	if _, err := manager.History(ctx, access, opened.ID, "back"); err != nil {
		t.Fatal(err)
	}
	second, err := manager.NewPage(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	if second.URL != "about:blank" {
		t.Fatalf("new page=%#v", second)
	}
	secondClipboard, err := manager.Clipboard(ctx, access, ClipboardOptions{PageID: second.ID, Action: "read_text"})
	if err != nil || secondClipboard != "" {
		t.Fatalf("clipboard leaked between tabs: %#v err=%v", secondClipboard, err)
	}
	second, err = manager.Open(ctx, access, server.URL, OpenOptions{PageID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectPage(ctx, access, opened.ID); err != nil {
		t.Fatal(err)
	}
	pages, err := manager.Pages(ctx, access)
	if err != nil || len(pages) != 2 || !pages[0].Selected || pages[0].ID != opened.ID {
		t.Fatalf("pages=%#v second=%#v err=%v", pages, second, err)
	}
}
