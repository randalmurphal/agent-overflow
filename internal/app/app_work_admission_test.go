package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/supervise"
)

func TestAccountManagerSharesTheRestartFence(t *testing.T) {
	a := newTestAppWithStore(t)
	manager := a.ensureProviderAccountManager()
	_, _ = a.workAdmission.quiesce(func() (string, error) { return "", nil })
	a.workAdmission.stopWaiting()
	_, err := manager.RunAccountProbe(provideraccountapp.ProbeRequest{
		ProviderName: "claude",
		Probe: func(context.Context) (provider.AccountInfo, error) {
			t.Fatal("account probe started after update handoff")
			return provider.AccountInfo{}, nil
		},
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("account manager bypassed the host restart fence: %v", err)
	}
}

func TestWorkAdmissionHandoffAndCancellation(t *testing.T) {
	var gate workAdmission
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := gate.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := gate.quiesce(func() (string, error) {
		t.Fatal("checked idle state during admission")
		return "", nil
	}); err != nil || reason == "" {
		t.Fatalf("active admission: %q, %v", reason, err)
	}
	// Nested admissions cannot deadlock behind a waiting updater.
	nested, err := gate.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nested()
	release()
	if reason, err := gate.quiesce(func() (string, error) { return "", nil }); err != nil || reason != "" {
		t.Fatalf("quiesce: %q, %v", reason, err)
	}
	defer gate.reopen()
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := gate.begin(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}
	entered := make(chan struct{})
	go func() {
		release, err := gate.begin(ctx)
		if err != nil {
			return
		}
		defer release()
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("new work crossed the restart fence")
	case <-time.After(20 * time.Millisecond):
	}
	gate.reopen()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("canceled update did not reopen admission")
	}
}

func TestUnconfirmedSupervisorUpdateKeepsAdmissionClosed(t *testing.T) {
	rig := newServiceUpdateRig(t, serviceUpdateOptions{configure: true, supervised: true})
	rig.requestErr = supervise.ErrUpdateOutcomeUnknown
	if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
		t.Fatal(err)
	}
	status := rig.settled()
	if status.Phase != serviceUpdatePhaseRequested || status.Error == "" {
		t.Fatalf("unconfirmed: %+v", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if release, err := rig.app.workAdmission.begin(ctx); err == nil {
		release()
		t.Fatal("unconfirmed handoff reopened work admission")
	}
	if err := rig.app.CancelServiceUpdate(); err == nil {
		t.Fatal("unconfirmed handoff advertised cancellation")
	}
}

func TestUpdateAdmissionWaitersReleaseBeforeTransportDrain(t *testing.T) {
	var gate workAdmission
	_, _ = gate.quiesce(func() (string, error) { return "", nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		release, err := gate.begin(ctx)
		if err == nil {
			release()
		}
		result <- err
	}()
	gate.stopWaiting()
	select {
	case err := <-result:
		if !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("shutdown wait: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("transport drain would wait for app cancellation")
	}
}

func TestWorkAdmissionIdleCheckIsAtomicWithNewWork(t *testing.T) {
	var gate workAdmission
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	checking, checked := make(chan struct{}), make(chan struct{})
	proceed := make(chan struct{})
	go func() {
		_, _ = gate.quiesce(func() (string, error) {
			close(checking)
			<-proceed
			return "", nil
		})
		close(checked)
	}()
	<-checking
	entered := make(chan struct{})
	go func() {
		release, err := gate.begin(ctx)
		if err == nil {
			release()
			close(entered)
		}
	}()
	close(proceed)
	<-checked
	select {
	case <-entered:
		t.Fatal("admission slipped between idle check and fence")
	case <-time.After(20 * time.Millisecond):
	}
	gate.reopen()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestServiceUpdateWaitsForWorkAndCanBeCanceled(t *testing.T) {
	for _, cancelUpdate := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish", true: "cancel"}[cancelUpdate], func(t *testing.T) {
			rig := newServiceUpdateRig(t, serviceUpdateOptions{configure: true, supervised: true})
			release, err := rig.app.workAdmission.begin(rig.app.lifeCtx())
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.app.RequestServiceUpdate(context.Background(), "v1.5.0"); err != nil {
				release()
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				status, _ := rig.app.GetServiceUpdateStatus()
				if status.Phase == serviceUpdatePhaseWaiting {
					if !status.Cancelable || status.WaitingFor == "" {
						release()
						t.Fatalf("waiting status: %+v", status)
					}
					break
				}
				if time.Now().After(deadline) {
					release()
					t.Fatalf("did not wait: %+v", status)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if cancelUpdate {
				if err := rig.app.CancelServiceUpdate(); err != nil {
					release()
					t.Fatal(err)
				}
			}
			release()
			status := rig.settled()
			want := serviceUpdatePhaseRequested
			if cancelUpdate {
				want = serviceUpdatePhaseCanceled
			}
			if status.Phase != want || status.WaitingFor != "" {
				t.Fatalf("settled: %+v", status)
			}
			rig.mu.Lock()
			requests := len(rig.requested)
			rig.mu.Unlock()
			if cancelUpdate && requests != 0 {
				t.Fatal("canceled waiting update requested restart")
			}
		})
	}
}
