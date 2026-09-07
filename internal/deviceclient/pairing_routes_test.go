package deviceclient

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
)

func TestPairingRoutesRejectWrongIdentityAndPinBeforeRedemption(t *testing.T) {
	unexpected := func(http.ResponseWriter, *http.Request) { t.Error("route selection sent a credential-bearing request") }
	_, good := candidateServer(t, "target", unexpected)
	_, wrong := candidateServer(t, "other", unexpected)
	badPin := good
	badPin.CertFingerprint = "sha256:" + strings.Repeat("0", 64)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := SelectPairingRoute(ctx, "target", []computerroute.Route{wrong, badPin}); err == nil {
		t.Fatal("wrong identity or pin accepted")
	}
	chosen, err := SelectPairingRoute(ctx, "target", []computerroute.Route{wrong, badPin, good})
	if err != nil || chosen != good {
		t.Fatalf("verified alternate not selected: %+v %v", chosen, err)
	}
}
