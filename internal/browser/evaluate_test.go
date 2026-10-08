package browser

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	cdpruntime "github.com/chromedp/cdproto/runtime"
)

func TestEvaluationSourceShapesTheCode(t *testing.T) {
	for _, tc := range []struct {
		name, code, argument string
		readOnly, statements bool
		want                 string
	}{
		{name: "expression", code: `document.title`, want: evaluateRunner + "(async function () {\nreturn (\ndocument.title\n);\n}, 256000, undefined, false)"},
		{name: "expression statement", code: "document.title; \n", want: evaluateRunner + "(async function () {\nreturn (\ndocument.title\n);\n}, 256000, undefined, false)"},
		{name: "expression ending in a line comment", code: "1 // one;", want: evaluateRunner + "(async function () {\nreturn (\n1 // one\n);\n}, 256000, undefined, false)"},
		{name: "statements", code: "const a = 1; return a // one", statements: true, want: evaluateRunner + "(async function () {\nconst a = 1; return a // one\n\n}, 256000, undefined, true)"},
		{name: "argument", code: `(arg) => arg`, argument: `{"s": "</script>\u2028"}`, want: evaluateRunner + "(async function () {\nreturn (\n(arg) => arg\n);\n}, 256000, " + jsonString(`{"s": "</script>\u2028"}`) + ", false)"},
		{name: "read only", code: `document.title`, readOnly: true, want: evaluateReadOnlyRunner + "(function () {\nreturn (\ndocument.title\n);\n}, 256000, undefined, false)"},
		{name: "read-only statements", code: `return 1`, readOnly: true, statements: true, want: evaluateReadOnlyRunner + "(function () {\nreturn 1\n\n}, 256000, undefined, true)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argument json.RawMessage
			if tc.argument != "" {
				argument = json.RawMessage(tc.argument)
			}
			if got := evaluationSource(tc.code, argument, tc.readOnly, tc.statements); got != tc.want {
				t.Fatalf("source = %q\nwant %q", got[len(got)-200:], tc.want[len(tc.want)-200:])
			}
		})
	}
}

// The engine reads the code: runEvaluation offers it as an expression, and as
// statements only when the engine reports the expression does not compile.
func TestRunEvaluationLetsTheEngineReadTheCode(t *testing.T) {
	syntax := func(message string) error { return &evaluateSyntaxError{message: message} }
	type answer struct {
		raw string
		err error
	}
	for _, tc := range []struct {
		name, code string
		readOnly   bool
		answers    []answer
		want       any
		wantNote   string
		wantErr    string
	}{
		{name: "an expression", code: `document.title`, answers: []answer{{raw: `"t"`}}, want: "t"},
		{name: "an expression with no value", code: `el.click()`, answers: []answer{{}}},
		{name: "statements", code: `const a = 1; return a`, answers: []answer{{err: syntax("SyntaxError: Unexpected token 'const'")}, {raw: `1`}}, want: 1.0},
		{name: "statements with no value", code: `const a = 1; a`, answers: []answer{{err: syntax("SyntaxError: Unexpected token 'const'")}, {}}, wantNote: evaluateStatementsNote},
		{name: "a throw is never retried", code: `JSON.parse("{")`, answers: []answer{{err: errors.New("Uncaught SyntaxError: bad JSON")}}, wantErr: "Uncaught SyntaxError: bad JSON"},
		{name: "an engine failure is never retried", code: `1`, answers: []answer{{err: errors.New("browser: the page is gone")}}, wantErr: "browser: the page is gone"},
		{name: "code that compiles neither way", code: `1 +`, answers: []answer{{err: syntax("SyntaxError: Unexpected token ')'")}, {err: syntax("SyntaxError: Unexpected token '}'")}}, wantErr: "SyntaxError: Unexpected token '}'"},
		{name: "await in read-only code", code: `await x`, readOnly: true, answers: []answer{{err: syntax("SyntaxError: a")}, {err: syntax("SyntaxError: b")}}, wantErr: "SyntaxError: b; browser_evaluate_readonly runs code synchronously and cannot await; use browser_evaluate"},
		{name: "a read-only syntax error without await", code: `1 +`, readOnly: true, answers: []answer{{err: syntax("SyntaxError: a")}, {err: syntax("SyntaxError: b")}}, wantErr: "SyntaxError: b"},
		{name: "await that compiles in read-only code", code: `await x`, readOnly: true, answers: []answer{{err: errors.New("Uncaught ReferenceError: await is not defined")}}, wantErr: "Uncaught ReferenceError: await is not defined"},
		{name: "a result past the bound", code: `x`, answers: []answer{{raw: `"` + strings.Repeat("é", maxEvaluateBytes/2) + `"`}}, wantErr: "the result exceeds 256000 bytes as JSON; return less, such as a count, a slice or only the fields you need"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sources []string
			run := func(_ context.Context, source string) (json.RawMessage, error) {
				i := len(sources)
				sources = append(sources, source)
				if i >= len(tc.answers) {
					t.Fatalf("source %d run; the engine answers %d", i+1, len(tc.answers))
				}
				if tc.answers[i].raw == "" {
					return nil, tc.answers[i].err
				}
				return json.RawMessage(tc.answers[i].raw), tc.answers[i].err
			}
			value, note, err := runEvaluation(t.Context(), run, tc.code, nil, tc.readOnly)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil || !reflect.DeepEqual(value, tc.want) || note != tc.wantNote {
				t.Fatalf("= %#v, %q, %v; want %#v, %q", value, note, err, tc.want, tc.wantNote)
			}
			want := []string{evaluationSource(tc.code, nil, tc.readOnly, false), evaluationSource(tc.code, nil, tc.readOnly, true)}[:len(tc.answers)]
			if !reflect.DeepEqual(sources, want) {
				t.Fatalf("ran %d sources, want the expression then the statements, %d in all", len(sources), len(want))
			}
		})
	}
	if _, _, err := runEvaluation(t.Context(), func(context.Context, string) (json.RawMessage, error) {
		t.Fatal("refused code reached the engine")
		return nil, nil
	}, " ", nil, false); err == nil || err.Error() != "expression is required" {
		t.Fatalf("empty code: %v", err)
	}
}

