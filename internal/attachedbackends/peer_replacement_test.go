package attachedbackends

import (
	"errors"
	"testing"

	"agent-overflow/internal/deviceclient"
)

func TestReplacingPairingAllowsManagerReadsDuringCarrierRetirement(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "re-pair", true: "retired profile"}[retired], func(t *testing.T) {
			const id = "remote"
			m, err := New(t.TempDir(), "Computer", "linux")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			leaving := make(chan struct{})
			held := &carrier{client: &deviceclient.Client{}, peers: peerConns{launch: "launch"}, stopRenewal: func() { close(leaving) }}
			m.carriers[id] = held
			if retired {
				held.client.Retire()
			}
			stop := errors.New("stop before redemption")
			m.SetLabelGetter(func() (string, error) { return "", stop })
			held.peers.mu.Lock()
			finished := make(chan error, 1)
			go func() {
				if retired {
					_, err := m.carrier(t.Context(), id)
					finished <- err
				} else {
					_, err := m.addLinkLocked(t.Context(), deviceclient.Link{BackendID: id})
					finished <- err
				}
			}()
			<-leaving
			if m.mu.TryLock() {
				m.mu.Unlock()
			} else {
				t.Error("manager locked while the old carrier waits for its observation to commit")
			}
			held.peers.mu.Unlock()
			if err := <-finished; !errors.Is(err, stop) && !errors.Is(err, deviceclient.ErrNoSession) {
				t.Fatal(err)
			}
		})
	}
}
