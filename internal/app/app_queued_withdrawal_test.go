package app

import (
	"slices"
	"testing"
)

// Every root the app writes a thread's messages under has an owner its
// withdrawal goes back through, and the owners name no root the app does
// not write under: a root added to appSendIDRoots without an owner fails
// here, as one a withdrawal would leave queued.
func TestEverySendIDRootHasAQueuedOwner(t *testing.T) {
	t.Parallel()
	a := &App{}
	for _, root := range appSendIDRoots {
		sendID := root + "computer:request"
		if kind := queuedKindOf(sendID); kind == queuedUnowned {
			t.Errorf("the send id root %q has no owner in queuedKindRoots", root)
			continue
		}
		if _, ok := a.queuedMessageOwner("thread", sendID, queuedWithdrawal{byPerson: true}); !ok {
			t.Errorf("a person cannot withdraw a message under %q", root)
		}
	}
	for _, entry := range queuedKindRoots {
		if !slices.Contains(appSendIDRoots, entry.root) {
			t.Errorf("queuedKindRoots names %q, which appSendIDRoots does not", entry.root)
		}
	}
	if _, ok := a.queuedMessageOwner("thread", "person-send", queuedWithdrawal{why: "archived"}); ok {
		t.Error("an app withdrawal takes a person's own message")
	}
}
