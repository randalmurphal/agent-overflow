package app

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
)

// The soak sends the page's default send: the reference is
// frontend/src/test/fixtures/pageDefaultSend.json, which a frontend test
// holds to buildSendOptions, read here as the bound method reads a page's
// send. Only the send id differs.
func TestSoakSendsThePageDefaultSend(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(reporoot.Path(t, "frontend/src/test/fixtures/pageDefaultSend.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want SendMessageOptions
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode the page's default send: %v", err)
	}
	got := pageSendOptions("send-1")
	if got.SendID != "send-1" {
		t.Fatalf("SendID = %q, want the one given", got.SendID)
	}
	got.SendID = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("soak options = %+v (draft %+v)\nthe page's default send = %s", got, got.ConsumeDraft, raw)
	}
}
