//go:build noremote

package app

import (
	"testing"

	"agent-overflow/internal/attachedbackends"
	"github.com/google/uuid"
)

func TestNoRemoteBuildDoesNotLookForOwnerWaits(t *testing.T) {
	f := newRequestFixture(t)
	f.seedRemoteRequest(t, uuid.NewString())
	f.app.backends = &attachedbackends.Manager{}
	before := f.app.store.ReadCount()
	if err := f.app.refreshOwnerPeers(); err != nil {
		t.Fatal(err)
	}
	if after := f.app.store.ReadCount(); after != before {
		t.Fatalf("disabled remote access examined owner work: %d reads", after-before)
	}
}
