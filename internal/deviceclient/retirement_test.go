package deviceclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type heldRotationTransport struct {
	base    http.RoundTripper
	arrived chan struct{}
	resume  chan struct{}
}

func (h heldRotationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := h.base.RoundTrip(request)
	if request.URL.Path == authTokenPath {
		close(h.arrived)
		select {
		case <-h.resume:
		case <-request.Context().Done():
		}
	}
	return response, err
}
func TestRenewalCannotResurrectRemovedOrReplacedProfileAndKeepsConcurrentName(t *testing.T) {
	for _, action := range []string{"forget", "replace", "rename"} {
		t.Run(action, func(t *testing.T) {
			backend := newBackend(t)
			client, dir := openAgainst(t, backend, nil)
			held := heldRotationTransport{base: client.http.Transport, arrived: make(chan struct{}), resume: make(chan struct{})}
			client.http.Transport = held
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- client.renew(ctx) }()
			<-held.arrived
			switch action {
			case "forget":
				if err := client.Forget(); err != nil {
					t.Fatal(err)
				}
			case "replace":
				replacement := client.Session()
				replacement.Credential = "replacement-credential"
				client.Retire()
				if err := SaveSession(dir, replacement); err != nil {
					t.Fatal(err)
				}
			case "rename":
				if err := client.SetNickname("GPU workstation"); err != nil {
					t.Fatal(err)
				}
			}
			close(held.resume)
			err := <-done
			if action == "rename" && err != nil {
				t.Fatal(err)
			}
			if action != "rename" && !errors.Is(err, ErrSessionEnded) {
				t.Fatalf("late renewal: %v", err)
			}
			stored, loadErr := LoadSession(dir, "backend-a")
			switch action {
			case "forget":
				if loadErr == nil {
					t.Fatal("removed profile came back")
				}
			case "replace":
				if loadErr != nil || stored.Credential != "replacement-credential" {
					t.Fatalf("replacement lost: %+v %v", stored, loadErr)
				}
				if err := client.Forget(); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadSession(dir, "backend-a"); err != nil {
					t.Fatal("retired client deleted its replacement")
				}
			case "rename":
				if loadErr != nil || stored.Nickname != "GPU workstation" {
					t.Fatalf("rename lost: %+v %v", stored, loadErr)
				}
			}
		})
	}
}
