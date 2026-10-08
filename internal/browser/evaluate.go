package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// The evaluate tools take JavaScript the way Playwright's evaluate does: an
// expression, whose value is the result, or a function, called with the
// optional JSON argument. Other code runs as statements, the body of a
// function, and gives a value only through return. browser_evaluate runs the
// code in an async function, so await works and a returned promise is
// awaited; browser_evaluate_readonly runs it synchronously, because Chrome's
// side-effect check refuses every await, so a promise result fails there.
// Declarations belong to the call, and `this` is the window.
//
// The engine reads the code, not the tools: runEvaluation hands it the code
// as an expression, and as statements only when the engine reports that the
// expression does not compile. Nothing in a source runs unless all of it
// compiles, and nothing the code does escapes the runner, so that report
// means nothing ran, and no code runs twice. Every engine evaluates natively,
// not through the page's eval, so a page whose script policy forbids
// 'unsafe-eval' runs both forms. The runner encodes the result in the page
// and hands back one JSON text on every engine (evaluationAnswer).

// evaluateStatementsNote comes with a result of statements that gave none.
const evaluateStatementsNote = "No value: code that is not one expression runs as statements, which give a value only through return."

// evaluateSyntaxError is an engine's report that a source does not compile.
// Nothing in the source ran.
type evaluateSyntaxError struct{ message string }

func (e *evaluateSyntaxError) Error() string { return e.message }

// runEvaluation runs code through run, which evaluates one source in the page
// as pageDriver.Evaluate does. It returns the result and a note the result
// carries, or "".
func runEvaluation(ctx context.Context, run func(context.Context, string) (json.RawMessage, error), code string, argument json.RawMessage, readOnly bool) (any, string, error) {
	if err := checkEvaluationInput(code, argument); err != nil {
		return nil, "", err
	}
	raw, err := run(ctx, evaluationSource(code, argument, readOnly, false))
	var syntax *evaluateSyntaxError
	statements := errors.As(err, &syntax)
	if statements {
		raw, err = run(ctx, evaluationSource(code, argument, readOnly, true))
		if readOnly && errors.As(err, &syntax) && strings.Contains(code, "await") {
			err = fmt.Errorf("%w; browser_evaluate_readonly runs code synchronously and cannot await; use browser_evaluate", err)
		}
	}
	if err != nil {
		return nil, "", err
	}
	value, err := decodeEvaluation(raw)
	if err != nil {
		return nil, "", err
	}
	if statements && raw == nil {
		return nil, evaluateStatementsNote, nil
	}
	return value, "", nil
}

// checkEvaluationInput refuses code and an argument past the input bounds.
func checkEvaluationInput(code string, argument json.RawMessage) error {
	switch {
	case len(code) > maxBrowserInputBytes:
		return fmt.Errorf("expression exceeds %d bytes", maxBrowserInputBytes)
	case strings.TrimSpace(code) == "":
		return errors.New("expression is required")
	case len(argument) > maxBrowserInputBytes:
		return fmt.Errorf("argument exceeds %d bytes", maxBrowserInputBytes)
	case len(argument) > 0 && !json.Valid(argument):
		return errors.New("argument is not JSON")
	}
	return nil
}

// evaluationSource is what an engine evaluates: the runner called with the
// code as the body of a function, either returning the code as an expression
// or running it as statements.
func evaluationSource(code string, argument json.RawMessage, readOnly, statements bool) string {
	// A line comment at the end must not swallow what follows the code.
	body := code + "\n"
	if !statements {
		// The semicolon that ends an expression statement cannot end an
		// expression.
		body = "return (\n" + strings.TrimRightFunc(code, func(r rune) bool { return r == ';' || unicode.IsSpace(r) }) + "\n);"
	}
	// The runner parses the argument's JSON text, where what the page's
	// JSON.parse throws is caught.
	text := "undefined"
	if len(argument) > 0 {
		text = jsonString(string(argument))
	}
	runner, function := evaluateRunner, "async function"
	if readOnly {
		runner, function = evaluateReadOnlyRunner, "function"
	}
	return runner + "(" + function + " () {\n" + body + "\n}, " + strconv.Itoa(maxEvaluateBytes) + ", " + text + ", " + strconv.FormatBool(statements) + ")"
}

