package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/git/forgeschema"
	"agent-overflow/internal/harness/forgefake"
)

// Every GraphQL request a test's fake forge receives here is validated
// as the forges validate it. cmd/ao-mockforge's TestMain holds the
// package to every operation: each one its source defines must reach the
// validating fake engine through the app's own code.

// checkGraphQLRequest validates one GraphQL request a test fake received:
// its document names operation, and GraphQLRequestProblems finds nothing
// (each variable used is declared, each declared one used, each non-null
// one without a default sent, nothing sent undeclared). A failure fails t
// and returns the errors body the forge would answer.
func checkGraphQLRequest(t testing.TB, operation, query string, vars map[string]any) (string, bool) {
	problems := forgefake.GraphQLRequestProblems(query, vars)
	if op, err := forgefake.ParseGraphQLOperation(query); err == nil && op.Name != operation {
		problems = append(problems, forgefake.GraphQLProblem{Code: "operationNotFound", Message: fmt.Sprintf("the document defines %q, not %q", op.Name, operation)})
	}
	if len(problems) == 0 {
		return "", true
	}
	errs := make([]map[string]any, 0, len(problems))
	for _, p := range problems {
		t.Errorf("GraphQL %s: %s", operation, p.Message)
		errs = append(errs, map[string]any{"message": p.Message, "extensions": map[string]any{"code": p.Code}})
	}
	body, err := json.Marshal(map[string]any{"errors": errs})
	if err != nil {
		t.Fatal(err)
	}
	return string(body), false
}

// checkGraphQLAnswer fails t on a test forge's 200 answer to a valid
// document that has members the document does not select, or a scalar
// of the wrong JSON kind (forgeschema.AnswerProblems), so a hand-written
// answer cannot drift from the document it answers. A document that is
// not valid against the schema is a test's stand-in, answered as the
// test wants; the app's own are held valid by
// TestGraphQLDocumentsMatchForgeSchemas.
func checkGraphQLAnswer(t testing.TB, path string, call forgeAPICall, reply forgeAPIAnswer) {
	if reply.Unshaped || (reply.Status != 0 && reply.Status != http.StatusOK) {
		return
	}
	forge := "github"
	if strings.HasPrefix(path, "/gitlab/") {
		forge = "gitlab"
	}
	problems, err := forgeschema.AnswerProblems(forge, call.Query, []byte(reply.Body))
	if errors.Is(err, forgeschema.ErrInvalidDocument) {
		return
	}
	if err != nil {
		t.Errorf("GraphQL %s answer: %v", call.Op, err)
	}
	for _, p := range problems {
		t.Errorf("GraphQL %s answer: %s", call.Op, p)
	}
}

// The checks fail on the shape of the ReactionTarget defect: a shared
// selection that uses a variable the operation does not declare.
func TestCheckGraphQLRequest(t *testing.T) {
	t.Parallel()
	recorder := &failureRecorder{TB: t}
	defective := `query ReactionTarget($id: ID!) { node(id: $id) { ... on IssueComment { ` + `reactionGroups @include(if: $wantReactions) { content } } } }`
	if body, ok := checkGraphQLRequest(recorder, "ReactionTarget", defective, map[string]any{"id": "IC_1"}); ok || !strings.Contains(body, "variableNotDefined") || len(recorder.errors) != 1 {
		t.Fatalf("the defect passed: %q %v", body, recorder.errors)
	}
	recorder.errors = nil
	if _, ok := checkGraphQLRequest(recorder, "Other", `query ReactionTarget($id: ID!) { node(id: $id) { id } }`, map[string]any{"id": "IC_1"}); ok || len(recorder.errors) != 1 {
		t.Fatalf("another operation's name passed: %v", recorder.errors)
	}
}

// failureRecorder records Errorf instead of failing.
type failureRecorder struct {
	testing.TB
	errors []string
}

func (r *failureRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
