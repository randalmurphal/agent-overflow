//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
	"agent-overflow/internal/wsllauncher"
)

// TestRenderPicker_InjectsGlobals proves the picker page renders the
// JS globals that the inline pick() handler reads:
//
//   - window.__AO_DISTROS__         — JSON array used to render rows.
//   - window.__AO_PICK_DISTRO_FQN__ — the FQN string Wails dispatches
//     by, formatted as "<pkgPath>.<TypeName>.<MethodName>".
//
// If either drifts (renamed type, format change in Wails' Bindings.Add),
// the picker click silently no-ops with a method-not-found error. This
// test catches that at compile/test time.
func TestRenderPicker_InjectsGlobals(t *testing.T) {
	distros := []wsllauncher.Distro{
		{Name: "Ubuntu-24.04", Default: true, Version: 2, State: "Running"},
		{Name: "Debian", Default: false, Version: 1, State: "Stopped"},
	}
	out, err := renderPicker(distros)
	if err != nil {
		t.Fatalf("renderPicker: %v", err)
	}
	body := string(out)

	wantDistros := `window.__AO_DISTROS__ = [{"name":"Ubuntu-24.04","default":true,"version":2,"state":"Running"},{"name":"Debian","default":false,"version":1,"state":"Stopped"}]`
	if !strings.Contains(body, wantDistros) {
		t.Errorf("missing __AO_DISTROS__ injection\nwant substring: %s\ninjection block: %s", wantDistros, injectionExcerpt(body))
	}

	elem := reflect.TypeOf((*launcherApp)(nil)).Elem()
	wantFQN := fmt.Sprintf("%s.%s.PickDistro", elem.PkgPath(), elem.Name())
	fqnJSON, err := json.Marshal(wantFQN)
	if err != nil {
		t.Fatalf("marshal expected FQN: %v", err)
	}
	wantInj := "window.__AO_PICK_DISTRO_FQN__ = " + string(fqnJSON)
	if !strings.Contains(body, wantInj) {
		t.Errorf("missing __AO_PICK_DISTRO_FQN__ injection\nwant substring: %s\ninjection block: %s", wantInj, injectionExcerpt(body))
	}

	// The injection must sit before the inline picker script so the
	// inline script reads populated globals.
	injIdx := strings.Index(body, "window.__AO_DISTROS__ = ")
	consumerIdx := strings.Index(body, "const distros = window.__AO_DISTROS__")
	if injIdx < 0 || consumerIdx < 0 || injIdx >= consumerIdx {
		t.Errorf("injection must precede consumer script (injIdx=%d, consumerIdx=%d)", injIdx, consumerIdx)
	}
}

// TestRenderPicker_EmptyDistros covers the no-WSL-installed render
// path: the distros array becomes [], but the FQN is still injected
// so a hypothetical fallback pick() call wouldn't blank-screen.
func TestRenderPicker_EmptyDistros(t *testing.T) {
	out, err := renderPicker(nil)
	if err != nil {
		t.Fatalf("renderPicker: %v", err)
	}
	body := string(out)

	if !strings.Contains(body, "window.__AO_DISTROS__ = []") {
		t.Errorf("empty distros must render as []; injection block: %s", injectionExcerpt(body))
	}
	if !strings.Contains(body, "window.__AO_PICK_DISTRO_FQN__ = ") {
		t.Errorf("FQN must still be injected when distros are empty; injection block: %s", injectionExcerpt(body))
	}
}

// TestPickerHTML_DoesNotLoadRuntimeJS pins the deliberate choice to
// skip /wails/runtime.js and call Wails' wire endpoint directly. The
// bundled runtime is an ES module (top-level `export { ... }`) — every
// way of pulling it in we tried (plain <script>, <script type="module">)
// failed in this WebView2 launcher: plain script throws a SyntaxError on
// the export, and the module form was either cached stale or somehow not
// applied, leaving window.wails undefined. Reintroducing the script tag
// silently regresses; the test guards that seam.
func TestPickerHTML_DoesNotLoadRuntimeJS(t *testing.T) {
	if strings.Contains(pickerHTML, `src="/wails/runtime.js"`) {
		t.Errorf("picker.html must not include a <script src=\"/wails/runtime.js\"> tag; the picker calls /wails/runtime directly to bypass runtime.js's module-loading footguns in WebView2")
	}
}

// TestPickerHTML_DirectWireCallShape pins the inline pick() handler
// against the wire format defined in
// pkg/application/messageprocessor.go (callRequest=0) and
// messageprocessor_call.go (CallBinding=0). If those constants ever
// change, this test surfaces the mismatch at build time rather than a
// runtime "unknown object N" error from the Wails server.
func TestPickerHTML_DirectWireCallShape(t *testing.T) {
	mustContain := []string{
		`fetch("/wails/runtime"`,
		`object: 0`,
		`method: 0`,
		`"call-id": callId`,
		`methodName: fqn`,
	}
	for _, frag := range mustContain {
		if !strings.Contains(pickerHTML, frag) {
			t.Errorf("picker.html missing wire-call fragment %q; pick() must POST {object:0, method:0, args:{call-id, methodName, args}} to /wails/runtime", frag)
		}
	}
}

