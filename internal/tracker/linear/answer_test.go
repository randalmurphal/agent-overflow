package linear

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func identity(s string) string { return s }

// settleWrite is what an answer the transport passed proves about an
// ArchiveIssue write.
func settleWrite(body string) (outcome, error) {
	var data mutationData[wireIssue]
	err := (&Client{}).readAnswer(answer{op: opArchiveIssue, status: 200, scrub: identity}, []byte(body), &data)
	if err == nil && data.Result.Value == nil {
		err = errors.New("no entity")
	}
	return outcomeOf(err), err
}

// Malformed answers to a write, each with the outcome it proves. A
// GraphQL answer is one JSON object naming each member once with nothing
// after it; any other answer proves nothing, so a write is unknown,
// whatever it says. A present success: false proves the refusal even
// when the entity beside it does not decode. Each row's outcome is the
// one the earlier two-step decode gave, except where noted: it took the
// last of a repeated member.
func TestWriteAnswerShapes(t *testing.T) {
	t.Parallel()
	issue := lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	ok := `{"data":{"result":{"success":true,"value":` + issue + `}}}`
	userErr := `[{"message":"bad","extensions":{"code":"INPUT_ERROR","userError":true}}]`
	badEntity := `{"id":"issue-1","updatedAt":"invalid"}`
	for _, c := range []struct {
		name, body string
		want       outcome
	}{
		{"complete", ok, applied},
		{"whitespace after", ok + " \n\t", applied},
		{"another member beside data", `{"extensions":{"x":1},"data":{"result":{"success":true,"value":` + issue + `}}}`, applied},
		{"text not UTF-8", `{"data":{"result":{"success":true,"value":` + strings.Replace(issue, `"WEB-1"`, "\"WEB-\xff\"", 1) + `}}}`, applied},
		// Repeated members, refused anywhere (the earlier decode took the
		// last, so the second row read as applied).
		{"data twice, applied first", `{"data":{"result":{"success":true,"value":` + issue + `}},"data":{"result":{}}}`, unknown},
		{"data twice, applied last", `{"data":{"result":{}},"data":{"result":{"success":true,"value":` + issue + `}}}`, unknown},
		{"result twice", `{"data":{"result":{"success":false},"result":{"success":true,"value":` + issue + `}}}`, unknown},
		{"success twice", `{"data":{"result":{"success":false,"success":true,"value":` + issue + `}}}`, unknown},
		{"a member of the entity twice", `{"data":{"result":{"success":true,"value":` + strings.Replace(issue, `"id":"issue-1"`, `"id":"issue-1","id":"issue-2"`, 1) + `}}}`, unknown},
		{"errors twice", `{"errors":` + userErr + `,"errors":[],"data":null}`, unknown},
		// What follows the object.
		{"bytes after", ok + `garbage`, unknown},
		{"a second object after", ok + `{}`, unknown},
		{"a refusal followed by bytes", `{"data":{"result":{"success":false}}}x`, unknown},
		// Not an answer.
		{"empty body", ``, unknown},
		{"whitespace", "  \n", unknown},
		{"null", `null`, unknown},
		{"an array", `[]`, unknown},
		{"cut short", `{"data":{"result":{"success":true`, unknown},
		{"no members", `{}`, unknown},
		// Wrong types.
		{"data an array", `{"data":[]}`, unknown},
		{"data a string", `{"data":"x"}`, unknown},
		{"result a string", `{"data":{"result":"x"}}`, unknown},
		{"errors an object", `{"data":null,"errors":{"message":"x"}}`, unknown},
		{"an error of the wrong shape", `{"data":null,"errors":[{"message":1}]}`, unknown},
		// The entity does not decode: the payload's success decides.
		{"refused, entity does not decode", `{"data":{"result":{"success":false,"value":` + badEntity + `}}}`, refused},
		{"refused with errors, entity does not decode", `{"data":{"result":{"success":false,"value":` + badEntity + `}},"errors":` + userErr + `}`, refused},
		{"applied, entity does not decode", `{"data":{"result":{"success":true,"value":` + badEntity + `}}}`, unknown},
		{"entity of the wrong type", `{"data":{"result":{"success":true,"value":[]}}}`, unknown},
		// Partial data and errors beside it.
		{"no result, user errors", `{"data":{"result":null},"errors":` + userErr + `}`, refused},
		{"data null, user errors", `{"data":null,"errors":` + userErr + `}`, refused},
		{"applied with errors", `{"data":{"result":{"success":true,"value":` + issue + `}},"errors":` + userErr + `}`, unknown},
		{"empty errors beside success", `{"data":{"result":{"success":true,"value":` + issue + `}},"errors":[]}`, applied},
		{"success missing", `{"data":{"result":{"value":` + issue + `}}}`, unknown},
	} {
		got, err := settleWrite(c.body)
		if got != c.want {
			t.Errorf("%s: %s, want %s (%v)", c.name, got, c.want, err)
		}
	}
}