// evaluationAnswer reads what the runner returned, which the engine hands
// over as a JSON string: the text of {"value": V}, of {} when the result is
// undefined, or of {"error": message}. It returns V's JSON text, nil for
// undefined.
func evaluationAnswer(raw json.RawMessage) (json.RawMessage, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("the engine answered %.100q, not the evaluation's text", raw)
	}
	var answer struct {
		Value json.RawMessage `json:"value"`
		Error *string         `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &answer); err != nil {
		return nil, fmt.Errorf("read the evaluation's answer: %w", err)
	}
	if answer.Error != nil {
		return nil, errors.New(*answer.Error)
	}
	return answer.Value, nil
}

// decodeEvaluation decodes the JSON text of an evaluation's value, nil for
// undefined. The page bounded the text in UTF-16 code units; this bounds it
// in bytes.
func decodeEvaluation(raw json.RawMessage) (any, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw) > maxEvaluateBytes {
		return nil, fmt.Errorf("the result exceeds %d bytes as JSON; return less, such as a count, a slice or only the fields you need", maxEvaluateBytes)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode the result: %w", err)
	}
	return value, nil
}

// evaluateEncoder declares, inside a runner, what turns the result into the
// runner's answer. The result reads as JSON.stringify writes it, with these
// exceptions:
//
//   - A value that refers back to an object containing it reads as
//     "[Circular]"; JSON.stringify throws instead.
//   - A DOM node reads as its outerHTML when it is an element, as its text
//     when it is a text or comment node, and as its name otherwise.
//   - A Map reads as an array of [key, value] pairs; a Set, a typed array,
//     a NodeList, an HTMLCollection and a DOMTokenList read as arrays.
//   - An Error or a DOMException reads as its "Name: message", and a BigInt
//     as its digits.
//   - A lone surrogate is escaped, which the Go side reads as U+FFFD.
//
// Kinds are read from tags (Object.prototype.toString) and prototype chains,
// which answer in every realm, so a node or a Map from a same-origin frame
// reads as one; instanceof answers only for this window's own.
//
// The encoder stops past the size limit, counted in UTF-16 code units, which
// never exceed the text's UTF-8 bytes, and past 1000 levels of nesting.
// Chrome's side-effect check accepts all of it, which is what lets the
// read-only runner encode inside that check.
const evaluateEncoder = `
	const stop = {};
	let refusal = "";
	const refuse = (message) => {
		refusal = message;
		throw stop;
	};
	const fail = (message) => '{"error":' + JSON.stringify(message.slice(0, 1000)) + "}";
	const toString = Object.prototype.toString;
	const tagOf = (value) => Reflect.apply(toString, value, []).slice(8, -1);
	const kinds = ["Node", "NodeList", "HTMLCollection", "DOMTokenList", "Map", "Set", "Error", "DOMException"];
	// kindOf names the first of kinds on the object's tag or its prototype
	// chain, or "" for none: an element's chain reaches Node, and a select's
	// options reach HTMLCollection. A proxy can answer a chain without end,
	// and no kind sits deep in one, so the walk stops after 100 links.
	const kindOf = (object) => {
		let o = object;
		for (let link = 0; o !== null && link < 100; link++) {
			const tag = tagOf(o);
			if (kinds.includes(tag)) return tag;
			o = Object.getPrototypeOf(o);
		}
		return "";
	};
	const isError = (kind) => kind === "Error" || kind === "DOMException";
	// resolve answers what JSON.stringify writes for value under key: what
	// toJSON returns, unboxed, or undefined when it writes nothing.
	const resolve = (key, value) => {
		if (value !== null && (typeof value === "object" || typeof value === "function" || typeof value === "bigint")) {
			const toJSON = value.toJSON;
			if (typeof toJSON === "function") value = Reflect.apply(toJSON, value, [key]);
		}
		switch (typeof value) {
		case "undefined":
		case "symbol":
		case "function":
			return undefined;
		case "object":
			if (value === null) return null;
			switch (tagOf(value)) {
			case "Number":
				return Number(value);
			case "String":
				return String(value);
			case "Boolean":
				return Reflect.apply(Boolean.prototype.valueOf, value, []);
			}
		}
		return value;
	};
	const json = (root, budget) => {
		const parts = [], ancestors = [];
		let size = 0;
		const fits = (length) => {
			if (size + length > budget) {
				refuse("the result exceeds " + budget + " bytes as JSON; return less, such as a count, a slice or only the fields you need");
			}
		};
		const add = (text) => {
			fits(text.length);
			size += text.length;
			parts.push(text);
		};
		// A string's JSON is at least its length and two quotes, so one past
		// the budget is refused before it is copied.
		const quote = (text) => {
			fits(text.length + 2);
			add(JSON.stringify(text));
		};
		// item writes one entry of an array, where nothing reads as null.
		const item = (key, value) => {
			const resolved = resolve(key, value);
			if (resolved === undefined) add("null");
			else write(resolved);
		};
		const write = (value) => {
			switch (typeof value) {
			case "string":
				quote(value);
				return;
			case "number":
				add(Number.isFinite(value) ? String(value) : "null");
				return;
			case "boolean":
				add(value ? "true" : "false");
				return;
			case "bigint":
				add('"' + value + '"');
				return;
			}
			if (value === null) {
				add("null");
				return;
			}
			if (ancestors.includes(value)) {
				add('"[Circular]"');
				return;
			}
			if (ancestors.length === 1000) refuse("the result nests deeper than 1000 levels");
			const kind = kindOf(value);
			if (kind === "Node") {
				quote(value.nodeType === 1 ? value.outerHTML : typeof value.data === "string" ? value.data : value.nodeName);
				return;
			}
			if (isError(kind)) {
				quote(String(value));
				return;
			}
			ancestors.push(value);
			if (Array.isArray(value) || ArrayBuffer.isView(value) && tagOf(value) !== "DataView" || kind === "NodeList" || kind === "HTMLCollection" || kind === "DOMTokenList") {
				// JSON.stringify reads an array's length once.
				const length = value.length;
				add("[");
				for (let i = 0; i < length; i++) {
					if (i > 0) add(",");
					item(String(i), value[i]);
				}
				add("]");
			} else if (kind === "Map" || kind === "Set") {
				// Entry by entry, so the size limit ends a large collection
				// without a copy of it.
				add("[");
				let i = 0;
				for (const entry of value) {
					if (i > 0) add(",");
					item(String(i++), entry);
				}
				add("]");
			} else {
				add("{");
				let first = true;
				for (const name of Object.keys(value)) {
					const resolved = resolve(name, value[name]);
					if (resolved === undefined) continue;
					add((first ? "" : ",") + JSON.stringify(name) + ":");
					first = false;
					write(resolved);
				}
				add("}");
			}
			ancestors.pop();
		};
		const value = resolve("", root);
		if (value === undefined) return undefined;
		write(value);
		return parts.join("");
	};
	const describe = (error) => {
		if (typeof error === "string") return error;
		if ((typeof error === "object" && error !== null) && isError(kindOf(error))) return String(error);
		const text = json(error, 1000);
		return text === undefined ? String(error) : text;
	};
	const encode = (value) => {
		try {
			const text = json(value, limit);
			return text === undefined ? "{}" : '{"value":' + text + "}";
		} catch (error) {
			return fail(error === stop ? refusal : "Uncaught " + describe(error));
		}
	};
`

// evaluateUndescribed is a runner's answer when describing the result or
// what the code threw throws in turn, or when a built-in the runner reads,
// which the page can replace, throws. That can happen before or after the
// code ran. evaluateUndescribedAnswer spells it inside a JavaScript string
// literal, so it has no quotes or backslashes.
const evaluateUndescribed = "the outcome of the evaluation could not be described; the code may have run"

// evaluateUndescribedAnswer is the runners' last answer: a JavaScript string
// literal of what evaluationAnswer reads, which cannot throw.
const evaluateUndescribedAnswer = `'{"error":"` + evaluateUndescribed + `"}'`

// evaluateArguments declares, inside a runner, the argument list parsed from
// its JSON text, and what refuses an argument. A JSON null reads as no
// argument where one is refused, as callers fill optional fields with null; a
// function is still called with null.
const evaluateArguments = `
			const args = argument === undefined ? [] : [JSON.parse(argument)];
			const argued = args.length > 0 && args[0] !== null;
			const refuseArgument = () => fail("an argument needs code that is one function, such as (arg) => arg.id");
			if (statements && argued) return refuseArgument();`

// evaluateRunner is called with the async function evaluationSource built,
// the size limit, the argument's JSON text or undefined, and whether the
// function runs the code as statements, and resolves to the answer
// evaluationAnswer reads. It calls a function value with the window as this.
// What the code throws is the answer's error.
//
// Nothing escapes the runner: the engine would report an escaped SyntaxError
// as code that does not compile, and runEvaluation would run the code again.
// Everything after "use strict", the encoder's declarations included, runs
// inside one try whose answer is a literal, which cannot throw. Calls go through
// Reflect.apply, so a callable's own apply or call properties are not read.
const evaluateRunner = `(async function (body, limit, argument, statements) {
	"use strict";
	try {` + evaluateEncoder + `
		let value;
		try {` + evaluateArguments + `
			value = await Reflect.apply(body, globalThis, []);
			if (!statements && typeof value === "function") {
				value = await Reflect.apply(value, globalThis, args);
			} else if (argued) {
				return refuseArgument();
			}
		} catch (error) {
			return fail("Uncaught " + describe(error));
		}
		return encode(value);
	} catch (_) {
		return ` + evaluateUndescribedAnswer + `;
	}
})`

// evaluateReadOnlyRunner is evaluateRunner without an await, which Chrome's
// side-effect check would refuse, for the plain function a read-only
// evaluation builds. It refuses a promise result, from any realm, instead.
const evaluateReadOnlyRunner = `(function (body, limit, argument, statements) {
	"use strict";
	try {` + evaluateEncoder + `
		let value;
		try {` + evaluateArguments + `
			value = Reflect.apply(body, globalThis, []);
			if (!statements && typeof value === "function") {
				value = Reflect.apply(value, globalThis, args);
			} else if (argued) {
				return refuseArgument();
			}
			if ((typeof value === "object" && value !== null || typeof value === "function") && typeof value.then === "function") {
				return fail("the code returned a promise, which browser_evaluate_readonly cannot await; return the value itself, or use browser_evaluate");
			}
		} catch (error) {
			return fail("Uncaught " + describe(error));
		}
		return encode(value);
	} catch (_) {
		return ` + evaluateUndescribedAnswer + `;
	}
})`
