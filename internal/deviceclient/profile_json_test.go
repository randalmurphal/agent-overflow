package deviceclient

import (
	"encoding/json"
	"testing"
)

func TestProfileUnknownFieldsCannotResurrectClearedKnownAliases(t *testing.T) {
	var held Session
	if err := json.Unmarshal([]byte(`{"backendId":"backend","sessionId":"session","credential":"credential","PendingNextSecret":"old-receipt","FutureField":{"enabled":true}}`), &held); err != nil {
		t.Fatal(err)
	}
	if held.PendingNextSecret != "old-receipt" {
		t.Fatal("fixture did not exercise encoding/json's case folding")
	}
	held.PendingNextSecret = ""
	data, err := json.Marshal(held)
	if err != nil {
		t.Fatal(err)
	}
	var reread Session
	if err := json.Unmarshal(data, &reread); err != nil {
		t.Fatal(err)
	}
	if reread.PendingNextSecret != "" {
		t.Fatal("cleared pending operation was resurrected by a field alias")
	}
	if string(reread.extraFields["FutureField"]) != `{"enabled":true}` {
		t.Fatal("unknown field was removed")
	}
}
