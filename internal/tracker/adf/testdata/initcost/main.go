// Command initcost links the adf package and nothing else of this module,
// so GODEBUG=inittrace=1 reports the package's own init cost, free of
// test variables (TestInitCost). It calls the package's entry points
// behind a condition the linker cannot decide, as the app does: with no
// call to them the linker drops the package variables only they use, and
// an initializer among those would not show.
package main

import (
	"context"
	"os"

	"agent-overflow/internal/tracker/adf"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	ctx := context.Background()
	doc, ws, err := adf.FromMarkdown(ctx, os.Args[1], nil)
	if err != nil || adf.CheckLoss(ws, false) != nil || adf.Validate(doc) != nil {
		os.Exit(1)
	}
	edited, _, err := adf.Edit(ctx, doc, adf.ToMarkdown(doc), nil)
	if err != nil || !adf.Equal(doc, edited) || adf.Blank(edited) {
		os.Exit(1)
	}
	adf.ReplaceMediaIDs(edited, nil)
	_ = adf.PlainText(adf.Doc(edited.Content...))
}
