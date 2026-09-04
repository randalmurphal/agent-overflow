package browser

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"strings"
	"testing"
)

// The WebKit driver's JavaScript is where a tool silently changes meaning with
// the engine, so every builder is asserted on the property that matters rather
// than on its exact text.

func TestWebKitExpressionBodyAwaitsTheValue(t *testing.T) {
	if got := webkitExpressionBody("document.title"); got != "return (document.title);" {
		t.Fatalf("expression body = %q", got)
	}
}

func TestWebKitClickPlainUsesActivationBehaviour(t *testing.T) {
	plain := webkitClickFunction(1, "", nil)
	if !strings.Contains(plain, "this.click()") || strings.Contains(plain, "MouseEvent") {
		t.Fatalf("plain click should be element.click(): %s", plain)
	}
	if !strings.Contains(plain, "scrollIntoView") {
		t.Fatalf("click must scroll into view first: %s", plain)
	}
}

func TestWebKitClickCarriesButtonModifiersAndCount(t *testing.T) {
	script := webkitClickFunction(2, "right", []string{"shift", "controlormeta"})
	for _, want := range []string{`"button":2`, `"shiftKey":true`, `"ctrlKey":true`, "dblclick"} {
		if !strings.Contains(script, want) {
			t.Fatalf("click missing %s: %s", want, script)
		}
	}
	if strings.Contains(script, `"metaKey":true`) {
		t.Fatalf("controlormeta must resolve to control on this engine: %s", script)
	}
}

func TestWebKitTypeClearsThroughTheValueSetter(t *testing.T) {
	// A framework-controlled input ignores a raw `.value =`, so both the clear
	// and the append have to go through the prototype setter.
	script := webkitTypeFunction("hi", true)
	if strings.Count(script, "getOwnPropertyDescriptor") != 2 {
		t.Fatalf("clear and append must both use the value setter: %s", script)
	}
	if !strings.Contains(script, `"input"`) || !strings.Contains(script, `"change"`) {
		t.Fatalf("type must dispatch input and change: %s", script)
	}
}

func TestWebKitPressEnterSubmitsAndInsertsPrintableKeys(t *testing.T) {
	script := webkitPressFunction("Enter")
	if !strings.Contains(script, "requestSubmit") {
		t.Fatalf("Enter must submit an owning form: %s", script)
	}
	if !strings.Contains(webkitPressFunction("Control+a"), `"ctrlKey":true`) {
		t.Fatal("chord modifiers must reach the event init")
	}
}

