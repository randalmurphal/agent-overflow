//go:build !windows

package shellenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv unsets names for the test and restores them afterwards.
func clearEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// exportingShell is a stub login shell that exports the given variables and
// then runs the probe's real script, so the script itself is under test.
func exportingShell(t *testing.T, exports ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakesh")
	body := "#!/bin/sh\n"
	for _, export := range exports {
		body += "export " + export + "\n"
	}
	body += "eval \"$2\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The probe carries certificate and proxy settings to a backend started
// outside a shell. A value the app inherited wins, an empty one is not
// imported, and nothing outside the list (a credential here) comes along.
func TestSyncImportsCertificateAndProxyVariables(t *testing.T) {
	clearEnv(t, append(importedVars, "OPENAI_API_KEY")...)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/inherited/extra.pem")
	t.Setenv("SHELL", exportingShell(t,
		"SSL_CERT_FILE=/login/bundle.pem",
		"NODE_EXTRA_CA_CERTS=/login/extra.pem",
		"HTTPS_PROXY=http://proxy.corp:8080",
		"NO_PROXY=corp.example",
		"REQUESTS_CA_BUNDLE=",
		"OPENAI_API_KEY=sk-test",
	))

	if err := Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	for name, want := range map[string]string{
		"SSL_CERT_FILE":       "/login/bundle.pem",
		"NODE_EXTRA_CA_CERTS": "/inherited/extra.pem",
		"HTTPS_PROXY":         "http://proxy.corp:8080",
		"NO_PROXY":            "corp.example,localhost,127.0.0.1,::1,[::1]",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"REQUESTS_CA_BUNDLE", "OPENAI_API_KEY", "no_proxy"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s = %q, want it left unset", name, value)
		}
	}
}

func TestLoopbackBypassesAConfiguredProxy(t *testing.T) {
	proxyNames := []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"}
	unset := "<unset>"
	for _, tc := range []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{"no proxy leaves the lists alone",
			map[string]string{},
			map[string]string{"NO_PROXY": unset, "no_proxy": unset}},
		{"a 127.0.0.1-only list gains the other loopback names",
			map[string]string{"https_proxy": "http://proxy:3128", "no_proxy": "127.0.0.1"},
			map[string]string{"no_proxy": "127.0.0.1,localhost,::1,[::1]", "NO_PROXY": unset}},
		{"no list sets both spellings",
			map[string]string{"HTTP_PROXY": "http://proxy:3128"},
			map[string]string{"NO_PROXY": "localhost,127.0.0.1,::1,[::1]", "no_proxy": "localhost,127.0.0.1,::1,[::1]"}},
		{"a wildcard already bypasses everything",
			map[string]string{"ALL_PROXY": "socks5://proxy:1080", "NO_PROXY": "*"},
			map[string]string{"NO_PROXY": "*", "no_proxy": unset}},
		{"existing entries match without regard to case or spacing",
			map[string]string{"HTTPS_PROXY": "http://proxy:3128", "NO_PROXY": "LOCALHOST, ::1 ,"},
			map[string]string{"NO_PROXY": "LOCALHOST, ::1,127.0.0.1,[::1]", "no_proxy": unset}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t, proxyNames...)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			if err := ensureLoopbackBypassesProxy(); err != nil {
				t.Fatal(err)
			}
			for name, want := range tc.want {
				got, ok := os.LookupEnv(name)
				if !ok {
					got = unset
				}
				if got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestExtractVarsReadsOnlyTheImportedNames(t *testing.T) {
	out := strings.Join([]string{
		"banner", varsStartSentinel,
		"SSL_CERT_FILE=/a=b.pem", "", "HTTPS_PROXY=", "OPENAI_API_KEY=sk-test", "not a pair",
		varsEndSentinel, "SSL_CERT_DIR=/after/end",
	}, "\n")
	vars, err := extractVars(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 1 || vars["SSL_CERT_FILE"] != "/a=b.pem" {
		t.Fatalf("vars = %v, want only SSL_CERT_FILE=/a=b.pem", vars)
	}
	if _, err := extractVars("banner\n" + varsStartSentinel + "\nSSL_CERT_FILE=/x\n"); err == nil {
		t.Fatal("extractVars accepted output with no end sentinel")
	}
}
