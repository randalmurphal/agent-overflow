package deviceclient

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func storedSession(backendID, endpoint string) Session {
	return Session{
		BackendID:   backendID,
		BackendName: "Studio",
		Endpoint:    endpoint,
		SessionID:   "session-" + backendID,
		Credential:  "ao1.credential",
		Scopes:      []string{"threads:read"},
	}
}

// TestSession_RoundTripsAndSurvivesAReplacement — a rotation rewrites this
// file on every renewal, so what it reads back has to be what it wrote,
// including the second time.
func TestSession_RoundTripsAndSurvivesAReplacement(t *testing.T) {
	dir := t.TempDir()
	want := storedSession("backend-a", "http://a:8317")
	if err := SaveSession(dir, want); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	got, err := LoadSession(dir, "backend-a")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got.SessionID != want.SessionID || got.Credential != want.Credential {
		t.Fatalf("LoadSession = %+v, want %+v", got, want)
	}

	rotated := want
	rotated.Credential = "ao1.rotated"
	if err := SaveSession(dir, rotated); err != nil {
		t.Fatalf("SaveSession again: %v", err)
	}
	if got, err = LoadSession(dir, "backend-a"); err != nil || got.Credential != "ao1.rotated" {
		t.Fatalf("after rotation = %+v (err %v)", got, err)
	}

	info, err := os.Stat(filepath.Join(dir, SessionsDirName, "backend-a.json"))
	if err != nil {
		t.Fatalf("stat the session file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestLoadSession_AbsentAndUnusableAreDifferentAnswers — absent is what
// every unpaired installation answers, and a file that decoded into
// something unpresentable is a state somebody has to be told about.
func TestLoadSession_AbsentAndUnusableAreDifferentAnswers(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("LoadSession on an empty profile = %v, want ErrNoSession", err)
	}
	root := filepath.Join(dir, SessionsDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "backend-a.json"), []byte(`{"backendId":"backend-a"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadSession(dir, "backend-a")
	if err == nil {
		t.Fatal("LoadSession accepted a session with no credential")
	}
	if errors.Is(err, ErrNoSession) {
		t.Fatalf("an unusable file reported as an absent one: %v", err)
	}
}

// TestSessionPath_RefusesABackendIDThatIsNotAName closes the class rather
// than sanitising a spelling. The id arrives in a payload this client did
// not mint and becomes a path component; the alphabet has no separator and
// no dot in it, so nothing that passes can address another directory.
func TestSessionPath_RefusesABackendIDThatIsNotAName(t *testing.T) {
	dir := t.TempDir()
	for _, backendID := range []string{
		"", "..", "../escape", "a/b", `a\b`, "a.b", "back end",
		strings.Repeat("a", maxBackendIDLen+1),
	} {
		if _, err := sessionPath(dir, backendID); err == nil {
			t.Errorf("sessionPath accepted %q", backendID)
		}
		if err := SaveSession(dir, Session{BackendID: backendID}); err == nil {
			t.Errorf("SaveSession accepted backend id %q", backendID)
		}
	}
	// A real backend id is a UUID and passes unchanged.
	if _, err := sessionPath(dir, "1b4e28ba-2fa1-11d2-883f-0016d3cca427"); err != nil {
		t.Errorf("sessionPath refused a UUID: %v", err)
	}
}

// TestResolve_ThreeSpellingsAndNoGuessing — each spelling is what somebody
// has in front of them at a different moment. What is deliberately absent
// is a prefix match: a resolution that could mean two backends is one that
// attaches to the wrong machine silently.
func TestResolve_ThreeSpellingsAndNoGuessing(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSession(dir, storedSession("backend-a", "http://192.168.1.5:8317")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if err := SaveSession(dir, storedSession("backend-b", "https://studio.example:9000")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	for name, target := range map[string]string{
		"backend id":     "backend-a",
		"exact endpoint": "http://192.168.1.5:8317",
		"authority":      "192.168.1.5:8317",
		"other scheme":   "ws://192.168.1.5:8317",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Resolve(dir, target)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", target, err)
			}
			if got.BackendID != "backend-a" {
				t.Fatalf("Resolve(%q) = %q, want backend-a", target, got.BackendID)
			}
		})
	}

	if _, err := Resolve(dir, "backend-c"); !errors.Is(err, ErrNoSession) {
		t.Errorf("Resolve on an unknown name = %v, want ErrNoSession", err)
	}
	if _, err := Resolve(dir, ""); err == nil {
		t.Error("Resolve accepted an empty target")
	}
	if _, err := Resolve(t.TempDir(), "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Errorf("Resolve on an unpaired profile = %v, want ErrNoSession", err)
	}
}

// TestResolve_AmbiguityIsAnErrorThatNamesTheCandidates — answering with
// the first hit would attach to one of two machines by directory order.
func TestResolve_AmbiguityIsAnErrorThatNamesTheCandidates(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSession(dir, storedSession("backend-a", "http://192.168.1.5:8317")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if err := SaveSession(dir, storedSession("backend-b", "https://192.168.1.5:8317")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	_, err := Resolve(dir, "192.168.1.5:8317")
	if err == nil {
		t.Fatal("Resolve picked one of two backends on the same authority")
	}
	if !strings.Contains(err.Error(), "backend-a") || !strings.Contains(err.Error(), "backend-b") {
		t.Fatalf("the ambiguity error names neither candidate: %v", err)
	}
}

// TestListSessions_SkipsWhatItCannotRead — one damaged profile must not
// make the other backends unreachable. The damaged one surfaces as "not
// paired" the moment somebody names it, which the resolve above covers.
func TestListSessions_SkipsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSession(dir, storedSession("backend-b", "http://b:8317")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if err := SaveSession(dir, storedSession("backend-a", "http://a:8317")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	root := filepath.Join(dir, SessionsDirName)
	if err := os.WriteFile(filepath.Join(root, "damaged.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	sessions, err := ListSessions(dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions returned %d sessions, want the 2 readable ones", len(sessions))
	}
	if sessions[0].BackendID != "backend-a" || sessions[1].BackendID != "backend-b" {
		t.Fatalf("ListSessions is not ordered by backend id: %v", sessions)
	}
	if got, err := ListSessions(t.TempDir()); err != nil || len(got) != 0 {
		t.Fatalf("ListSessions on an unpaired profile = %v (err %v), want empty", got, err)
	}
}

// TestForgetSession_KeepsTheDeviceKey — the key names the DEVICE, and the
// backend adopts its row by thumbprint when this installation pairs again.
// Removing it would strand that row.
func TestForgetSession_KeepsTheDeviceKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnrollDeviceKey(dir); err != nil {
		t.Fatalf("EnrollDeviceKey: %v", err)
	}
	if err := SaveSession(dir, storedSession("backend-a", "http://a:8317")); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if err := ForgetSession(dir, "backend-a"); err != nil {
		t.Fatalf("ForgetSession: %v", err)
	}
	if _, err := LoadSession(dir, "backend-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("the session survived a forget: %v", err)
	}
	if _, err := DeviceKey(dir); err != nil {
		t.Fatalf("the device key did not survive a forget: %v", err)
	}
	// Idempotent: forgetting what is already gone is not a failure.
	if err := ForgetSession(dir, "backend-a"); err != nil {
		t.Fatalf("ForgetSession twice: %v", err)
	}
}

// TestNicknameSurvivesAndIsSeparateFromTheBackendsOwnName — a nickname is
// what THIS installation calls a machine, and the machine goes on calling
// itself whatever it calls itself. Two machines that both answer
// "mac-mini" are told apart by nothing else.
func TestNicknameSurvivesAndIsSeparateFromTheBackendsOwnName(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnrollDeviceKey(dir); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	session := Session{
		BackendID: "aaa", BackendName: "mac-mini", Endpoint: "https://mini.local:8443",
		SessionID: "s1", Credential: "c1",
	}
	if err := SaveSession(dir, session); err != nil {
		t.Fatalf("save: %v", err)
	}
	client, err := Open(dir, session)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := client.SetNickname("The Loft Mini"); err != nil {
		t.Fatalf("set nickname: %v", err)
	}
	reloaded, err := LoadSession(dir, "aaa")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Nickname != "The Loft Mini" {
		t.Errorf("nickname = %q, want it persisted", reloaded.Nickname)
	}
	if reloaded.BackendName != "mac-mini" {
		t.Errorf("BackendName = %q, want the machine's own name untouched", reloaded.BackendName)
	}
	if reloaded.Credential != "c1" {
		t.Errorf("credential = %q, want a rename to touch nothing else", reloaded.Credential)
	}
	if held := client.Session(); held.Nickname != "The Loft Mini" {
		t.Errorf("in-memory nickname = %q, want it updated in step", held.Nickname)
	}
}
