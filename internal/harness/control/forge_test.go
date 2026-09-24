package control

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startForgeServer(t *testing.T, forge func(ForgeCall) ForgeResult) *Server {
	t.Helper()
	srv, err := NewServer(ServerConfig{
		Resolve: func(Registration) (Assignment, error) { return Assignment{}, nil },
		Forge:   forge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

func TestForgeRoundTripCarriesBinaryBytes(t *testing.T) {
	var got ForgeCall
	srv := startForgeServer(t, func(call ForgeCall) ForgeResult {
		got = call
		return ForgeResult{Stdout: []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}, Stderr: "warn", ExitCode: 3}
	})
	client := clientFor(t, srv)
	result, err := client.Forge(ForgeCall{
		CLI:   "gh",
		Args:  []string{"api", "user", "--jq", ".login"},
		Cwd:   "/work",
		Stdin: []byte("{\"body\":\"x\"}"),
		PID:   42,
	})
	if err != nil {
		t.Fatalf("Forge: %v", err)
	}
	if !bytes.Equal(result.Stdout, []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}) || result.Stderr != "warn" || result.ExitCode != 3 {
		t.Fatalf("result = %+v", result)
	}
	if got.CLI != "gh" || strings.Join(got.Args, " ") != "api user --jq .login" || got.Cwd != "/work" ||
		string(got.Stdin) != "{\"body\":\"x\"}" || got.PID != 42 {
		t.Fatalf("server saw %+v", got)
	}
}

func TestForgeWithoutHandlerFailsLoudly(t *testing.T) {
	srv := startForgeServer(t, nil)
	_, err := clientFor(t, srv).Forge(ForgeCall{CLI: "glab", Args: []string{"api", "user"}})
	if err == nil || !strings.Contains(err.Error(), noForgeBody) {
		t.Fatalf("err = %v, want the no-fake-forge refusal", err)
	}
}

func TestForgeRequiresToken(t *testing.T) {
	called := false
	srv := startForgeServer(t, func(ForgeCall) ForgeResult { called = true; return ForgeResult{} })
	resp, err := http.Post("http://"+srv.Addr()+"/forge", "application/json", strings.NewReader(`{"cli":"gh"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || called {
		t.Fatalf("unauthenticated /forge: status %d, handler called %v", resp.StatusCode, called)
	}
}

func TestForgeRejectsUnknownFields(t *testing.T) {
	srv := startForgeServer(t, func(ForgeCall) ForgeResult { t.Error("handler ran on a malformed call"); return ForgeResult{} })
	req, _ := http.NewRequest(http.MethodPost, "http://"+srv.Addr()+"/forge", strings.NewReader(`{"cli":"gh","argv":["x"]}`))
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
