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
			dir := t.TempDir()
			m, err := New(dir, "Computer", "linux")
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
			held.peers.mu.Lock()
			finished := make(chan error, 1)
			go func() {
				if retired {
					_, err := m.carrier(t.Context(), id)
					finished <- err
				} else {
					// What a re-pairing's save does to the carrier it replaces.
					_, err := m.transact(id, func(tx *computerTx) error {
						tx.drop(errPairingReplaced)
						return nil
					})
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
			if err := <-finished; err != nil && !errors.Is(err, deviceclient.ErrNoSession) {
				t.Fatal(err)
			}
		})
	}
}
