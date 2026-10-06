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
