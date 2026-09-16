package app

import (
	"context"
	"testing"
)

func TestVerifyBrowserUnlockRequiresSessionAndFreshProof(t *testing.T) {
	a := newTestAppWithStore(t)
	a.initIdentity("browser-lock-test")
	for _, ctx := range []context.Context{context.Background(), callFrom("session", false), callFrom("session", true), callSteppedUp("session")} {
		if err := a.VerifyBrowserUnlock(ctx, "missing-ceremony", nil); err == nil {
			t.Fatal("browser unlock accepted a caller without a session and fresh proof")
		}
	}
}
