package forgeschema

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The shapes the forges' validation refuses and GitHub's deprecations
// are reported: the conflicting selections internal/git once sent and a
// deprecated field.
func TestCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, forge, doc    string
		problem, deprecated string
	}{
		{name: "a valid document", forge: "github", doc: `query Q { viewer { login name } }`},
		{name: "the old CommentOwner: pullRequest is PullRequest! and PullRequest", forge: "github",
			doc:     `query CommentOwner($id: ID!) { node(id: $id) { __typename ... on PullRequestReviewComment { pullRequest { number } } ... on IssueComment { pullRequest { number } } } }`,
			problem: `Fields "pullRequest" conflict because they return conflicting types "PullRequest!" and "PullRequest"`},
		{name: "the old PRTick reviewer: name is String and String!", forge: "github",
			doc:     `query Q($id: ID!) { node(id: $id) { ... on ReviewRequest { requestedReviewer { ... on User { name } ... on Team { name } } } } }`,
			problem: `Fields "name" conflict because they return conflicting types "String" and "String!"`},
		{name: "a field the type lacks", forge: "gitlab", doc: `query Q { currentUser { nope } }`, problem: `Cannot query field "nope" on type "CurrentUser"`},
		{name: "a deprecated field", forge: "github",
			doc:        `query Q($id: ID!) { node(id: $id) { ... on PullRequestReview { databaseId } } }`,
			deprecated: "PullRequestReview.databaseId"},
		{name: "its replacement", forge: "github", doc: `query Q($id: ID!) { node(id: $id) { ... on PullRequestReview { fullDatabaseId } } }`},
	} {
		problems, deprecated, err := Check(tc.forge, tc.doc)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(problems, "; "); (tc.problem == "") != (got == "") || !strings.Contains(got, tc.problem) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.problem)
		}
		if _, ok := deprecated[tc.deprecated]; (tc.deprecated == "") != (len(deprecated) == 0) || (tc.deprecated != "" && !ok) {
			t.Errorf("%s: deprecated %v, want %q", tc.name, deprecated, tc.deprecated)
		}
	}
	if _, _, err := Check("bitbucket", `query Q { a }`); err == nil {
		t.Error("an unknown forge has a schema")
	}
}

// An answer member the document does not select on the object's
// concrete type, or a scalar or enum of the wrong kind, is a problem; an
// alias, an absent member, null and an abstract type's fragments chosen
// by __typename are not.
func TestAnswerProblems(t *testing.T) {
	t.Parallel()
	const owner = `query CommentOwner($id: ID!) { node(id: $id) { __typename ` +
		`... on PullRequestReviewComment { pullRequest { number repository { nameWithOwner } } } ` +
		`... on IssueComment { issuePullRequest: pullRequest { number } } } }`
	for _, tc := range []struct {
		name, doc, body string
		want            []string
	}{
		{name: "a review comment", doc: owner, body: `{"data":{"node":{"__typename":"PullRequestReviewComment","pullRequest":{"number":5,"repository":{"nameWithOwner":"o/r"}}}}}`},
		{name: "an issue comment under its alias", doc: owner, body: `{"data":{"node":{"__typename":"IssueComment","issuePullRequest":null}}}`},
		{name: "an issue comment under the review comment's name", doc: owner, body: `{"data":{"node":{"__typename":"IssueComment","pullRequest":{"number":5}}}}`,
			want: []string{"data.node.pullRequest: not selected on IssueComment"}},
		{name: "a member nothing selects", doc: owner, body: `{"data":{"node":{"__typename":"PullRequestReviewComment","pullRequest":{"number":5,"title":"t"}}}}`,
			want: []string{"data.node.pullRequest.title: not selected on PullRequest"}},
		{name: "a number where a string goes", doc: owner, body: `{"data":{"node":{"__typename":"PullRequestReviewComment","pullRequest":{"number":"5","repository":{"nameWithOwner":7}}}}}`,
			want: []string{"data.node.pullRequest.number: a Int is a JSON string", "data.node.pullRequest.repository.nameWithOwner: a String is a JSON float64"}},
		{name: "a type that is no Node", doc: owner, body: `{"data":{"node":{"__typename":"Nope"}}}`, want: []string{`data.node: __typename "Nope" is no Node`}},
		{name: "an enum value the schema lacks", doc: `query Q($o: String!, $n: String!) { repository(owner: $o, name: $n) { pullRequest(number: 1) { state } } }`,
			body: `{"data":{"repository":{"pullRequest":{"state":"open"}}}}`, want: []string{"data.repository.pullRequest.state: open is no PullRequestState value"}},
		{name: "a list", doc: `query Q($o: String!, $n: String!) { repository(owner: $o, name: $n) { labels(first: 1) { nodes { name } } } }`,
			body: `{"data":{"repository":{"labels":{"nodes":[{"name":"bug"},{"color":"red"}]}}}}`, want: []string{"data.repository.labels.nodes[1].color: not selected on Label"}},
		{name: "null data beside errors", doc: owner, body: `{"data":null,"errors":[{"message":"x"}]}`},
		{name: "a body cut short", doc: owner, body: `{"data":{"node":`},
	} {
		got, err := AnswerProblems("github", tc.doc, []byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: problems %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := AnswerProblems("github", `query Q { x }`, []byte(`{"data":{"x":1}}`)); !errors.Is(err, ErrInvalidDocument) {
		t.Errorf("an invalid document: %v", err)
	}
}
