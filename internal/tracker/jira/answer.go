package jira

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// AnswerError is a successful answer this package could not read: a body
// that is not JSON, or JSON of another shape than the call's. For a
// write it means the write was applied; StepError.Applied reports it.
// Message holds no text of the answer that could carry the credential.
type AnswerError struct {
	Message string
}

func (e *AnswerError) Error() string { return "jira: " + e.Message }

// errCredentialChanged is an answer that arrived as the credential its
// request carried was dropped, so its text can no longer be checked for
// that credential and is not used.
var errCredentialChanged = &AnswerError{Message: "the credential changed while the answer was read; read it again"}

// decodeAnswer decodes resp's body into out. It is the one place a
// successful answer is decoded: every string in the body, object keys
// included, has the credential the request carried replaced through
// resp.Scrub before any of it reaches out, so no text a call returns,
// puts in an error or lists as a problem or warning can repeat it, and
// text written with JSON escapes is checked as it decodes, not as sent.
// A decode error is an *AnswerError whose text is scrubbed too.
func decodeAnswer(resp *trackerapi.Response, out any) error {
	body, err := scrubbedBody(resp)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &AnswerError{Message: "the answer did not decode: " + resp.Scrub(err.Error())}
	}
	return nil
}

// scrubbedBody is resp's body with every string scrubbed: the body itself
// when no string holds the credential, and the scrubbed document
// encoded again otherwise.
func scrubbedBody(resp *trackerapi.Response) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, &AnswerError{Message: "the answer is not JSON: " + resp.Scrub(err.Error())}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &AnswerError{Message: "the answer holds more than one JSON value"}
	}
	var texts []string
	collectStrings(tree, &texts)
	if len(texts) == 0 {
		return resp.Body, nil
	}
	// A credential holds no control character, so a dropped one is the
	// only reason this probe changes.
	if resp.Scrub("\x00") != "\x00" {
		return nil, errCredentialChanged
	}
	// One check for the common answer that holds no credential: each
	// string is a part of the joined text, so none holds a form when the
	// joined text holds none.
	joined := strings.Join(texts, "\x00")
	if resp.Scrub(joined) == joined {
		return resp.Body, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(scrubStrings(tree, resp.Scrub)); err != nil {
		return nil, &AnswerError{Message: "the scrubbed answer did not encode: " + resp.Scrub(err.Error())}
	}
	return buf.Bytes(), nil
}

// collectStrings appends every string in a decoded JSON value, object
// keys included.
func collectStrings(v any, out *[]string) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case map[string]any:
		for k, e := range v {
			*out = append(*out, k)
			collectStrings(e, out)
		}
	case []any:
		for _, e := range v {
			collectStrings(e, out)
		}
	}
}

// scrubStrings is v with scrub applied to every string, object keys
// included.
func scrubStrings(v any, scrub func(string) string) any {
	switch v := v.(type) {
	case string:
		return scrub(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[scrub(k)] = scrubStrings(e, scrub)
		}
		return out
	case []any:
		for i, e := range v {
			v[i] = scrubStrings(e, scrub)
		}
		return v
	}
	return v
}
