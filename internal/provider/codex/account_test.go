package codex

import (
	"testing"
)

func TestAccountReadShapeCarriesChatGPTIdentity(t *testing.T) {
	t.Parallel()
	info, err := decodeAccountInfo(
		[]byte(`{"account":{"type":"chatgpt","email":"user@example.com","planType":"pro"},"requiresOpenaiAuth":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if info.Email != "user@example.com" ||
		info.SubscriptionType != "pro" ||
		info.APIProvider != "openai" {
		t.Fatalf("info = %+v", info)
	}
}

// workspaceName (codex 0.166+) is the selected ChatGPT workspace's display
// name. Older servers omit it and newer ones send null when no workspace
// is known; both must read as "no name", never as a stale or invented one.
func TestAccountReadShapeCarriesWorkspaceName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		account string
		want    string
	}{
		{"reported", `{"type":"chatgpt","email":"user@example.com","planType":"team","workspaceName":"  Acme Team "}`, "Acme Team"},
		{"null", `{"type":"chatgpt","email":"user@example.com","planType":"team","workspaceName":null}`, ""},
		{"omitted by older codex", `{"type":"chatgpt","email":"user@example.com","planType":"team"}`, ""},
		{"blank", `{"type":"chatgpt","email":"user@example.com","planType":"team","workspaceName":"  "}`, ""},
		{"api key", `{"type":"apiKey"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, err := decodeAccountInfo([]byte(`{"account":` + tc.account + `,"requiresOpenaiAuth":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if info.OrgName != tc.want {
				t.Fatalf("OrgName = %q, want %q (info %+v)", info.OrgName, tc.want, info)
			}
			if info.OrgID != "" {
				t.Fatalf("OrgID = %q: account/read carries no workspace id", info.OrgID)
			}
		})
	}
}
