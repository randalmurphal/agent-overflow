package trackerapp

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
)

func TestVerifyDuringDisableRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	arrived := make(chan struct{})
	f.jira.On("GET", myselfPath, blocked(arrived))
	done := make(chan Connection)
	errs := make(chan error)
	go func() {
		got, err := f.svc.Verify(context.Background(), conn.ID)
		done <- got
		errs <- err
	}()
	<-arrived
	if _, err := f.svc.SetEnabled(conn.ID, false); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if err := <-errs; err != nil {
		t.Fatalf("Verify ended by a disable = %v", err)
	}
	if got.Enabled || got.Problem != nil || got.VerifiedAt != 0 || f.row(conn.ID).VerifiedAt != 0 {
		t.Fatalf("Verify ended by a disable = %+v, problem %v", got.TrackerConnection, got.Problem)
	}
	if f.inUse(conn.ID) {
		t.Fatal("the disabled connection has a client")
	}
}

func TestVerifyDuringRemoveIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.writeToken(conn, linearKey)
	arrived := make(chan struct{})
	f.linear.Expect(lineartest.Exchange{Op: "Viewer", Wait: true, Arrived: arrived})
	errs := make(chan error)
	go func() {
		_, err := f.svc.Verify(context.Background(), conn.ID)
		errs <- err
	}()
	<-arrived
	if err := f.svc.Remove(conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; !isNotFound(err) {
		t.Fatalf("Verify ended by a remove = %v, want ErrNotFound", err)
	}
}

func TestStopEndsAVerifyInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addJira()
	f.writeToken(conn, jiraToken)
	arrived := make(chan struct{})
	f.jira.On("GET", myselfPath, blocked(arrived))
	errs := make(chan error, 1)
	go func() {
		_, err := f.svc.Verify(context.Background(), conn.ID)
		errs <- err
	}()
	<-arrived
	f.svc.Stop()
	if err := <-errs; !errors.Is(err, ErrStopped) {
		t.Fatalf("Verify ended by Stop = %v", err)
	}
	if f.row(conn.ID).VerifiedAt != 0 {
		t.Fatal("a Verify Stop ended recorded a verification")
	}
}

func TestCallsAfterStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.addLinear()
	f.svc.Stop()
	f.svc.Stop()
	for name, call := range map[string]func() error{
		"Add":            func() error { _, err := f.svc.Add(NewConnection{Kind: "linear", Site: "other"}); return err },
		"Connections":    func() error { _, err := f.svc.Connections(); return err },
		"Rename":         func() error { _, err := f.svc.Rename(conn.ID, "x"); return err },
		"SetTokenExpiry": func() error { _, err := f.svc.SetTokenExpiry(conn.ID, 1); return err },
		"SetEnabled":     func() error { _, err := f.svc.SetEnabled(conn.ID, false); return err },
		"Verify":         func() error { _, err := f.svc.Verify(t.Context(), conn.ID); return err },
		"Remove":         func() error { return f.svc.Remove(conn.ID) },
		"PrepareKeyFile": func() error { _, err := f.svc.PrepareKeyFile(conn.ID); return err },
		"TokenPage":      func() error { _, err := f.svc.TokenPage(conn.ID); return err },
		"Provider":       func() error { _, err := f.svc.Provider(t.Context(), conn.ID); return err },
	} {
		if err := call(); !errors.Is(err, ErrStopped) {
			t.Errorf("%s after Stop = %v", name, err)
		}
	}
	if row := f.row(conn.ID); row.Label != "" || !row.Enabled {
		t.Fatalf("a call after Stop changed the row: %+v", row)
	}
}
