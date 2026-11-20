// Command initcost links the trackerapp package and nothing else of this
// module beyond what it imports, so GODEBUG=inittrace=1 reports the
// package's own init cost, free of test variables (TestInitCost). It
// reaches the service behind a condition the linker cannot decide, as the
// app does: with no use of it the linker drops the package variables only
// it uses, and an initializer among those would not show. A method looked
// up by a name known only at run time keeps every exported method of the
// service linked.
package main

import (
	"context"
	"os"
	"reflect"
	"time"

	"agent-overflow/internal/trackerapp"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	svc, err := trackerapp.New(trackerapp.Deps{
		Changed:        func() {},
		CredentialsDir: os.Args[1],
		Origin:         trackerapp.APIOrigin,
		Version:        os.Args[1],
		Now:            time.Now,
		Life:           context.Background(),
	})
	if err != nil {
		os.Exit(1)
	}
	defer svc.Stop()
	if !reflect.ValueOf(svc).MethodByName(os.Args[1]).IsValid() {
		os.Exit(1)
	}
}
