package localcontrol

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-overflow/internal/pagehost"
)

func TestLocalDesktopPageUsesHeaderCredentialAndRejectsRemoteNavigation(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8123", "localhost:8123", "elsewhere.test"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer private-launch" || r.URL.Query().Get(pagehost.Param) != pagehost.Webview {
				t.Error("missing local authorization")
			}
			if strings.Contains(r.URL.String(), "private-launch") {
				t.Error("credential in URL")
			}
			_ = json.NewEncoder(w).Encode(pagehost.Answer{URL: "http://" + host + "/?host=webview", Ticket: "once"})
		}))
		answer, err := Page(t.Context(), Endpoint{Address: strings.TrimPrefix(server.URL, "http://"), Token: "private-launch"})
		server.Close()
		if host == "elsewhere.test" {
			if err == nil {
				t.Fatal("remote page accepted")
			}
		} else if err != nil || answer.Ticket != "once" {
			t.Fatalf("page: %+v %v", answer, err)
		}
	}
}
