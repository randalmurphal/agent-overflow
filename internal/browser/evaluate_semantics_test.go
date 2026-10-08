package browser

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// evaluateSemanticsPage carries the script policy AO's own UI ships:
// `script-src 'self'`, with no 'unsafe-eval'. A tool that reaches for the
// page's eval fails here, which is the page agents most often drive.
const evaluateSemanticsPage = `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="script-src 'self'"><title>evaluate</title></head><body><p id="x" class="a b">hi</p></body></html>`

// evaluateEngine names the engine family under test where the evaluate
// contract lets them answer differently.
type evaluateEngine string

const (
	evaluateEngineCDP    evaluateEngine = "cdp"
	evaluateEngineWebKit evaluateEngine = "webkit"
)

// evaluator runs code with an optional JSON argument and returns the decoded
// result and the note the tool result carries.
type evaluator func(code string, argument json.RawMessage) (any, string, error)

// assertEvaluateSemantics drives browser_evaluate and browser_evaluate_readonly
// against a real engine's page loaded from evaluateSemanticsPage. The engine
// reads the code and the page encodes the result, so only a real engine can
// hold them to the contract; the real-engine gates call it.
func assertEvaluateSemantics(t *testing.T, engine evaluateEngine, evaluate, readOnly evaluator) {
	t.Helper()
	call := func(t *testing.T, run evaluator, code string, argument ...string) (any, string, error) {
		t.Helper()
		var raw json.RawMessage
		if len(argument) > 0 {
			raw = json.RawMessage(argument[0])
		}
		return run(code, raw)
	}
	expectFrom := func(t *testing.T, run evaluator, code string, want any, argument ...string) string {
		t.Helper()
		got, note, err := call(t, run, code, argument...)
		if err != nil {
			t.Fatalf("evaluate %q: %v", code, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("evaluate %q = %#.300v, want %#.300v", code, got, want)
		}
		return note
	}
	failsFrom := func(t *testing.T, run evaluator, code string, wantParts ...string) error {
		t.Helper()
		got, _, err := run(code, nil)
		if err == nil {
			t.Fatalf("evaluate %q = %#v, want an error", code, got)
		}
		for _, part := range wantParts {
			if !strings.Contains(err.Error(), part) {
				t.Fatalf("evaluate %q error %q does not mention %q", code, err, part)
			}
		}
		return err
	}
	expect := func(t *testing.T, code string, want any, argument ...string) {
		t.Helper()
		if note := expectFrom(t, evaluate, code, want, argument...); note != "" {
			t.Fatalf("evaluate %q carried the note %q", code, note)
		}
	}
	fails := func(t *testing.T, code string, wantParts ...string) error {
		t.Helper()
		return failsFrom(t, evaluate, code, wantParts...)
	}
	element := `<p id="x" class="a b">hi</p>`
	// indescribable throws a SyntaxError from the code that describes it.
	// Escaping the runner, that would read as code that does not compile,
	// and an expression would run again as statements.
	indescribable := `{toJSON() { throw 0; }, get [Symbol.toStringTag]() { throw new SyntaxError("tag"); }}`
	// replaceToString makes reading Object.prototype.toString, which a
	// runner does as it sets itself up, throw a SyntaxError once, putting the
	// built-in back as it throws.
	replaceToString := `window.__aoToString = Object.getOwnPropertyDescriptor(Object.prototype, "toString"); window.__aoSetUp = 0; Object.defineProperty(Object.prototype, "toString", {configurable: true, get() { window.__aoSetUp++; Object.defineProperty(Object.prototype, "toString", window.__aoToString); throw new SyntaxError("replaced"); }}); return 1`

	t.Run("the page forbids eval", func(t *testing.T) {
		// CDP evaluation is exempt from the page's script policy, eval
		// included; WebKit evaluation is not, so on WebKit this proves the
		// cases below run under the policy.
		want := "EvalError"
		if engine == evaluateEngineCDP {
			want = "eval allowed"
		}
		expect(t, `(() => { try { eval("1"); return "eval allowed"; } catch (e) { return e.name; } })()`, want)
	})
	t.Run("expression", func(t *testing.T) {
		expect(t, `document.querySelector("#x").textContent`, "hi")
		expect(t, `document.querySelector("#x").textContent;`, "hi")
		expect(t, "1 + 1 // two", 2.0)
		expect(t, `{a: 1}`, map[string]any{"a": 1.0})
		expect(t, `[1, 2].map((n) => n * 2)`, []any{2.0, 4.0})
		expect(t, `void 0`, nil)
	})
	t.Run("statements", func(t *testing.T) {
		expect(t, `window.__aoEval = [1, 2, 3]; return window.__aoEval.length`, 3.0)
		expect(t, `window.__aoEval.join(",")`, "1,2,3")
		expect(t, `if (true) { return "early"; } return "late"`, "early")
		expect(t, "const a = 2;\nreturn a // two", 2.0)
		for _, code := range []string{`const a = 1; a`, `const a = 1;`} {
			if note := expectFrom(t, evaluate, code, nil); note != evaluateStatementsNote {
				t.Fatalf("evaluate %q note = %q, want the statements note", code, note)
			}
		}
	})
	t.Run("declarations belong to their call", func(t *testing.T) {
		expect(t, `const k = 1; return k + 1`, 2.0)
		expect(t, `const k = 2; return k + 1`, 3.0)
		expect(t, `var aoLocal = 5; function aoDouble(n) { return n * 2; } return aoDouble(aoLocal)`, 10.0)
		expect(t, `[typeof k, typeof aoLocal, typeof aoDouble]`, []any{"undefined", "undefined", "undefined"})
		expect(t, `this === window`, true)
	})
	t.Run("await", func(t *testing.T) {
		expect(t, `await new Promise((resolve) => setTimeout(() => resolve("late"), 10))`, "late")
		expect(t, `const v = await Promise.resolve(4); return v * 2`, 8.0)
		expect(t, `const p = Promise.resolve(7); return p`, 7.0)
		expect(t, `Promise.resolve(9)`, 9.0)
		expect(t, `try { await import("./missing.js"); return "loaded"; } catch (e) { return "failed"; }`, "failed")
	})
	t.Run("a function is called with the argument", func(t *testing.T) {
		expect(t, `() => document.title`, "evaluate")
		expect(t, `function () { return this === window; }`, true)
		expect(t, `function () { "use strict"; return this === window; }`, true)
		// The call reads none of the function's own properties.
		expect(t, `Object.assign(() => 42, {apply: () => 99, call: () => 99})`, 42.0)
		expect(t, `Object.setPrototypeOf(() => 42, null)`, 42.0)
		expect(t, `(selector) => document.querySelector(selector).textContent`, "hi", `"#x"`)
		expect(t, `async (n) => { await null; return n + 1; }`, 2.0, `1`)
		expect(t, `(arg) => arg === null`, true, `null`)
		// The argument keeps its JSON meaning: __proto__ is a key, not the
		// object's prototype.
		expect(t, `(arg) => Object.getPrototypeOf(arg) === Object.prototype && arg.__proto__`, 5.0, `{"__proto__": 5}`)
		expect(t, `(arg) => arg.s`, "</script> <!--", `{"s": "</script> <!--"}`)
	})
	t.Run("an argument needs a function", func(t *testing.T) {
		// A JSON null reads as no argument.
		expect(t, `document.title`, "evaluate", `null`)
		_, _, err := call(t, evaluate, `document.title`, `1`)
		if err == nil || !strings.Contains(err.Error(), "an argument needs code that is one function") {
			t.Fatalf("argument for an expression: %v", err)
		}
		// Statements never run with an argument they cannot take.
		_, _, err = call(t, evaluate, `window.__aoArgued = 1; return 1`, `1`)
		if err == nil || !strings.Contains(err.Error(), "an argument needs code that is one function") {
			t.Fatalf("argument for statements: %v", err)
		}
		expect(t, `typeof window.__aoArgued`, "undefined")
	})
	t.Run("code that throws runs once", func(t *testing.T) {
		// A SyntaxError the code throws while running is the code's answer,
		// not a sign that it should be read another way and run again.
		fails(t, `(window.__aoRuns = (window.__aoRuns || 0) + 1, JSON.parse("{bad"))`, "Uncaught SyntaxError")
		expect(t, `window.__aoRuns`, 1.0)
		fails(t, `window.__aoStatementRuns = (window.__aoStatementRuns || 0) + 1; JSON.parse("{bad")`, "Uncaught SyntaxError")
		expect(t, `window.__aoStatementRuns`, 1.0)
		fails(t, `null.x`, "Uncaught TypeError")
		fails(t, `throw "plain"`, "Uncaught plain")
		fails(t, `throw {code: 1}`, `Uncaught {"code":1}`)
		fails(t, `({get x() { throw new Error("getter"); }})`, "Uncaught Error: getter")
		err := fails(t, `throw new Error("m".repeat(5000))`, "Uncaught Error: mmm")
		if strings.Contains(err.Error(), strings.Repeat("m", 1000)) {
			t.Fatalf("a thrown message reached the tool unbounded: %d bytes", len(err.Error()))
		}
		// What the code threw, and what a getter the encoder reads threw.
		fails(t, `(window.__aoDescribed = (window.__aoDescribed || 0) + 1, (() => { throw `+indescribable+`; })())`,
			evaluateUndescribed)
		expect(t, `window.__aoDescribed`, 1.0)
		fails(t, `(window.__aoEncoded = (window.__aoEncoded || 0) + 1, {get x() { throw `+indescribable+`; }})`,
			evaluateUndescribed)
		expect(t, `window.__aoEncoded`, 1.0)
		// A built-in the runner calls can throw too, once the page replaces
		// it: while the runner reports what the code threw, and while it
		// refuses a result past the size limit.
		expect(t, `window.__aoStringify = JSON.stringify; return 1`, 1.0)
		for counter, result := range map[string]string{
			"__aoThrown":    `(() => { throw "page error"; })()`,
			"__aoOversized": `"x".repeat(` + strconv.Itoa(maxEvaluateBytes+1) + `)`,
		} {
			_, _, err := call(t, evaluate, `(window.`+counter+` = (window.`+counter+` || 0) + 1, JSON.stringify = () => { throw new SyntaxError("replaced"); }, `+result+`)`)
			expect(t, `JSON.stringify = window.__aoStringify; return window.`+counter, 1.0)
			if err == nil || !strings.Contains(err.Error(), evaluateUndescribed) {
				t.Fatalf("with JSON.stringify replaced, %s: %v, want %q", result, err, evaluateUndescribed)
			}
		}
		// And while the runner sets itself up, before the code runs.
		expect(t, replaceToString, 1.0)
		fails(t, `(window.__aoSetUpRan = 1, 42)`, evaluateUndescribed)
		expect(t, `[window.__aoSetUp, typeof window.__aoSetUpRan]`, []any{1.0, "undefined"})
		// And while the answer is handed over: the second read of a
		// promise's constructor comes after the runner answered, where WebKit
		// awaits the runner's promise, and CDP awaits it natively.
		handedOver, _, err := call(t, evaluate, `(window.__aoHandedOver = (window.__aoHandedOver || 0) + 1, window.__aoConstructorReads = 0, Object.defineProperty(Promise.prototype, "constructor", {configurable: true, get() { if (++window.__aoConstructorReads === 1) return Promise; Object.defineProperty(Promise.prototype, "constructor", {value: Promise, writable: true, configurable: true}); throw new SyntaxError("replaced"); }}), 42)`)
		expect(t, `Object.defineProperty(Promise.prototype, "constructor", {value: Promise, writable: true, configurable: true}); return window.__aoHandedOver`, 1.0)
		if engine == evaluateEngineWebKit && (err == nil || !strings.Contains(err.Error(), evaluateUndescribed)) {
			t.Fatalf("with the answer's handover throwing: %v, want %q", err, evaluateUndescribed)
		}
		if engine == evaluateEngineCDP && (err != nil || handedOver != 42.0) {
			t.Fatalf("with the answer's handover throwing: %v, %v; want 42: CDP awaits the runner without reading its constructor", handedOver, err)
		}
		// The argument is parsed inside the runner.
		expect(t, `window.__aoParse = JSON.parse; window.__aoParsed = 0; JSON.parse = () => { window.__aoParsed++; throw new SyntaxError("replaced"); }; return 1`, 1.0)
		_, _, err = call(t, evaluate, `(arg) => arg`, `1`)
		expect(t, `JSON.parse = window.__aoParse; return window.__aoParsed`, 1.0)
		if err == nil || !strings.Contains(err.Error(), "Uncaught SyntaxError: replaced") {
			t.Fatalf("with JSON.parse replaced, the argument: %v", err)
		}
	})
	t.Run("code that does not compile never runs", func(t *testing.T) {
		fails(t, `window.__aoEarly = 1; )`, "SyntaxError")
		expect(t, `typeof window.__aoEarly`, "undefined")
		fails(t, `1 +`, "SyntaxError")
		fails(t, `let a; let a;`, "SyntaxError")
	})
	t.Run("results read as JSON.stringify writes them", func(t *testing.T) {
		expect(t, `({nan: NaN, inf: -Infinity, n: 1, gone: undefined})`, map[string]any{"nan": nil, "inf": nil, "n": 1.0})
		expect(t, `[NaN, Infinity, -Infinity, -0]`, []any{nil, nil, nil, 0.0})
		expect(t, `({a: undefined, b: 1, c: () => 1, d: Symbol("s"), e: 2})`, map[string]any{"b": 1.0, "e": 2.0})
		expect(t, `({a: undefined})`, map[string]any{})
		expect(t, `[undefined, () => 1, Symbol("s"), , 4]`, []any{nil, nil, nil, nil, 4.0})
		expect(t, `null`, nil)
		expect(t, `new Date(0)`, "1970-01-01T00:00:00.000Z")
		expect(t, `new Date(NaN)`, nil)
		expect(t, `({n: 12345678901234567890n})`, map[string]any{"n": "12345678901234567890"})
		expect(t, `[new Number(1), new String("s"), new Boolean(false)]`, []any{1.0, "s", false})
		expect(t, `({k: {toJSON(key) { return "key " + key; }}})`, map[string]any{"k": "key k"})
		expect(t, `({toJSON: Object.assign(() => 42, {call: () => 99})})`, 42.0)
		expect(t, `Object.prototype.toString.call = () => "[object Map]"; return {a: 1}`, map[string]any{"a": 1.0})
		expect(t, `delete Object.prototype.toString.call; return typeof Object.prototype.toString.call`, "function")
		expect(t, `const b = new Boolean(false); b.valueOf = () => true; return [b, JSON.stringify(b)]`, []any{false, "false"})
		expect(t, `"a\uD800b"`, "a�b")
		// What is left out takes none of the size limit.
		expect(t, `({["x".repeat(`+strconv.Itoa(maxEvaluateBytes)+`)]: undefined, k: 1})`, map[string]any{"k": 1.0})
		expect(t, `({["y".repeat(`+strconv.Itoa(maxEvaluateBytes)+`)]: {toJSON() {}}})`, map[string]any{})
		// The length is read once, as JSON.stringify reads it.
		expect(t, `const a = [0, 1]; Object.defineProperty(a, 0, {get() { a.push(2); return 0; }}); return a`, []any{0.0, 1.0})
	})
	t.Run("DOM and collection results", func(t *testing.T) {
		expect(t, `document.querySelector("#x")`, element)
		expect(t, `document.querySelectorAll("p")`, []any{element})
		expect(t, `document.querySelector("#x").classList`, []any{"a", "b"})
		expect(t, `document.querySelector("#x").firstChild`, "hi")
		expect(t, `new Map([["a", 1], [2, {b: true}]])`, []any{[]any{"a", 1.0}, []any{2.0, map[string]any{"b": true}}})
		expect(t, `new Set([1, 1, "two"])`, []any{1.0, "two"})
		expect(t, `new Uint8Array([1, 2, 255])`, []any{1.0, 2.0, 255.0})
		expect(t, `new DataView(new ArrayBuffer(2))`, map[string]any{})
		expect(t, `({e: new TypeError("bad")})`, map[string]any{"e": "TypeError: bad"})
		expect(t, `new DOMException("gone", "NotFoundError")`, "NotFoundError: gone")
		expect(t, `const s = document.createElement("select"); s.append(new Option("a")); return s.options`, []any{"<option>a</option>"})
	})
	t.Run("values from a same-origin frame read as their kind", func(t *testing.T) {
		expect(t, `const f = document.createElement("iframe"); f.id = "aoFrame"; document.body.append(f); return f.contentWindow !== window`, true)
		t.Cleanup(func() { expect(t, `document.getElementById("aoFrame").remove()`, nil) })
		expect(t, `const w = document.getElementById("aoFrame").contentWindow; return [w.document.body, w.document.querySelectorAll("body"), new w.Map([["a", 1]]), new w.Set([1]), new w.Uint8Array([2]), new w.Error("bad")]`,
			[]any{"<body></body>", []any{"<body></body>"}, []any{[]any{"a", 1.0}}, []any{1.0}, []any{2.0}, "Error: bad"})
		expect(t, `window.__aoForeignPromise = new (document.getElementById("aoFrame").contentWindow.Promise)(() => {}); return 1`, 1.0)
		failsFrom(t, readOnly, `window.__aoForeignPromise`, "use browser_evaluate")
	})
	t.Run("a proxy's prototype chain without end", func(t *testing.T) {
		// The proxies give up after 100000 links, which a walk without a
		// bound reaches.
		endless := `let links = 0, p; p = new Proxy({}, {getPrototypeOf() { if (++links > 100000) throw new Error("unbounded"); return p; }});`
		expect(t, endless+` return p`, map[string]any{})
		fails(t, endless+` throw p`, "Uncaught {}")
		expect(t, `let links = 0; const link = () => new Proxy({}, {getPrototypeOf() { if (++links > 100000) throw new Error("unbounded"); return link(); }}); return link()`, map[string]any{})
	})
	t.Run("cycles and shared values", func(t *testing.T) {
		expect(t, `const o = {n: 1}; o.self = o; o.list = [o]; return o`, map[string]any{"n": 1.0, "self": "[Circular]", "list": []any{"[Circular]"}})
		expect(t, `const m = new Map(); m.set("m", m); return m`, []any{[]any{"m", "[Circular]"}})
		expect(t, `const shared = {x: 1}; return [shared, {again: shared}]`,
			[]any{map[string]any{"x": 1.0}, map[string]any{"again": map[string]any{"x": 1.0}}})
	})
	t.Run("results past the bounds fail and the page answers on", func(t *testing.T) {
		nested := func(levels int) string {
			return `const root = []; let o = root; for (let i = 1; i < ` + strconv.Itoa(levels) + `; i++) { const next = []; o.push(next); o = next; } return root;`
		}
		expect(t, nested(1000), nestedArrays(1000))
		fails(t, nested(1001), "the result nests deeper than 1000 levels")
		expect(t, `"x".repeat(`+strconv.Itoa(maxEvaluateBytes-2)+`)`, strings.Repeat("x", maxEvaluateBytes-2))
		fails(t, `"x".repeat(`+strconv.Itoa(maxEvaluateBytes-1)+`)`, "the result exceeds 256000 bytes as JSON")
		// Within the page's count of UTF-16 code units, past the bound in
		// UTF-8 bytes.
		fails(t, `"é".repeat(200000)`, "exceeds 256000 bytes")
		// A collection is read entry by entry, so the limit ends a large one
		// early rather than after a copy of all of it.
		fails(t, `const s = new Set(); for (let i = 0; i < 200000; i++) s.add("entry" + i); const values = s.values.bind(s); window.__aoPulled = 0; s[Symbol.iterator] = function* () { for (const v of values()) { window.__aoPulled++; yield v; } }; return s`,
			"exceeds 256000 bytes")
		if pulled, _, err := call(t, evaluate, `window.__aoPulled`); err != nil || pulled == nil || pulled.(float64) >= 100000 {
			t.Fatalf("the encoder read %v of 200000 entries (%v), want it to stop at the limit", pulled, err)
		}
		expect(t, `1`, 1.0)
	})
	t.Run("read only", func(t *testing.T) {
		// Built where it may change the page, for Chrome's side-effect check.
		expect(t, `window.__aoShadowed = Object.assign(() => 42, {apply: () => 99, call: () => 99}); return 1`, 1.0)
		for _, tc := range []struct {
			code, argument string
			want           any
		}{
			// Chrome's side-effect check refuses getElementById outright,
			// in any form; querySelector passes it.
			{code: `document.querySelector("#x").textContent`, want: "hi"},
			{code: `let n = 0; for (const x of [1, 2]) n += x; return n`, want: 3.0},
			{code: `(() => 5)() // five`, want: 5.0},
			{code: `() => document.title`, want: "evaluate"},
			{code: `function () { "use strict"; return this === window; }`, want: true},
			{code: `window.__aoShadowed`, want: 42.0},
			{code: `(selector) => document.querySelector(selector).textContent`, argument: `"#x"`, want: "hi"},
			{code: `document.querySelector("#x")`, want: element},
			{code: `[document.querySelectorAll("p"), new Map([["a", new Date(0)]])]`, want: []any{[]any{element}, []any{[]any{"a", "1970-01-01T00:00:00.000Z"}}}},
		} {
			if tc.argument != "" {
				expectFrom(t, readOnly, tc.code, tc.want, tc.argument)
			} else {
				expectFrom(t, readOnly, tc.code, tc.want)
			}
		}
		failsFrom(t, readOnly, `await Promise.resolve(1)`, "cannot await")
		failsFrom(t, readOnly, `const v = await Promise.resolve(1); return v`, "cannot await")
		// A promise fails: WebKit returns it and the runner refuses it, and
		// Chrome may refuse the async call first. Either way the agent is
		// sent to browser_evaluate.
		failsFrom(t, readOnly, `async () => document.title`, "use browser_evaluate")
		failsFrom(t, readOnly, `Promise.resolve(document.title)`, "use browser_evaluate")
		if engine == evaluateEngineWebKit {
			// Chrome's side-effect check refuses creating any error, so
			// only WebKit's read-only runner meets one while describing.
			failsFrom(t, readOnly, `(window.__aoReadOnlyDescribed = (window.__aoReadOnlyDescribed || 0) + 1, (() => { throw `+indescribable+`; })())`,
				evaluateUndescribed)
			expect(t, `window.__aoReadOnlyDescribed`, 1.0)
			expect(t, `window.__aoStringify = JSON.stringify; return 1`, 1.0)
			_, _, err := call(t, readOnly, `(window.__aoReadOnlyReplaced = (window.__aoReadOnlyReplaced || 0) + 1, JSON.stringify = () => { throw new SyntaxError("replaced"); }, (() => { throw "page error"; })())`)
			expect(t, `JSON.stringify = window.__aoStringify; return window.__aoReadOnlyReplaced`, 1.0)
			if err == nil || !strings.Contains(err.Error(), evaluateUndescribed) {
				t.Fatalf("read only, with JSON.stringify replaced: %v, want %q", err, evaluateUndescribed)
			}
			expect(t, replaceToString, 1.0)
			failsFrom(t, readOnly, `(window.__aoReadOnlySetUpRan = 1, 42)`, evaluateUndescribed)
			expect(t, `[window.__aoSetUp, typeof window.__aoReadOnlySetUpRan]`, []any{1.0, "undefined"})
		}
		if engine == evaluateEngineCDP {
			// WebKit has no side-effect check; ReadOnlyCaveat says so.
			failsFrom(t, readOnly, `window.__aoReadOnly = 1`, "rejected a possible side effect")
			failsFrom(t, readOnly, `try { window.__aoReadOnly = 1 } catch (e) {} return 1`, "rejected a possible side effect")
			expect(t, `typeof window.__aoReadOnly`, "undefined")
		}
	})
}

func nestedArrays(levels int) any {
	var value any = []any{}
	for range levels - 1 {
		value = []any{value}
	}
	return value
}

// driverEvaluator evaluates code on one page driver the way the Manager does.
func driverEvaluator(ctx context.Context, driver pageDriver, readOnly bool) evaluator {
	return func(code string, argument json.RawMessage) (any, string, error) {
		return runEvaluation(ctx, func(ctx context.Context, source string) (json.RawMessage, error) {
			return driver.Evaluate(ctx, source, readOnly)
		}, code, argument, readOnly)
	}
}
