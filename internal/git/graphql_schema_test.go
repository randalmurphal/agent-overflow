package git

import (
	"strings"
	"testing"

	"agent-overflow/internal/git/forgeschema"
)

// Every GraphQL document this package can send is valid against its
// forge's schema (forgeschema, refreshed by `make forge-schemas`),
// selects nothing the schema deprecates, and defines the operation its
// call site names. The documents are found where they are sent
// (forgeschema.Sends): every call of forgeapi.Client.GraphQL, its
// arguments traced to package-level constants or a table listed here; a
// call site it cannot trace fails the test.

// graphQLDocumentTables are the package-level tables a send ranges over
// for its operation and document, by name. Their documents are built at
// run time, so the test reads them from the values.
var graphQLDocumentTables = map[string]any{
	"gitlabPullsReads": gitlabPullsReads,
}

// allowedDeprecatedGraphQL are the deprecated schema elements a document
// may select, keyed by forge and element, each with why. GitLab marks an
// experiment deprecated with a reason starting "Status: Experiment"; no
// other deprecation is allowed.
var allowedDeprecatedGraphQL = map[string]string{
	"gitlab MergeRequest.mergeabilityChecks": "the Details read's change-request check (PRManageChecks); GitLab has no stable field for it",
}

func TestGraphQLDocumentsMatchForgeSchemas(t *testing.T) {
	t.Parallel()
	sends, sites, err := forgeschema.Sends(".", graphQLDocumentTables)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	checked := map[string]bool{}
	for _, send := range sends {
		if op := forgeschema.OperationName(send.Text); op != send.Operation {
			t.Errorf("%s sends %s as operation %q, but it defines %q", send.Site, send.Name, send.Operation, op)
		}
		if checked[send.Name] {
			continue
		}
		checked[send.Name] = true
		problems, deprecated, err := forgeschema.Check(send.Forge, send.Text)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range problems {
			t.Errorf("%s (%s, sent at %s): %s", send.Name, send.Forge, send.Site, p)
		}
		for element, reason := range deprecated {
			key := send.Forge + " " + element
			if _, ok := allowedDeprecatedGraphQL[key]; ok && strings.HasPrefix(reason, "Status: Experiment") {
				allowed[key] = true
				continue
			}
			t.Errorf("%s (%s) selects %s, which the schema deprecates (%s): use its replacement", send.Name, send.Forge, element, reason)
		}
	}
	for key := range allowedDeprecatedGraphQL {
		if !allowed[key] {
			t.Errorf("allowedDeprecatedGraphQL lists %s, which no document selects as an experiment", key)
		}
	}
	t.Logf("%d call sites send %d documents", sites, len(checked))
	// The trace reaches documents through locals, parameters, a function
	// literal's parameters, a generic helper and a table.
	for _, want := range []string{"githubPRTickQuery", "githubReopenPRMutation", "githubMergePRMessageMutation", "githubRemoveLabelsMutation", "gitlabSetDraftMutation", "gitlabPullsReads[5]"} {
		if !checked[want] {
			t.Errorf("the send trace missed %s", want)
		}
	}
}
