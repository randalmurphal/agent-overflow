package git

import (
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/git/forgeschema"
)

// allowedDeprecatedGraphQL are the deprecated schema elements a document
// may select, keyed by forge and element, each with why. GitLab marks an
// experiment deprecated with a reason starting "Status: Experiment"; no
// other deprecation is allowed.
var allowedDeprecatedGraphQL = map[string]string{
	"gitlab MergeRequest.mergeabilityChecks": "the Details read's change-request check (PRManageChecks); GitLab has no stable field for it",
}

// Every GraphQL document this package can send is valid against its
// forge's schema (forgeschema, refreshed by `make forge-schemas`) and
// selects nothing the schema deprecates. Client.GraphQL sends only a
// forgeapi.Document, whose constructor registers it, so the registry is
// every document; the package's are package-level values, made at init.
func TestGraphQLDocumentsMatchForgeSchemas(t *testing.T) {
	t.Parallel()
	docs := forgeapi.Documents()
	allowed := map[string]bool{}
	for _, doc := range docs {
		name := doc.Forge() + " " + doc.Operation()
		// The constructor's reading of the operation agrees with a parser's.
		if op := forgeschema.OperationName(doc.Text()); op != doc.Operation() {
			t.Errorf("%s: a GraphQL parser reads operation %q", name, op)
		}
		problems, deprecated, err := forgeschema.Check(doc.Forge(), doc.Text())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Errorf("%s: %s", name, p)
		}
		for element, reason := range deprecated {
			key := doc.Forge() + " " + element
			if _, ok := allowedDeprecatedGraphQL[key]; ok && strings.HasPrefix(reason, "Status: Experiment") {
				allowed[key] = true
				continue
			}
			t.Errorf("%s selects %s, which the schema deprecates (%s): use its replacement", name, element, reason)
		}
	}
	for key := range allowedDeprecatedGraphQL {
		if !allowed[key] {
			t.Errorf("allowedDeprecatedGraphQL lists %s, which no document selects as an experiment", key)
		}
	}
	t.Logf("%d documents", len(docs))
	// The registry holds the constant documents and the built ones.
	for _, want := range []string{"github PRTick", "github MergePRMessage", "gitlab PRSetDraft", "gitlab PRMergeMessage", "github PRMergeMessage", "gitlab PullsAuthoredApprovals"} {
		if !strings.Contains(registered(docs), "\n"+want+"\n") {
			t.Errorf("the registry lacks %s", want)
		}
	}
}

func registered(docs []forgeapi.Document) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, d := range docs {
		b.WriteString(d.Forge() + " " + d.Operation() + "\n")
	}
	return b.String()
}
