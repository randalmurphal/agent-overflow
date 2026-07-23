package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginOpensNativeURLAndWaitsForCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mock-codex-login")
	script := "#!/bin/bash\n" +
		"read -r _ || true\nread -r _ || true\nread -r _ || true\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"type":"chatgpt","loginId":"login-1","authUrl":"https://auth.example.test/authorize?state=secret"}}'` + "\n" +
		`printf '%s\n' '{"jsonrpc":"2.0","method":"account/login/completed","params":{"loginId":"login-1","success":true}}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var opened string
	err := Login(context.Background(), LoginConfig{
		Binary: path,
		OpenURL: func(value string) error {
			opened = value
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if opened != "https://auth.example.test/authorize?state=secret" {
		t.Fatalf("opened URL = %q", opened)
	}
}
