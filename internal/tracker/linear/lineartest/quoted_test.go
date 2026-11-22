package lineartest

import (
	"encoding/json"
	"testing"
)

// A value holding a quote or a control character stays one JSON string in
// every builder that places a caller's value.
func TestBuildersQuoteTheirValues(t *testing.T) {
	const v = "a\"b\\c\x01"
	ref := IssueRef{ID: v, Identifier: v, Title: v, State: v}
	for name, doc := range map[string]string{
		"viewer":     ViewerAnswer(v),
		"connection": ConnectionJSON(nil, true, v),
		"relation":   RelationJSON(v, v, ref, ref),
		"changes":    IssueChangesJSON(v, v, v, nil, true, v),
	} {
		var got any
		if err := json.Unmarshal([]byte(doc), &got); err != nil {
			t.Errorf("%s: %v in %s", name, err, doc)
			continue
		}
		if count(got, v) == 0 {
			t.Errorf("%s: the value did not come back whole from %s", name, doc)
		}
	}
}

// count is how many strings under doc equal want.
func count(doc any, want string) int {
	n := 0
	switch d := doc.(type) {
	case string:
		if d == want {
			n = 1
		}
	case []any:
		for _, e := range d {
			n += count(e, want)
		}
	case map[string]any:
		for _, e := range d {
			n += count(e, want)
		}
	}
	return n
}
