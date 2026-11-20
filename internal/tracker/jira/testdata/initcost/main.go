// Command initcost links the jira package and nothing else of this
// module beyond what it imports, so GODEBUG=inittrace=1 reports the
// package's own init cost, free of test variables (TestInitCost). It
// reaches the package's entry points behind a condition the linker cannot
// decide, as the app does: with no use of them the linker drops the
// package variables only they use, and an initializer among those would
// not show. A method looked up by a name known only at run time keeps
// every exported method of the client and the catalog linked.
package main

import (
	"os"
	"reflect"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	c, err := jira.New(jira.Options{
		BaseURL: os.Args[1], Version: "0",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(os.Args[1])))),
	})
	if err != nil {
		os.Exit(1)
	}
	defer c.Close()
	cat := jira.NewCatalog(nil)
	if !reflect.ValueOf(c).MethodByName(os.Args[1]).IsValid() || !reflect.ValueOf(cat).MethodByName(os.Args[1]).IsValid() {
		os.Exit(1)
	}
	_, _ = jira.ParseSprintIDs(os.Args[1])
	_, _ = jira.ParseDate(os.Args[1])
	_ = jira.FieldChanges(nil, os.Args[1])
	_ = jira.IsIssueKey(os.Args[1])
	_ = jira.QuoteJQL(os.Args[1]) + jira.QuoteJQLText(os.Args[1]) + jira.QuoteJQLList(os.Args[1:]...)
	_, _ = jira.Text(os.Args[1]), jira.RawJSON(nil)
}
