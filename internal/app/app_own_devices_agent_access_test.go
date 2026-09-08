package app

import (
	"testing"

	"agent-overflow/internal/deviceclient"
)

// TestOwnDeviceReconcileKeepsAgentAccessOptIn pins the regression the
// agent-computers e2e caught: an agent-command opt-in is a separate,
// explicit grant, and the automatic personal-device-group reconciliation
// must not clear it when it replaces an ordinary profile with a group
// session. The reset of an opt-in belongs only to an explicit user
// re-pairing (Manager.Add), never to own-device re-enrollment, which runs
// the same addLinkLocked primitive underneath.
func TestOwnDeviceReconcileKeepsAgentAccessOptIn(t *testing.T) {
	ownConnectionNetwork(t)
	a, b, c := ownConnectionBackend(t), ownConnectionBackend(t), ownConnectionBackend(t)

	// A holds an ordinary full grant to C and opts C in for agent commands,
	// exactly as the frontend's "Enable access" does after pairing.
	pairOwnConnection(t, c, a.app.backends, false)
	if err := a.app.backends.SetAgentAccess(c.id, true); err != nil {
		t.Fatalf("opt C in for agent commands: %v", err)
	}
	legacy, err := deviceclient.LoadSession(a.profile, c.id)
	if err != nil || legacy.OwnDevice {
		t.Fatalf("ordinary profile expected before enrollment: %+v %v", legacy, err)
	}

	// A and C both join B's personal group. A now learns, through B, that C
	// is a group member and upgrades its ordinary profile to a group session.
	pairOwnConnection(t, b, a.app.backends, true)
	pairOwnConnection(t, b, c.app.backends, true)
	for range 3 {
		for _, host := range []ownConnectionHost{a, c, b} {
			reconcileOwnConnection(t, host)
		}
	}

	upgraded, err := deviceclient.LoadSession(a.profile, c.id)
	if err != nil || !upgraded.OwnDevice || upgraded.SessionID == legacy.SessionID {
		t.Fatalf("group enrollment did not re-pair C's profile, so this test would not guard the reset: %+v %v", upgraded, err)
	}

	access, err := a.app.backends.AgentAccess()
	if err != nil {
		t.Fatalf("read agent access: %v", err)
	}
	if !access[c.id] {
		t.Fatalf("own-device re-enrollment wiped the explicit agent opt-in: %v", access)
	}
}
