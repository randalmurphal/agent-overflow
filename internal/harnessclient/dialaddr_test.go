package harnessclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-overflow/internal/localcontrol"
)

func TestControlFileNameMatchesLocalControl(t *testing.T) {
	if controlFileName != localcontrol.Filename {
		t.Fatalf("controlFileName = %q, localcontrol.Filename = %q", controlFileName, localcontrol.Filename)
	}
}

// pageResponder answers the page-URL route with its own name, so a test can
// tell which listener a client reached.
func pageResponder(t *testing.T, network, addr, name string) *httptest.Server {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("http://" + name + "/\n"))
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// Clients dial the address the instance published in control.json, which
// is its ::1 listener: under WSL virtioproxy, 127.0.0.1 inside the distro
// goes through a Windows relay that refuses bursts of connects. A file
// from another launch, or none yet, leaves the main bind's port.
func TestClientsDialTheAddressTheInstancePublished(t *testing.T) {
	mainSrv := pageResponder(t, "tcp4", "127.0.0.1:0", "main")
	localSrv := pageResponder(t, "tcp6", "[::1]:0", "local")
	mainAddr := mainSrv.Listener.Addr().String()
	localAddr := localSrv.Listener.Addr().String()

	for _, tc := range []struct {
		name      string
		published string
		token     string
		want      string
		wantAddr  string
	}{
		{"published ::1 listener", localAddr, "tok", "local", localAddr},
		{"listener failed, main bind republished", mainAddr, "tok", "main", mainAddr},
		{"another launch's file", localAddr, "other", "main", mainAddr},
		{"nothing published yet", "", "", "main", mainAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.published != "" {
				if err := localcontrol.Publish(dir, tc.published, tc.token); err != nil {
					t.Fatal(err)
				}
			}
			bs := bootstrapFor(t, mainSrv, "tok")
			bs.DataDir = dir

			if want := "ws://" + tc.wantAddr + "/ws?"; !strings.HasPrefix(bs.WSURL(), want) {
				t.Fatalf("WSURL = %q, want prefix %q", bs.WSURL(), want)
			}
			got, err := bs.PageURL(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if want := "http://" + tc.want + "/"; got != want {
				t.Fatalf("PageURL reached %q, want %q", got, want)
			}
		})
	}
}