func TestWebKitNodeActRejectsWhatTheManagerCannotFixUp(t *testing.T) {
	if _, err := webkitNodeActScript(nil, "#a", nodeAction{Kind: "press"}); err == nil {
		t.Fatal("press without a key must fail")
	}
	if _, err := webkitNodeActScript(nil, "#a", nodeAction{Kind: "teleport"}); err == nil {
		t.Fatal("unknown action must fail")
	}
	script, err := webkitNodeActScript([]string{"iframe#inner"}, "#a", nodeAction{Kind: "fill", Value: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "aoElement") || !strings.Contains(script, "iframe#inner") {
		t.Fatalf("fill must resolve through the frame chain: %s", script)
	}
}

func TestWebKitElementCallEncodesSelectorsAsData(t *testing.T) {
	// A selector is untrusted text: it must cross as a JSON string, never as
	// source spliced into the script.
	selector := `a[href="\"];alert(1);//"]`
	script := webkitElementCallScript(nil, selector, `function(){return 1}`)
	if !strings.Contains(script, jsonString(selector)) {
		t.Fatalf("selector must cross as a JSON string: %s", script)
	}
	if strings.Contains(script, selector) {
		t.Fatalf("selector was spliced into source verbatim: %s", script)
	}
	// An unframed call must still hand the walk an array: `null` is not
	// iterable, and the frame loop would throw before reaching the selector.
	if !strings.Contains(script, "aoElement([],") {
		t.Fatalf("unframed call must pass an empty frame list: %s", script)
	}
}

func TestWebKitPointerValidatesItsArguments(t *testing.T) {
	if _, err := webkitPointerScript(PointerOptions{Action: "click", Button: "wheel"}); err == nil {
		t.Fatal("unknown button must fail")
	}
	if _, err := webkitPointerScript(PointerOptions{Action: "click", Modifiers: []string{"hyper"}}); err == nil {
		t.Fatal("unknown modifier must fail")
	}
	if _, err := webkitPointerScript(PointerOptions{Action: "orbit"}); err == nil {
		t.Fatal("unknown action must fail")
	}
	if _, err := webkitPointerScript(PointerOptions{Action: "drag", Path: []Point{{X: 1, Y: 1}}}); err == nil {
		t.Fatal("a one-point drag must fail")
	}
	drag, err := webkitPointerScript(PointerOptions{Action: "drag", Path: []Point{{X: 1, Y: 2}, {X: 3, Y: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dragstart", "dragover", "drop", "dragend"} {
		if !strings.Contains(drag, want) {
			t.Fatalf("drag missing %s", want)
		}
	}
}

// Chrome's Input domain gives a right-click its DOM consequences; the
// untrusted sequence has to spell them out, and a site's custom menu listens
// for exactly one of them.
func TestWebKitPointerSpellsOutButtonSemantics(t *testing.T) {
	right, err := webkitPointerScript(PointerOptions{Action: "click", Button: "right", X: 10, Y: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"contextmenu"`, `"auxclick"`, `"button":2`, `"buttons":2`} {
		if !strings.Contains(right, want) {
			t.Fatalf("right click missing %s: %s", want, right)
		}
	}
	middle, err := webkitPointerScript(PointerOptions{Action: "click", Button: "middle", X: 10, Y: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(middle, `"buttons":4`) {
		t.Fatalf("middle click must set the auxiliary buttons bit: %s", middle)
	}
	left, err := webkitPointerScript(PointerOptions{Action: "double_click", X: 10, Y: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(left, `"buttons":1`) || !strings.Contains(left, `"dblclick"`) {
		t.Fatalf("primary double click: %s", left)
	}
}

// A statement list is what CDP's Runtime.evaluate accepts and `return (...)`
// cannot parse, so the expression body is tried first and the eval body only
// on a parse failure. A page exception that is not a parse failure is the
// answer, never a retry.
func TestWebKitEvaluateFallsBackToStatementsOnlyOnSyntaxErrors(t *testing.T) {
	var bodies []string
	eval := func(_ context.Context, body string) (json.RawMessage, error) {
		bodies = append(bodies, body)
		if strings.HasPrefix(body, "return (") {
			return nil, errors.New("SyntaxError: Unexpected token ';'")
		}
		return json.RawMessage("4"), nil
	}
	raw, err := webkitEvaluate(context.Background(), eval, "const n = 1 + 1; n * 2")
	if err != nil || string(raw) != "4" {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	if len(bodies) != 2 || bodies[0] != "return (const n = 1 + 1; n * 2);" || bodies[1] != `return eval("const n = 1 + 1; n * 2");` {
		t.Fatalf("bodies=%q", bodies)
	}

	bodies = nil
	pageError := errors.New("TypeError: x is not a function")
	_, err = webkitEvaluate(context.Background(), func(context.Context, string) (json.RawMessage, error) {
		bodies = append(bodies, "")
		return nil, pageError
	}, "x()")
	if err != pageError || len(bodies) != 1 {
		t.Fatalf("a page exception must not be retried: err=%v calls=%d", err, len(bodies))
	}
}

func TestWebKitKeyChordVocabularyMatchesTheCDPDriver(t *testing.T) {
	key, modifiers := webkitKeyChord("ControlOrMeta+Shift+ArrowLeft")
	if key != "ArrowLeft" || !modifiers["control"] || !modifiers["shift"] {
		t.Fatalf("chord = %q %#v", key, modifiers)
	}
	if key, _ := webkitKeyChord("esc"); key != "Escape" {
		t.Fatalf("esc = %q", key)
	}
}

func TestWebKitConsoleScriptPostsToItsOwnHandler(t *testing.T) {
	script := webkitConsoleCaptureScript(webkitConsoleHandler)
	if !strings.Contains(script, "messageHandlers?."+webkitConsoleHandler) {
		t.Fatalf("console script must post to the registered handler: %s", script)
	}
	for _, level := range []string{"warn", "error", "debug"} {
		if !strings.Contains(script, `"`+level+`"`) {
			t.Fatalf("console script does not capture %s", level)
		}
	}
	if !strings.Contains(script, "unhandledrejection") {
		t.Fatal("console script must capture unhandled rejections")
	}
}

func TestWebKitLocatorResolveWalksFramesThenResolves(t *testing.T) {
	script := webkitLocatorResolveScript(Locator{Frames: []string{"iframe"}, Role: "button"}, "")
	if !strings.Contains(script, "aoRoot([\"iframe\"])") {
		t.Fatalf("resolver must start from the frame chain: %s", script)
	}
}

func TestWebKitAssetFetchCarriesCredentialsAndItsCap(t *testing.T) {
	script := webkitAssetFetchScript("https://example.test/a.png", 1234)
	if !strings.Contains(script, `credentials:"include"`) {
		t.Fatalf("asset fetch must use the page's credentials: %s", script)
	}
	if !strings.Contains(script, "1234") {
		t.Fatalf("asset fetch must enforce the cap in the page: %s", script)
	}
}

func TestWebKitNavigationMarkStandsInForALoaderID(t *testing.T) {
	// WebKit has no loader id. history.length is the stand-in, and it must be
	// carried as a string so the mark decodes into navigationMark.
	var mark navigationMark
	if err := json.Unmarshal([]byte(`{"url":"about:blank","loader":"3"}`), &mark); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(webkitNavigationMarkScript, "history.length") {
		t.Fatal("navigation mark must sample history.length")
	}
}

func TestWebKitDecodeSnapshotSwapsBGRAInPlace(t *testing.T) {
	pixels := []byte{1, 2, 3, 255, 4, 5, 6, 255}
	frame, err := webkitDecodeSnapshot(pixels, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := frame.RGBAAt(0, 0); got.R != 3 || got.G != 2 || got.B != 1 || got.A != 255 {
		t.Fatalf("pixel 0 = %#v", got)
	}
	if got := frame.RGBAAt(1, 0); got.R != 6 || got.B != 4 {
		t.Fatalf("pixel 1 = %#v", got)
	}
	if &frame.Pix[0] != &pixels[0] {
		t.Fatal("decode must reuse the snapshot buffer")
	}
}

func TestWebKitDecodeSnapshotRefusesAShortBuffer(t *testing.T) {
	if _, err := webkitDecodeSnapshot([]byte{1, 2, 3, 4}, 2, 1); err == nil {
		t.Fatal("a truncated snapshot must be an error, not a half image")
	}
	if _, err := webkitDecodeSnapshot(nil, 0, 0); err == nil {
		t.Fatal("an empty snapshot must be an error")
	}
}

func TestWebKitCropClampsAndKeepsTheFrameWhenOutside(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 10, 10))
	if got := webkitCrop(frame, 2, 2, 20, 20).Rect; got != image.Rect(2, 2, 10, 10) {
		t.Fatalf("clamped crop = %v", got)
	}
	if got := webkitCrop(frame, 50, 50, 4, 4).Rect; got != frame.Rect {
		t.Fatalf("outside crop should keep the frame, got %v", got)
	}
}
