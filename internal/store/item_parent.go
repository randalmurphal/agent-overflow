package store

import (
	"fmt"
	"slices"
)

// ItemParentLink is what the parent rule reads of a row on a parent chain.
type ItemParentLink struct {
	Kind     string
	ParentID string
}

// itemParentChainLimit bounds the parent chain the rule walks.
const itemParentChainLimit = 16

// ItemParentProblem reports why parentID cannot parent itemID, or "" when
// it can. The parent_id invariant: every row up the chain is a tool_call,
// and the chain neither returns to the row nor runs past
// itemParentChainLimit hops. A row lookup does not find ends the chain
// without a problem, since it may be written later (a subagent's text can
// arrive before its Task row). lookup returns a row's link and whether it
// is stored.
func ItemParentProblem(itemID, parentID string, lookup func(id string) (ItemParentLink, bool, error)) (string, error) {
	if parentID == itemID {
		return "self reference", nil
	}
	var seenBuf [4]string
	seen := append(seenBuf[:0], itemID)
	current := parentID
	for hops := 0; hops < itemParentChainLimit; hops++ {
		if slices.Contains(seen, current) {
			return "cycle detected", nil
		}
		seen = append(seen, current)
		link, found, err := lookup(current)
		if err != nil || !found {
			return "", err
		}
		if link.Kind != "tool_call" {
			return fmt.Sprintf("parent kind %q is not tool_call", link.Kind), nil
		}
		if link.ParentID == "" {
			return "", nil
		}
		current = link.ParentID
	}
	return "parent chain too deep", nil
}
