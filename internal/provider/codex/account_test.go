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
// Only a chatgpt answer that names the login reports whether a name exists;
// any other answer must not clear a saved one.
func TestAccountReadShapeCarriesWorkspaceName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		account  string
		want     string
		reported bool
	}{
		{"reported", `{"type":"chatgpt","email":"user@example.com","planType":"team","workspaceName":"  Acme Team "}`, "Acme Team", true},
		{"null", `{"type":"chatgpt","email":"user@example.com","planType":"team","workspaceName":null}`, "", true},
		{"omitted by older codex", `{"type":"chatgpt","email":"user@example.com","planType":"team"}`, "", true},
		{"blank", `{"type":"chatgpt","email":"user@example.com","planType":"team","workspaceName":"  "}`, "", true},
		{"null email", `{"type":"chatgpt","email":null,"planType":"team","workspaceName":"Acme"}`, "Acme", false},
		{"api key", `{"type":"apiKey"}`, "", false},
		{"bedrock", `{"type":"amazonBedrock","usesCodexManagedCredentials":true}`, "", false},
		{"unknown type with email", `{"type":"future","email":"user@example.com"}`, "", false},
		{"no account", `null`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, err := decodeAccountInfo([]byte(`{"account":` + tc.account + `,"requiresOpenaiAuth":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if info.OrgName != tc.want || info.OrgNameReported != tc.reported {
				t.Fatalf("OrgName = %q reported = %v, want %q reported = %v (info %+v)",
					info.OrgName, info.OrgNameReported, tc.want, tc.reported, info)
			}
			if info.OrgID != "" {
				t.Fatalf("OrgID = %q: account/read carries no workspace id", info.OrgID)
			}
		})
	}
}
