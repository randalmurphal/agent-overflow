package sshsetup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type runnerFunc func(context.Context, Request, string, io.Reader, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, r Request, action string, in io.Reader, out, err io.Writer) error {
	return f(ctx, r, action, in, out, err)
}

var testRequest = Request{Target: "gpu", Binary: "/opt/Agent Overflow/bin/ao"}

func awaitState(t *testing.T, m *Manager, id, want string) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %s, got %+v", want, status)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestConsoleRequiresMatchingNumberAndRetainsCompletedResult(t *testing.T) {
	m := New(runnerFunc(func(ctx context.Context, _ Request, action string, in io.Reader, out, stderr io.Writer) error {
		if action == "start" {
			t.Error("unexpected start")
			return nil
		}
		fmt.Fprintln(out, `{"type":"invitation","data":{"url":"https://gpu.test/#pair=secret"}}`)
		fmt.Fprintln(out, `{"type":"verification","data":{"verificationNumber":"123456"}}`)
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil {
			return err
		}
		if line != "123456\n" {
			t.Errorf("forwarded %q", line)
		}
		fmt.Fprintln(out, `{"type":"paired"}`)
		return nil
	}))
	t.Cleanup(m.Close)
	status, err := m.Begin(t.Context(), testRequest)
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, m, status.ID, "verification")
	if err := m.Confirm(t.Context(), status.ID, "000000"); err == nil {
		t.Fatal("accepted mismatch")
	}
	if err := m.Confirm(t.Context(), status.ID, "123456"); err != nil {
		t.Fatal(err)
	}
	got := awaitState(t, m, status.ID, "connected")
	if got.Invitation != "" || got.VerificationNumber != "" {
		t.Fatal("retained completed pairing secrets")
	}
	next, err := m.Begin(t.Context(), testRequest)
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(next.ID)
	if _, err := m.Get(status.ID); err != nil {
		t.Fatal("a second setup hid the first result")
	}
}
func TestCancelAndShutdownReleaseRunnersAndBoundConfirm(t *testing.T) {
	stopped := make(chan struct{}, 1)
	m := New(runnerFunc(func(ctx context.Context, _ Request, _ string, _ io.Reader, out, _ io.Writer) error {
		fmt.Fprintln(out, `{"type":"invitation","data":{"url":"https://gpu.test/#pair=secret"}}`)
		fmt.Fprintln(out, `{"type":"verification","data":{"verificationNumber":"123456"}}`)
		<-ctx.Done()
		stopped <- struct{}{}
		return ctx.Err()
	}))
	t.Cleanup(m.Close)
	status, err := m.Begin(t.Context(), testRequest)
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, m, status.ID, "verification")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Confirm(ctx, status.ID, "123456"); !errors.Is(err, context.Canceled) {
		t.Fatalf("confirm: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("runner survived cancellation")
	}
	awaitState(t, m, status.ID, "canceled")
	m.Close()
	if _, err := m.Begin(t.Context(), testRequest); err == nil {
		t.Fatal("began after shutdown")
	}
}
func TestMalformedStreamAndStartFailureAreVisible(t *testing.T) {
	for _, tc := range []struct {
		line  string
		start bool
	}{
		{line: `{"type":"paired"}`},
		{line: `{"type":"invitation","data":{"url":"x"}}` + "\n" + `{"type":"verification","data":{"verificationNumber":"ABCDEF"}}`},
		{line: strings.Repeat("x", 17000)},
		{start: true},
	} {
		m := New(runnerFunc(func(_ context.Context, _ Request, action string, _ io.Reader, out, stderr io.Writer) error {
			if action == "start" {
				fmt.Fprint(stderr, "no service is installed")
				return errors.New("exit 1")
			}
			_, err := fmt.Fprintln(out, tc.line)
			return err
		}))
		request := testRequest
		request.StartService = tc.start
		status, err := m.Begin(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		got := awaitState(t, m, status.ID, "error")
		if got.Error == "" || got.Invitation != "" || len(got.Error) > 5000 {
			t.Fatalf("bad failure %+v", got)
		}
		if tc.start && !strings.Contains(got.Error, "no service") {
			t.Fatal(got.Error)
		}
		m.Close()
	}
}
func TestArgumentsQuoteRemotePathAndNeverLoosenHostChecks(t *testing.T) {
	request := testRequest
	request.Binary = "/opt/it's $(not-a-command)/ao"
	args, err := Arguments(request, "pair")
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "'/opt/it'\\''s $(not-a-command)/ao' pair --json --class desktop --wait 30s" {
		t.Fatal(got)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=yes") || !strings.Contains(joined, "BatchMode=yes") {
		t.Fatal(joined)
	}
	for _, target := range []string{"-oProxyCommand=bad", "host;touch /tmp/no", "host\narg", "user@host argument"} {
		request.Target = target
		if _, err := Arguments(request, "pair"); err == nil {
			t.Fatalf("accepted %q", target)
		}
	}
	if err := (OSRunner{}).Run(t.Context(), testRequest, "pair", nil, io.Discard, io.Discard); err == nil {
		t.Fatal("test reached real SSH")
	}
}
