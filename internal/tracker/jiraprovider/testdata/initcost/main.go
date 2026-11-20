// Command initcost links the jiraprovider package and nothing else of
// this module beyond what it imports, so GODEBUG=inittrace=1 reports the
// package's own init cost, free of test variables (TestInitCost). It
// reaches the package's entry points behind a condition the linker cannot
// decide, as the app does: with no use of them the linker drops the
// package variables only they use, and an initializer among those would
// not show. A method looked up by a name known only at run time keeps
// every exported method of the provider linked.
package main

import (
	"os"
	"reflect"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jiraprovider"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	creds := trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(os.Args[1]))))
	jc, err := jira.New(jira.Options{BaseURL: os.Args[1], Version: "0", Credentials: creds})
	if err != nil {
		os.Exit(1)
	}
	defer jc.Close()
	tc, err := tempo.New(tempo.Options{BaseURL: os.Args[1], Version: "0", Credentials: creds, Issues: jiraprovider.TempoIssues(jc)})
	if err != nil {
		os.Exit(1)
	}
	defer tc.Close()
	p, err := jiraprovider.New(jiraprovider.Options{Jira: jc, Tempo: tc, Site: os.Args[1]})
	if err != nil {
		os.Exit(1)
	}
	if !reflect.ValueOf(p).MethodByName(os.Args[1]).IsValid() {
		os.Exit(1)
	}
}
