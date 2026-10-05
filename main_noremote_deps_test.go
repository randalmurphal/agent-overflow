package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// noremoteForbiddenImports are the packages that carry remote access:
// the tailnet node (and WireGuard under it), LAN multicast discovery, ACME
// certificate issuance and the FCM push client. A build tagged `noremote`
// must link none of them (internal/buildvariant).
var noremoteForbiddenImports = []string{
	"tailscale.com/",
	"github.com/hashicorp/mdns",
	"github.com/miekg/dns",
	"golang.org/x/crypto/acme",
	"golang.org/x/oauth2/google",
}

// goListDeps answers the import graph of one build configuration. cgo is
// set as the release build sets it (build/windows/Taskfile.yml): the Linux
// payload needs it for internal/highlight, and the launcher is built
// without it. Go turns cgo off by default when GOOS names another platform,
// so leaving it unset would list a different graph on a macOS host.
func goListDeps(t *testing.T, goos, tags, pkg string) []string {
	t.Helper()
	cgo := "0"
	if goos == "linux" {
		cgo = "1"
	}
	cmd := exec.Command("go", "list", "-deps", "-tags", tags, pkg)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED="+cgo)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("go list -deps -tags %s %s (GOOS=%s CGO_ENABLED=%s): %v\n%s", tags, pkg, goos, cgo, err, stderr)
	}
	return strings.Fields(string(out))
}

func forbiddenIn(deps []string) []string {
	var found []string
	for _, dep := range deps {
		for _, forbidden := range noremoteForbiddenImports {
			if dep == forbidden || strings.HasPrefix(dep, strings.TrimSuffix(forbidden, "/")+"/") {
				found = append(found, dep)
			}
		}
	}
	return found
}

// The noremote release ships two binaries: the Linux payload (built with the
// payload's own tags, build/windows/Taskfile.yml) and the Windows launcher
// that embeds it. Neither may link a remote-access dependency. The standard
// payload is checked to link them, so a list that matches nothing cannot
// pass this test.
func TestNoremoteBuildsLinkNoRemoteAccessDependencies(t *testing.T) {
	if found := forbiddenIn(goListDeps(t, "linux", "production,nogui", ".")); len(found) == 0 {
		t.Fatal("the standard payload links none of the remote-access packages; the forbidden list no longer names them")
	}
	builds := []struct{ name, goos, tags, pkg string }{
		{"WSL payload", "linux", "production,nogui,noremote", "."},
		{"Windows launcher", "windows", "noremote", "./cmd/agent-overflow-windows"},
		{"noremote harness binary", "linux", "noremote", "."},
	}
	for _, build := range builds {
		if found := forbiddenIn(goListDeps(t, build.goos, build.tags, build.pkg)); len(found) > 0 {
			t.Errorf("%s links remote-access packages: %v", build.name, found)
		}
	}
}
