// Command initcost links the trackerapi package and nothing else of this
// module, so GODEBUG=inittrace=1 reports the package's own init cost,
// free of test variables (TestInitCost). It calls the package's entry
// points behind a condition the linker cannot decide, as the app does:
// with no call to them the linker drops the package variables only they
// use, and an initializer among those would not show.
package main

import (
	"context"
	"io"
	"os"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	opts := trackerapi.Options{
		Name:        "probe",
		BaseURL:     os.Args[1],
		Version:     "0",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(os.Args[1])))),
		Dialect:     trackerapi.PlainDialect{},
	}
	c, err := trackerapi.New(opts)
	if err != nil {
		os.Exit(1)
	}
	defer c.Close()
	peer, err := trackerapi.NewSharing(c, opts)
	if err != nil {
		os.Exit(1)
	}
	defer peer.Close()
	ctx := trackerapi.WithInteractive(context.Background())
	var out any
	resp, err := c.JSON(ctx, trackerapi.Request{Path: "a"}, &out)
	if trackerapi.WriteNotApplied(err) {
		os.Exit(1)
	}
	_ = resp.Scrub(os.Args[1])
	_, _ = c.Do(ctx, trackerapi.Request{Path: "a"})
	_, _ = c.Stream(ctx, trackerapi.Request{Path: "a", Transfer: true}, io.Discard, 1)
	_, _ = c.Location(ctx, trackerapi.Request{Path: "a"})
	c.ForgetCredential()
}