// Malformed answers to a read: an answer that is not a whole, well-formed
// GraphQL answer is a decode error; errors beside data are the call's
// error whatever the data holds, so a not-found stays one; data absent
// with no errors is an *Error naming none.
func TestReadAnswerShapes(t *testing.T) {
	t.Parallel()
	notFoundErr := `[{"message":"Entity not found: Issue","path":["issue"],"extensions":{"code":"INPUT_ERROR","userError":true}}]`
	read := func(body string) error {
		var data struct {
			Issue *wireIssue `json:"issue"`
		}
		return (&Client{}).readAnswer(answer{op: opIssueSummary, status: 200, scrub: identity}, []byte(body), &data)
	}
	isDecode := func(err error) bool { return err != nil && strings.Contains(err.Error(), "decode the answer") }
	isNotFound := func(err error) bool { return errors.Is(err, trackerapi.ErrNotFound) }
	isEmpty := func(err error) bool { e, ok := errors.AsType[*Error](err); return ok && len(e.Errors) == 0 }
	for _, c := range []struct {
		name string
		body string
		want func(error) bool
	}{
		{"not found beside data that does not decode", `{"data":{"issue":"x"},"errors":` + notFoundErr + `}`, isNotFound},
		{"not found beside null data", `{"data":null,"errors":` + notFoundErr + `}`, isNotFound},
		{"no data, no errors", `{"data":null}`, isEmpty},
		{"no members", `{}`, isEmpty},
		{"data that does not decode", `{"data":{"issue":"x"}}`, isDecode},
		{"data twice", `{"data":{"issue":null},"data":{"issue":null}}`, isDecode},
		{"a member twice deep in data", `{"data":{"issue":{"id":"a","id":"b"}}}`, isDecode},
		{"bytes after", `{"data":{"issue":null}}x`, isDecode},
		{"empty body", ``, isDecode},
		{"not an object", `[]`, isDecode},
	} {
		if err := read(c.body); !c.want(err) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := read(`{"data":{"issue":null}}`); err != nil {
		t.Errorf("a well-formed answer: %v", err)
	}
}

// The outcome of an aliased write is the least of its payloads': an
// alias the data lacks or holds null is no result, so the write is not
// taken as applied.
func TestReadOutcomeOfAliases(t *testing.T) {
	t.Parallel()
	results := []string{"i0", "i1"}
	for data, want := range map[string]payloadState{
		`{"i0":{"success":true},"i1":{"success":true}}`:  payloadSucceeded,
		`{"i0":{"success":true}}`:                        payloadNone,
		`{"i0":{"success":true},"i1":null}`:              payloadNone,
		`{"i0":{"success":true},"i1":{"success":false}}`: payloadFailed,
		`{"i0":{"success":true},"i1":{}}`:                payloadUnsaid,
		`{"i0":{"success":true},"i1":"x"}`:               payloadUnsaid,
	} {
		if got := readOutcome([]byte(data), results); got != want {
			t.Errorf("%s: %d, want %d", data, got, want)
		}
	}
	if got := readOutcome([]byte(`{"result":{"success":true}}`), nil); got != payloadUnsaid {
		t.Errorf("a write that names no payload: %d", got)
	}
}

// A write is never taken as applied unless its answer is one complete,
// well-formed JSON object naming each member once, whose errors are
// absent or empty, and whose payload says success: true.
func FuzzWriteAnswer(f *testing.F) {
	issue := lineartest.IssueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	for _, seed := range []string{
		`{"data":{"result":{"success":true,"value":` + issue + `}}}`,
		`{"data":{"result":{"success":false,"value":` + issue + `}}}`,
		`{"data":{"result":{"success":true,"value":` + issue + `}},"data":{"result":{}}}`,
		`{"data":{"result":{"success":true,"value":` + issue + `}}}garbage`,
		`{"data":null,"errors":[{"message":"bad"}]}`,
		``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if got, _ := settleWrite(string(body)); got != applied {
			return
		}
		var doc struct {
			Data struct {
				Result struct {
					Success jsontext.Value `json:"success"`
				} `json:"result"`
			} `json:"data"`
			Errors []jsontext.Value `json:"errors"`
		}
		err := jsonv2.Unmarshal(body, &doc, jsontext.AllowInvalidUTF8(true))
		if err != nil || len(doc.Errors) > 0 || string(doc.Data.Result.Success) != "true" {
			t.Fatalf("applied from %q (%v)", body, err)
		}
	})
}
