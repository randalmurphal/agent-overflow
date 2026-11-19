package linear

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// The variable check refuses what the schema refuses, independently of
// any expected JSON.
func TestVariableProblems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		op   operation
		vars string
		want string
	}{
		{"valid", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"blocks"}}`, ""},
		{"unknown enum value", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"bogus"}}`, "bogus is not a value of IssueRelationType"},
		{"enum case", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"Blocks"}}`, "Blocks is not a value of IssueRelationType"},
		{"missing required field", opCreateRelation, `{"input":{"issueId":"WEB-1","type":"blocks"}}`, "relatedIssueId is required"},
		{"unknown field", opCreateRelation, `{"input":{"issueId":"WEB-1","relatedIssueId":"WEB-2","type":"blocks","kind":"x"}}`, "kind is not a field"},
		{"missing variable", opCreateRelation, `{}`, "required $input is missing"},
		{"null required", opUpdateIssue, `{"id":null,"input":{}}`, "$id is null"},
		{"undeclared variable", opViewer, `{"x":1}`, "$x is not declared"},
		{"wrong scalar", opTeams, `{"first":"100"}`, "is not a Int"},
		{"fraction for Int", opTeams, `{"first":1.5}`, "is not a Int"},
		{"Int out of range", opTeams, `{"first":3000000000}`, "is not a Int"},
		{"sort enum case", opIssues, `{"first":100,"sort":[{"updatedAt":{"order":"ascending"}}]}`, "ascending is not a value of PaginationSortOrder"},
		{"list expected", opIssues, `{"first":100,"sort":{"updatedAt":{"order":"Ascending"}}}`, "is not a list"},
		{"nullable null", opUpdateIssue, `{"id":"WEB-1","input":{"assigneeId":null,"labelIds":[]}}`, ""},
		{"day", opUpdateIssue, `{"id":"WEB-1","input":{"dueDate":"2026-12-01"}}`, ""},
		{"not a day", opUpdateIssue, `{"id":"WEB-1","input":{"dueDate":"2026-12-01T00:00:00Z"}}`, "is not a TimelessDate"},
		{"not a time", opUpdateIssue, `{"id":"WEB-1","input":{"snoozedUntilAt":"2026-12-01"}}`, "is not a DateTime"},
	} {
		got := strings.Join(lineartest.VariableProblems(tc.op.text, tc.op.name, json.RawMessage(tc.vars)), "; ")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The fake refuses a request whose variables the schema refuses, even
// when a test expects exactly those variables.
func TestFakeRefusesInvalidVariables(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var mu sync.Mutex
	var got []string
	f.SetReport(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf(format, args...))
	})
	_, err := f.client(t).CreateRelation(t.Context(), CreateRelationInput{IssueID: "WEB-1", RelatedIssueID: "WEB-2", Type: "bogus"})
	if err == nil {
		t.Fatal("the fake answered invalid variables")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "bogus is not a value of IssueRelationType") {
		t.Errorf("reports %q", got)
	}
}
