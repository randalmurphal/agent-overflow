package sessionimport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
)

func TestTransferReservationRejectsStaleImportAndScan(t *testing.T) {
	for _, provider := range []string{ProviderClaude, ProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			st := newTestStore(t)
			homes := newProviderHomes(t)
			homes.claudeLinearSession(t, claudeSessionA)
			homes.writeCodexIndex(t, codexThreadA)
			homes.codexLinearSession(t, codexThreadA)
			d := homes.deps(st)
			row := scanOne(t, d, provider)
			request := store.ThreadTransfer{ID: entityid.New(), ThreadID: "moved-thread", PeerBackendID: entityid.New(),
				Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)}
			if _, err := st.CreateThreadTransfer(request); err != nil {
				t.Fatal(err)
			}
			if err := st.BindThreadTransferSessions(request.ID, []store.TransferSession{{Provider: provider, Ref: row.SessionID}}); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"prepared", "committed", "complete"} {
				if _, err := st.AdvanceThreadTransfer(request.ID, phase, strings.Repeat("b", 64)); err != nil {
					t.Fatal(err)
				}
			}
			var moved *store.ThreadTransferError
			if _, err := ImportOne(context.Background(), d, row); !errors.As(err, &moved) || !moved.Moved {
				t.Fatalf("stale scan imported a moved session: %v", err)
			}
			projects, err := st.ListProjects()
			if err != nil || len(projects) != 0 {
				t.Fatalf("blocked import created project: %v %v", projects, err)
			}
			// The bulk manager resolves projects before starting ImportOne.
			// A stale cached scan must hit the same fence at that earlier seam.
			var progress []ProgressEvent
			manager := NewManager(ManagerConfig{
				ResolveDeps:  func() (Deps, error) { return d, nil },
				Scan:         func(context.Context, Deps, Filter) (ScanResult, error) { return ScanResult{Rows: []Row{row}}, nil },
				EmitProgress: func(frame ProgressEvent) { progress = append(progress, frame) },
			})
			manager.run(context.Background(), &managerRun{id: "stale-scan", total: 1}, []string{row.ID})
			if len(progress) != 2 || progress[0].Status != ImportStatusFailed || !progress[1].Done {
				t.Fatalf("bulk import did not surface retirement: %+v", progress)
			}
			projects, err = st.ListProjects()
			if err != nil || len(projects) != 0 {
				t.Fatalf("bulk import registered a retired session's project: %+v %v", projects, err)
			}
			if result := scanFixture(t, d, Filter{Provider: provider}); len(result.Rows) != 0 {
				t.Fatalf("retired session offered again: %v", result.Rows)
			}
		})
	}
}
