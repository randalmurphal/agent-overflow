// app_harness_json.go holds the one JSON-decoding rule the harness RPC
// surface shares.
//
// Two properties, both about a harness caller being a SCRIPT rather than
// a person reading a stack trace:
//
//   - STRICT. An unknown field in a seed spec or a scenario document is
//     a typo, and accepting it silently means the caller's `treads: [...]`
//     produces an empty, successful seed and a test that fails much later
//     for an unrelated-looking reason. `internal/harness/scenario`'s
//     parser has always been strict; the seed spec was not, and the two
//     halves of one document format disagreeing is worse than either
//     choice.
//   - POSITIONED. encoding/json's own message names the offending
//     character and nothing else ("invalid character '}' looking for
//     beginning of object key string"), which in a 200-line generated
//     seed spec is unactionable. The byte offset is already on the error
//     or on the decoder; line/column is a newline count away.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// decodeStrictJSON decodes raw into dst, refusing unknown fields and
// trailing content, and annotating any failure with its position.
//
// `what` names the document for the message ("seed spec", "scenario").
func decodeStrictJSON(what string, raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%s is empty", what)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return annotateJSONError(what, raw, dec.InputOffset(), err)
	}
	// A second value in the stream means the caller sent two documents
	// (or one document and a stray brace); silently reading the first
	// would obey half of what they wrote.
	if dec.More() {
		return fmt.Errorf("%s has trailing content after the first JSON value%s",
			what, jsonPositionSuffix(raw, dec.InputOffset()))
	}
	return nil
}

// annotateJSONError appends the failure's position to a decode error.
// `fallbackOffset` is the decoder's own InputOffset, used when the error
// itself carries none — which is the case for DisallowUnknownFields
// rejections, the very errors a caller most needs located.
func annotateJSONError(what string, raw []byte, fallbackOffset int64, err error) error {
	offset := fallbackOffset
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		offset = syntaxErr.Offset
	case errors.As(err, &typeErr):
		offset = typeErr.Offset
	}
	return fmt.Errorf("%s: %w%s", what, err, jsonPositionSuffix(raw, offset))
}

// jsonPositionSuffix renders " (at line L, column C, byte N)" for an
// offset inside raw. Empty when the offset is not usable, so a caller can
// always append it unconditionally.
//
// Column is counted in BYTES, not runes: the value exists to be fed to an
// editor's goto-line, and a byte column is what every tool here (Go, jq,
// most editors' JSON linters) already reports.
func jsonPositionSuffix(raw []byte, offset int64) string {
	if offset <= 0 || offset > int64(len(raw)) {
		return ""
	}
	line, column := 1, 1
	for _, b := range raw[:offset-1] {
		if b == '\n' {
			line++
			column = 1
			continue
		}
		column++
	}
	return fmt.Sprintf(" (at line %d, column %d, byte offset %d)", line, column, offset)
}

// requireValidJSON is the cheap half for surfaces that forward a document
// verbatim rather than decoding it (the ui-query spec and its reply, the
// HarnessEmit payload): they only need "is this JSON", but a caller
// whose answer is no deserves to be told where.
func requireValidJSON(what string, raw []byte) error {
	if json.Valid(raw) {
		return nil
	}
	var probe json.RawMessage
	err := json.Unmarshal(raw, &probe)
	if err == nil {
		// json.Valid disagreed with Unmarshal — impossible in practice,
		// but a nil error here would report success on invalid input.
		return fmt.Errorf("%s is not valid JSON", what)
	}
	return annotateJSONError(what, raw, 0, err)
}
