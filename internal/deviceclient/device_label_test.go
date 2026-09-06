package deviceclient

import "testing"

func TestDeviceLabelPreservesLatestCredentialAndNickname(t *testing.T) {
	be := newBackend(t)
	client, dir := openAgainst(t, be, nil)
	latest := client.Session()
	latest.Credential = "new credential"
	latest.RefreshSecret = "new refresh"
	latest.Nickname = "my name"
	if err := SaveSession(dir, latest); err != nil {
		t.Fatal(err)
	}
	if err := client.SetDeviceLabel("Studio"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSession(dir, latest.BackendID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "Studio" || got.Credential != latest.Credential || got.RefreshSecret != latest.RefreshSecret || got.Nickname != latest.Nickname {
		t.Fatalf("profile metadata overwrote credential: %+v", got)
	}
	client.Retire()
	if err := client.SetDeviceLabel("old owner"); err == nil {
		t.Fatal("retired owner renamed a profile")
	}
}
