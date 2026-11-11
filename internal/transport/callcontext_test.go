package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// scopedCallApp answers two /rpc methods by their generated names:
// WorkflowAgentStartRun is not caller-bound, WorkflowAgentRunStatus is.
type scopedCallApp struct {
	entered chan string
	release chan struct{}
	done    chan error
}

func (a *scopedCallApp) WorkflowAgentStartRun(ctx context.Context) error {
	a.entered <- "start"
	<-a.release
	a.done <- ctx.Err()
	return nil
}

func (a *scopedCallApp) WorkflowAgentRunStatus(ctx context.Context) error {
	a.entered <- "status"
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
	a.done <- ctx.Err()
	return ctx.Err()
}

func newScopedCallServer(t *testing.T) (*Server, *scopedCallApp) {
	t.Helper()
	app := &scopedCallApp{entered: make(chan string, 4), release: make(chan struct{}), done: make(chan error, 4)}
	t.Cleanup(func() {
		select {
		case <-app.release:
		default:
			close(app.release)
		}
	})
	registry := newTokenRegistry()
	registry.put("chat-token", CallerScope{Kind: ScopeKindInteractive, ThreadID: "chat", ProjectID: "project"})
	dispatcher := NewDispatcher()
	if _, err := dispatcher.Register(app, RegisterOptions{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	server, err := New(Config{Dispatcher: dispatcher, EventBus: NewEventBus(DefaultRingCapacity), ScopedTokens: registry})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return server, app
}

// postAndAbandon sends one /rpc call and abandons the request once the
// method is running.
func postAndAbandon(t *testing.T, server *Server, app *scopedCallApp, method, want string) {
	t.Helper()
	body, err := json.Marshal(ClientFrame{Type: "rpc", ID: "1", Method: method})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+server.Addr()+ScopedRPCPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer chat-token")
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case got := <-app.entered:
		if got != want {
			t.Fatalf("entered %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never started", method)
	}
	cancel()
	<-sent
}

func TestScopedRPCRunsAnUnboundMethodPastItsCaller(t *testing.T) {
	server, app := newScopedCallServer(t)
	postAndAbandon(t, server, app, "WorkflowAgentStartRun", "start")
	time.Sleep(50 * time.Millisecond)
	close(app.release)
	select {
	case err := <-app.done:
		if err != nil {
			t.Fatalf("method context after its caller left = %v, want live", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("method never finished")
	}
}

func TestScopedRPCEndsACallerBoundMethodWithItsCaller(t *testing.T) {
	server, app := newScopedCallServer(t)
	postAndAbandon(t, server, app, "WorkflowAgentRunStatus", "status")
	select {
	case err := <-app.done:
		if err == nil {
			t.Fatal("caller-bound method ran past its caller")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller-bound method did not end with its caller")
	}
}

// Shutdown still ends a method that is not caller-bound.
func TestServerShutdownEndsAnUnboundMethod(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	caller := withServerLifetime(context.Background(), lifetime)
	ctx := methodContext(caller, &Method{Name: "Unbound"})
	derived, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	stop()
	select {
	case <-derived.Done():
	case <-time.After(time.Second):
		t.Fatal("derived context outlived server shutdown")
	}
	if ctx.Err() == nil {
		t.Fatal("method context outlived server shutdown")
	}
}
