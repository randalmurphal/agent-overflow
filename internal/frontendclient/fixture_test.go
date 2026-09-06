package frontendclient

// E2E's frontend-only process uses this test binary. It serves the production
// SPA through the production controller, without booting an App or provider.
// Every path is explicit and owned by the caller's isolated fixture.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("AO_TEST_FRONTEND_FIXTURE") == "1" {
		if err := runFrontendFixture(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFrontendFixture() error {
	if os.Getenv("AO_E2E_CONTAINED") != "1" {
		return fmt.Errorf("frontend fixture requires the E2E containment launcher")
	}
	profiles, config, assets := os.Getenv("AO_TEST_FRONTEND_PROFILES"), os.Getenv("AO_TEST_FRONTEND_CONFIG"), os.Getenv("AO_TEST_FRONTEND_ASSETS")
	for _, path := range []string{profiles, config, assets} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("frontend fixture requires explicit absolute paths")
		}
	}
	port, err := strconv.Atoi(os.Getenv("AO_TEST_FRONTEND_PORT"))
	if err != nil {
		return err
	}
	s, err := Serve(Config{Profiles: profiles, ConfigDir: config, ComputerID: os.Getenv("AO_TEST_FRONTEND_COMPUTER"),
		ClientID: "frontend-fixture", Assets: os.DirFS(assets), Port: port, Version: "test"})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"origin": "http://" + s.Addr(), "token": s.transport.Token()}); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	inputClosed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(inputClosed) }()
	select {
	case <-ctx.Done():
	case <-inputClosed:
	}
	return nil
}
