//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

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
