package claude

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStartMCPAuthRequiresCredentialReaderAndBoundedRequest(t *testing.T) {
	workDir := t.TempDir()
	_, _, err := StartMCPAuth(context.Background(), MCPAuthConfig{
		Config:         Config{WorkDir: workDir},
		RequestTimeout: time.Second,
	}, "srv")
	if err == nil || !strings.Contains(err.Error(), "credential reader required") {
		t.Fatalf("missing-reader error = %v", err)
	}

	_, _, err = StartMCPAuth(context.Background(), MCPAuthConfig{
		Config:         Config{WorkDir: workDir},
		ReadCredential: func() ([]byte, error) { return nil, nil },
	}, "srv")
	if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("missing-timeout error = %v", err)
	}
}
