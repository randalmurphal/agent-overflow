package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/logging"
)

func newProviderEventLogger(baseDir string) (*logging.Logger, error) {
	if !providerEventLoggingEnabled(os.Getenv("AGENT_OVERFLOW_DEBUG")) {
		return nil, nil
	}

	path := filepath.Join(baseDir, "logs", fmt.Sprintf(
		"provider-events-%s.ndjson",
		time.Now().Format("2006-01-02"),
	))
	return logging.NewLogger(path, 0)
}

func providerEventLoggingEnabled(value string) bool {
	for _, topic := range strings.Split(value, ",") {
		switch strings.TrimSpace(strings.ToLower(topic)) {
		case "all", "provider":
			return true
		}
	}
	return false
}
