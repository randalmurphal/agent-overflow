package wsllauncher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/selfupdate"

	"github.com/coder/websocket"
)

func TestClassifyInstallAck(t *testing.T) {
	refused := &RPCRefusedError{
		Method:  selfupdate.RPCReportStatus,
		Code:    "method_error",
		Message: "no install in flight",
	}
	cases := []struct {
		name string
		err  error
		want InstallAckOutcome
	}{
		{"accepted", nil, InstallAckAccepted},
		{"refused", refused, InstallAckRefused},
		{"refused through a wrap", fmt.Errorf("acknowledge install: %w", refused), InstallAckRefused},
		{"timed out", fmt.Errorf("%s RPC: %w", selfupdate.RPCReportStatus, context.DeadlineExceeded), InstallAckUndelivered},
		{"disconnected", fmt.Errorf("%w: write RPC", ErrNotificationBridgeDisconnected), InstallAckUndelivered},
		{"no connection", errors.New("wait for notification bridge connection: context canceled"), InstallAckUndelivered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyInstallAck(tc.err); got != tc.want {
				t.Fatalf("ClassifyInstallAck(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestInstallAckOutcomeString(t *testing.T) {
	cases := map[InstallAckOutcome]string{
		InstallAckAccepted:    "accepted",
		InstallAckRefused:     "refused",
		InstallAckUndelivered: "undelivered",
		InstallAckOutcome(99): "unknown",
	}
	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Fatalf("InstallAckOutcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
}

// TestReportUpdateInstallStatusTimeoutIsNotRefused pins the ambiguous case: an
// unanswered call must never look like a rejection, because the report may have
// landed with only its response lost.
func TestReportUpdateInstallStatusTimeoutIsNotRefused(t *testing.T) {
	received := make(chan struct{}, 1)
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, _ int) error {
		if err := expectSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		frame, err := readClientFrame(ctx, conn)
		if err != nil {
			return nil
		}
		if frame.Type != "rpc" {
			return fmt.Errorf("frame type = %q, want rpc", frame.Type)
		}
		// Deliberately never answer: hold the connection open so the caller
		// times out instead of seeing a disconnect.
		received <- struct{}{}
		<-ctx.Done()
		return nil
	})

	client, _ := newTestBridgeClient(t, wsURL, func(selfupdate.InstallDirective) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	callCtx, callCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer callCancel()
	err := client.ReportUpdateInstallStatus(callCtx, selfupdate.StatusProceeding, "0.0.11", "")
	if err == nil {
		t.Fatal("ReportUpdateInstallStatus = nil, want a timeout")
	}
	var refused *RPCRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("timeout surfaced as a refusal: %#v", refused)
	}
	if got := ClassifyInstallAck(err); got != InstallAckUndelivered {
		t.Fatalf("ClassifyInstallAck(%v) = %v, want %v", err, got, InstallAckUndelivered)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("stub server never received the status RPC")
	}
}

// TestReportUpdateInstallStatusDisconnectIsNotRefused covers the other
// ambiguous shape: the connection drops with the call in flight.
func TestReportUpdateInstallStatusDisconnectIsNotRefused(t *testing.T) {
	wsURL := startBridgeStub(t, func(ctx context.Context, conn *websocket.Conn, connection int) error {
		if err := expectSubscribeAndReplay(ctx, conn); err != nil {
			return err
		}
		if connection > 1 {
			// The client reconnects after the drop; leave later connections
			// idle so the assertion below reads the first call's result.
			<-ctx.Done()
			return nil
		}
		if _, err := readClientFrame(ctx, conn); err != nil {
			return nil
		}
		// Drop the connection with the call in flight.
		return nil
	})

	client, _ := newTestBridgeClient(t, wsURL, func(selfupdate.InstallDirective) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer callCancel()
	err := client.ReportUpdateInstallStatus(callCtx, selfupdate.StatusProceeding, "0.0.11", "")
	if err == nil {
		t.Fatal("ReportUpdateInstallStatus = nil, want a disconnect")
	}
	if !errors.Is(err, ErrNotificationBridgeDisconnected) {
		t.Fatalf("error = %v, want it to wrap ErrNotificationBridgeDisconnected", err)
	}
	var refused *RPCRefusedError
	if errors.As(err, &refused) {
		t.Fatalf("disconnect surfaced as a refusal: %#v", refused)
	}
	if got := ClassifyInstallAck(err); got != InstallAckUndelivered {
		t.Fatalf("ClassifyInstallAck(%v) = %v, want %v", err, got, InstallAckUndelivered)
	}
}
