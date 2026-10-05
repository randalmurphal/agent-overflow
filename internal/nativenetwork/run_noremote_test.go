//go:build noremote

package nativenetwork

import (
	"context"
	"testing"
	"time"
)

type countingBridge struct{ calls int }

func (b *countingBridge) GetNativeNetworkConfig(context.Context) (Config, error) {
	b.calls++
	return Config{Enabled: true}, nil
}

func (b *countingBridge) ReportNativeNetworkState(context.Context, State) error {
	b.calls++
	return nil
}

// A build without remote access returns from Run before asking the backend
// for a configuration, so the launcher can never bind a LAN relay even when
// the backend would say LAN is enabled.
func TestRunNeverReachesTheBridgeWithoutRemoteAccess(t *testing.T) {
	bridge := &countingBridge{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		Run(ctx, bridge)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Run kept polling in a build without remote access")
	}
	if bridge.calls != 0 {
		t.Fatalf("Run reached the bridge %d times", bridge.calls)
	}
}