func injectionExcerpt(body string) string {
	i := strings.Index(body, "<script>window.__AO_")
	if i < 0 {
		return "(no __AO_ injection script found)"
	}
	end := strings.Index(body[i:], "</script>")
	if end < 0 {
		tail := i + 200
		if tail > len(body) {
			tail = len(body)
		}
		return body[i:tail]
	}
	return body[i : i+end+9]
}

func TestPickerErrorRoutesServeTheCurrentFailure(t *testing.T) {
	page := startupFailureHTML(errLaunchFailed)
	handler := pickerAssetHandler(nil, func() []byte { return page }, func() loadingReport { return loadingReport{} })
	for _, path := range []string{"/startup-error", "/connectivity-error"} {
		page = startupFailureHTML(wsllauncher.BootstrapHTTPError{StatusCode: 404})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if !strings.Contains(response.Body.String(), "HTTP 404") || strings.Contains(response.Body.String(), "localhostForwarding") {
			t.Fatalf("%s served unrelated guidance: %s", path, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("failure page can be cached")
		}
	}
}

// TestLoadingReportFollowsTheLaunch: before a launch the report has no
// clock; a launch starts the clock; the backend's reports supply the
// status, step and the update being finished; a new launch forgets the
// previous backend's report.
func TestLoadingReportFollowsTheLaunch(t *testing.T) {
	var status loadingStatus
	t0 := time.Unix(1_700_000_000, 0)
	if got := status.report(t0); got != (loadingReport{Title: "Starting Agent Overflow"}) {
		t.Fatalf("report before a launch = %+v", got)
	}

	status.begin(t0)
	if got := status.report(t0.Add(1500 * time.Millisecond)); got.ElapsedMs != 1500 || got.Phase != "" {
		t.Fatalf("report while WSL boots = %+v", got)
	}

	status.setProgress(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 3 of 7 v101", Step: 3, Steps: 7, UpdatingTo: "1.2.3"})
	want := loadingReport{
		Title: "Updating Agent Overflow", Status: "Finishing update to v1.2.3: applying migration 3 of 7 v101",
		Phase: "store.migrate", Step: 3, Steps: 7, ElapsedMs: 12_000,
	}
	if got := status.report(t0.Add(12 * time.Second)); got != want {
		t.Fatalf("report = %+v, want %+v", got, want)
	}

	status.begin(t0.Add(time.Minute))
	if got := status.report(t0.Add(time.Minute)); got.Phase != "" || got.Title != "Starting Agent Overflow" || got.ElapsedMs != 0 {
		t.Fatalf("a new launch kept the old report: %+v", got)
	}
}

// TestLoadingRoutesServeTheLiveReport: /loading polls /loading.json through
// /loading.js, so the page updates without a reload, and none of it is
// cacheable.
func TestLoadingRoutesServeTheLiveReport(t *testing.T) {
	report := loadingReport{Title: "Starting Agent Overflow", Status: "Applying migration 1 of 2 a", Phase: "store.migrate", Step: 1, Steps: 2, ElapsedMs: 42}
	handler := pickerAssetHandler(nil, func() []byte { return nil }, func() loadingReport { return report })
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s can be cached", path)
		}
		return rec
	}

	page := get("/loading").Body.String()
	for _, want := range []string{`<script src="/loading.js">`, `id="ao-loading-title"`, `id="ao-loading-status"`, `id="ao-loading-meta"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("/loading lacks %s", want)
		}
	}
	if !strings.Contains(pickerHTML, `<script src="/loading.js">`) || !strings.Contains(pickerHTML, `id="ao-loading-status"`) {
		t.Fatal("the picker's post-pick loading line does not follow the launch report")
	}

	script := get("/loading.js")
	if ct := script.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("/loading.js content type = %q", ct)
	}
	for _, want := range []string{`fetch("/loading.json"`, "setTimeout(poll, 500)", "r.status", "r.steps", "r.elapsedMs"} {
		if !strings.Contains(script.Body.String(), want) {
			t.Fatalf("/loading.js lacks %s", want)
		}
	}

	rec := get("/loading.json")
	var got loadingReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got != report {
		t.Fatalf("/loading.json = %q (%v), want %+v", rec.Body.String(), err, report)
	}
	report.Step = 2
	if err := json.Unmarshal(get("/loading.json").Body.Bytes(), &got); err != nil || got.Step != 2 {
		t.Fatalf("/loading.json did not follow the report: %+v %v", got, err)
	}
}
