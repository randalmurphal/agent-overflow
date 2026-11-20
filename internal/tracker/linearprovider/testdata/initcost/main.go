// Command initcost links the linearprovider package and nothing else of
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
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linearprovider"
	"agent-overflow/internal/tracker/trackerapi"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	c, err := linear.New(linear.Options{
		BaseURL: os.Args[1], Version: "0", Workspace: os.Args[1],
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte(os.Args[1])))),
	})
	if err != nil {
		os.Exit(1)
	}
	defer c.Close()
	p, err := linearprovider.New(c)
	if err != nil {
		os.Exit(1)
	}
	if !reflect.ValueOf(p).MethodByName(os.Args[1]).IsValid() {
		os.Exit(1)
	}
}
