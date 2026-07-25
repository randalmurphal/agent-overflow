package pprofserve

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDisabledWhenUnsetOrOff(t *testing.T) {
	for _, v := range []string{"", "0", "false", "FALSE"} {
		t.Run(fmt.Sprintf("value=%q", v), func(t *testing.T) {
			t.Setenv(EnvVar, v)
			addr, stop, err := StartIfEnabled()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addr != "" {
				stop()
				t.Fatalf("expected disabled, got listener on %s", addr)
			}
			stop() // must be safe to call when disabled
		})
	}
}

func TestRefusesNonLoopbackAndMalformed(t *testing.T) {
	for _, v := range []string{"0.0.0.0:6363", "192.168.1.4:6363", "example.com:6363", "not-an-addr"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvVar, v)
			addr, stop, err := StartIfEnabled()
			if err == nil {
				stop()
				t.Fatalf("expected refusal for %q, got listener on %s", v, addr)
			}
		})
	}
}

func TestServesProfilesOnLoopback(t *testing.T) {
	// Port 0 avoids collisions with a concurrently running backend that
	// has the default port bound.
	t.Setenv(EnvVar, "127.0.0.1:0")
	addr, stop, err := StartIfEnabled()
	if err != nil {
		t.Fatalf("StartIfEnabled: %v", err)
	}
	defer stop()
	if addr == "" {
		t.Fatal("expected a bound address")
	}

	resp, err := http.Get("http://" + addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		t.Fatalf("GET heap profile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heap profile status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "heap profile") {
		t.Fatalf("body does not look like a heap profile: %.120s", body)
	}
}

func TestBareEnableUsesDefaultAddr(t *testing.T) {
	t.Setenv(EnvVar, "1")
	addr, stop, err := StartIfEnabled()
	if err != nil {
		// The default port may legitimately be taken (e.g. a live
		// backend running with pprof enabled). That is a bind error,
		// not a parse/validation error — accept it.
		if !strings.Contains(err.Error(), "listen") {
			t.Fatalf("unexpected error kind: %v", err)
		}
		return
	}
	defer stop()
	if addr != DefaultAddr {
		t.Fatalf("addr = %q, want %q", addr, DefaultAddr)
	}
}