func TestCheckEvaluationInputBoundsTheInput(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		argument   json.RawMessage
		want       string
	}{
		{name: "empty", code: " \n", want: "expression is required"},
		{name: "too long", code: strings.Repeat("1", maxBrowserInputBytes+1), want: "expression exceeds"},
		{name: "argument not JSON", code: `(a) => a`, argument: json.RawMessage(`{bad`), want: "argument is not JSON"},
		{name: "argument too long", code: `(a) => a`, argument: json.RawMessage(`"` + strings.Repeat("a", maxBrowserInputBytes) + `"`), want: "argument exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkEvaluationInput(tc.code, tc.argument); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := checkEvaluationInput(strings.Repeat("1", maxBrowserInputBytes), json.RawMessage(`null`)); err != nil {
		t.Fatalf("code at the bound: %v", err)
	}
}

func TestEvaluationAnswerRejectsWhatTheRunnerNeverSends(t *testing.T) {
	for _, raw := range []string{``, `null`, `5`, `"not json"`, `"[1]"`} {
		if value, err := evaluationAnswer(json.RawMessage(raw)); err == nil {
			t.Fatalf("answer %q = %s, want an error", raw, value)
		}
	}
}

func TestEvaluateUndescribedFitsTheRunnersLiteral(t *testing.T) {
	// The runners spell it inside '{"error":"..."}'.
	if strings.ContainsAny(evaluateUndescribed, `'"\`+"\n") {
		t.Fatalf("%q cannot be spelled inside the runners' literal", evaluateUndescribed)
	}
}

func TestDecodeEvaluationBoundsTheResultInBytes(t *testing.T) {
	if value, err := decodeEvaluation(nil); value != nil || err != nil {
		t.Fatalf("absent result = %v, %v; want nil, nil", value, err)
	}
	// The page counts UTF-16 code units; each é is one there and two bytes
	// here, so this result passes the page's bound and must fail this one.
	within := json.RawMessage(`"` + strings.Repeat("é", (maxEvaluateBytes-2)/2) + `"`)
	if _, err := decodeEvaluation(within); err != nil {
		t.Fatalf("a result of %d bytes: %v", len(within), err)
	}
	past := json.RawMessage(`"` + strings.Repeat("é", maxEvaluateBytes/2) + `"`)
	if _, err := decodeEvaluation(past); err == nil || !strings.Contains(err.Error(), "exceeds 256000 bytes") {
		t.Fatalf("a result of %d bytes: %v", len(past), err)
	}
	if _, err := decodeEvaluation(json.RawMessage(`{bad`)); err == nil {
		t.Fatal("a result that is not JSON must be an error")
	}
}

// The Manager refuses code past the input bounds before it finds or creates a
// page, lets the engine read the code, refreshes the page after the code ran
// whether or not it failed, and names the tool only on the evaluation's own
// failures.
func TestManagerEvaluateRefusesBeforeThePageAndNamesTheTool(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	for _, tc := range []struct {
		code, argument string
		readOnly       bool
		want           string
	}{
		{code: " ", want: "browser: evaluate: expression is required"},
		{code: `(a) => a`, argument: `{bad`, want: "browser: evaluate: argument is not JSON"},
		{code: `(a) => a`, argument: `{bad`, readOnly: true, want: "browser: read-only evaluate: argument is not JSON"},
	} {
		var err error
		if tc.readOnly {
			_, _, err = manager.EvaluateReadOnly(t.Context(), access, "", tc.code, json.RawMessage(tc.argument))
		} else {
			_, _, err = manager.Evaluate(t.Context(), access, "", tc.code, json.RawMessage(tc.argument))
		}
		if err == nil || err.Error() != tc.want {
			t.Fatalf("evaluate %q error = %v, want %q", tc.code, err, tc.want)
		}
	}
	if pages := manager.CompanionState(access).Pages; len(pages) != 0 {
		t.Fatalf("refused code opened pages %+v", pages)
	}

	info, err := manager.Open(t.Context(), access, "https://example.test/before", OpenOptions{})
	if err != nil {
		t.Fatalf("open on the fake engine: %v", err)
	}
	page, _, err := manager.lookupOwnedPage(access, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := page.driver.(*fakePage)
	type call struct {
		source   string
		readOnly bool
	}
	var calls []call
	type answer struct {
		raw string
		err error
	}
	answers := func(list ...answer) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.evaluate = func(_ context.Context, source string, readOnly bool) (json.RawMessage, error) {
			calls = append(calls, call{source, readOnly})
			// The code navigated before it answered.
			fake.mu.Lock()
			fake.title = "after " + strconv.Itoa(len(calls))
			fake.mu.Unlock()
			next := list[0]
			list = list[1:]
			if next.raw == "" {
				return nil, next.err
			}
			return json.RawMessage(next.raw), next.err
		}
	}

	answers(answer{raw: `{"n":1}`})
	value, note, err := manager.EvaluateReadOnly(t.Context(), access, "", `(arg) => arg`, json.RawMessage(`{"n":1}`))
	if err != nil || note != "" || !reflect.DeepEqual(value, map[string]any{"n": 1.0}) {
		t.Fatalf("read-only evaluate = %#v, %q, %v", value, note, err)
	}
	want := evaluationSource(`(arg) => arg`, json.RawMessage(`{"n":1}`), true, false)
	if len(calls) != 1 || calls[0] != (call{want, true}) {
		t.Fatalf("engine calls = %+v, want the read-only expression source once", calls)
	}

	calls = nil
	answers(answer{err: &evaluateSyntaxError{message: "SyntaxError: Unexpected token 'const'"}}, answer{})
	value, note, err = manager.Evaluate(t.Context(), access, "", `const a = 1; a`, nil)
	if err != nil || value != nil || note != evaluateStatementsNote {
		t.Fatalf("statements = %#v, %q, %v; want no value and the statements note", value, note, err)
	}
	if len(calls) != 2 || calls[1] != (call{evaluationSource(`const a = 1; a`, nil, false, true), false}) {
		t.Fatalf("engine calls = %d, want the expression and then the statements", len(calls))
	}

	for _, tc := range []struct {
		name    string
		answers []answer
		want    string
	}{
		{name: "the code threw", answers: []answer{{err: errors.New("Uncaught TypeError: x is null")}}, want: "browser: evaluate: Uncaught TypeError: x is null"},
		{name: "the code does not compile", answers: []answer{{err: &evaluateSyntaxError{message: "SyntaxError: a"}}, {err: &evaluateSyntaxError{message: "SyntaxError: b"}}}, want: "browser: evaluate: SyntaxError: b"},
		{name: "the engine failed", answers: []answer{{err: errors.New("browser: the page is gone")}}, want: "browser: the page is gone"},
		{name: "the result is too large", answers: []answer{{raw: `"` + strings.Repeat("é", maxEvaluateBytes/2) + `"`}}, want: "browser: evaluate: the result exceeds 256000 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers(tc.answers...)
			_, _, err := manager.Evaluate(t.Context(), access, "", `location.href = "/after"`, nil)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("evaluate error = %v, want %q", err, tc.want)
			}
			if title := manager.CompanionState(access).Pages[0].Title; title != "after "+strconv.Itoa(len(calls)) {
				t.Fatalf("page title = %q: the Manager did not refresh the page after the code ran", title)
			}
		})
	}
	if last := calls[len(calls)-1]; last.readOnly {
		t.Fatal("browser_evaluate ran read-only")
	}
}

// An exception that escapes the runner is Chrome's: a source that did not
// compile, or the side-effect check's refusal.
func TestCDPEvaluateExceptionNamesACompileFailure(t *testing.T) {
	exception := func(className, description string) *cdpruntime.ExceptionDetails {
		return &cdpruntime.ExceptionDetails{Text: "Uncaught", Exception: &cdpruntime.RemoteObject{ClassName: className, Description: description}}
	}
	var syntax *evaluateSyntaxError
	err := cdpEvaluateException(exception("SyntaxError", "SyntaxError: Unexpected token ')'\n    at <anonymous>"), false)
	if !errors.As(err, &syntax) || err.Error() != "SyntaxError: Unexpected token ')'" {
		t.Fatalf("compile failure = %#v", err)
	}
	err = cdpEvaluateException(exception("EvalError", "EvalError: Possible side-effect in debug-evaluate"), true)
	if errors.As(err, &syntax) || !strings.HasPrefix(err.Error(), "Chrome rejected a possible side effect") {
		t.Fatalf("side-effect refusal = %#v", err)
	}
	err = cdpEvaluateException(exception("TypeError", "TypeError: x"), false)
	if errors.As(err, &syntax) || err.Error() != "TypeError: x" {
		t.Fatalf("other exception = %#v", err)
	}
}
