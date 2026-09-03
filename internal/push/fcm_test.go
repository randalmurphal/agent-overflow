package push

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// NOTHING HERE REACHES THE NETWORK. The credential's own `token_uri` points
// at the test server, so the OAuth exchange lands there too, and the send
// endpoint is injected. That is the whole reason `newFCMSenderAt` exists: a
// sender whose endpoint was a constant could only be proved against Google.

// testKey is one RSA key for the whole package. Generated rather than
// pasted, so no key material is committed, and generated ONCE because 2048
// bits is the slowest thing in this file by an order of magnitude.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("push test: generate key: %v", err))
	}
	return key
})

func testKeyPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatalf("marshal test key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// credentialJSON is a service-account key file whose token endpoint is the
// caller's test server.
func credentialJSON(t *testing.T, tokenURI string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "ao-test-project",
		"private_key":  testKeyPEM(t),
		"client_email": "sender@ao-test-project.iam.gserviceaccount.com",
		"token_uri":    tokenURI,
	})
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	return raw
}

// capture is one FCM endpoint plus the token endpoint in front of it.
type capture struct {
	server   *httptest.Server
	mu       sync.Mutex
	auth     string
	body     []byte
	status   int
	refusal  string
	requests int
	tokens   int
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.tokens++
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-token","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		c.mu.Lock()
		c.auth = r.Header.Get("Authorization")
		c.body = body
		c.requests++
		status, refusal := c.status, c.refusal
		c.mu.Unlock()
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"name":"projects/ao-test-project/messages/1"}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(refusal))
	})
	c.server = httptest.NewServer(mux)
	t.Cleanup(c.server.Close)
	return c
}

func (c *capture) refuse(status int, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.refusal = status, body
}

func (c *capture) sender(t *testing.T) *FCMSender {
	t.Helper()
	cred, err := ParseCredential(credentialJSON(t, c.server.URL+"/token"))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	sender, err := newFCMSenderAt(cred, c.server.URL+"/send")
	if err != nil {
		t.Fatalf("newFCMSenderAt: %v", err)
	}
	return sender
}

func sampleMessage() Message {
	return Message{
		Token: "device-token",
		Tag:   "thread:t-1",
		Data:  map[string]string{KeyID: "thread:t-1", KeyKind: "turn-complete"},
	}
}

// The send is data-only, high priority, collapsed on the moment's id, and
// bearing the minted token. Asserted as the exact document because every
// one of those four is a design decision this package states in prose.
func TestASendIsDataOnlyAndCarriesTheMintedBearer(t *testing.T) {
	capture := newCapture(t)

	if err := capture.sender(t).Send(context.Background(), sampleMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	capture.mu.Lock()
	auth, body := capture.auth, string(capture.body)
	capture.mu.Unlock()

	if auth != "Bearer minted-token" {
		t.Errorf("Authorization = %q, want the token the exchange minted", auth)
	}
	want := `{"message":{"token":"device-token","data":{"id":"thread:t-1","kind":"turn-complete"},` +
		`"android":{"priority":"high","collapse_key":"thread:t-1"}}}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	if strings.Contains(body, `"notification"`) {
		t.Error("the request carried a notification block; a push must be data-only so the phone renders and a retraction is expressible")
	}
}

// One access token serves a whole fan-out. The reuse source is what keeps a
// backend with four phones from performing four OAuth exchanges per moment.
func TestTheAccessTokenIsMintedOnceForManySends(t *testing.T) {
	capture := newCapture(t)
	sender := capture.sender(t)

	for range 3 {
		if err := sender.Send(context.Background(), sampleMessage()); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	capture.mu.Lock()
	tokens, requests := capture.tokens, capture.requests
	capture.mu.Unlock()
	if requests != 3 {
		t.Fatalf("the endpoint saw %d sends, want 3", requests)
	}
	if tokens != 1 {
		t.Errorf("the token endpoint was called %d times, want 1: the source must cache", tokens)
	}
}

// UNREGISTERED is the one refusal a caller acts on, and it has two
// spellings. Both answer ErrTokenGone, because the action — drop the row —
// is the same and a proxy may preserve only one of them.
func TestAnUnregisteredTokenIsTheOneActionableRefusal(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "the documented 404 with the error code",
			status: http.StatusNotFound,
			body: `{"error":{"code":404,"status":"NOT_FOUND","details":[` +
				`{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`,
		},
		{
			name:   "the error code alone, behind another status",
			status: http.StatusBadRequest,
			body: `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[` +
				`{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`,
		},
		{
			name:   "a bare 404 with no body a proxy preserved",
			status: http.StatusNotFound,
			body:   "",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			capture := newCapture(t)
			capture.refuse(row.status, row.body)
			err := capture.sender(t).Send(context.Background(), sampleMessage())
			if !errors.Is(err, ErrTokenGone) {
				t.Fatalf("Send = %v, want ErrTokenGone", err)
			}
		})
	}
}

// Everything else describes THIS ATTEMPT and not the token. A 5xx that
// dropped a row would let one bad afternoon at Google unregister every
// phone the owner has, and INVALID_ARGUMENT is what a bug in this package's
// own payload would produce.
func TestAReachFailureIsNotATokenFailure(t *testing.T) {
	for _, row := range []struct {
		name   string
		status int
		body   string
	}{
		{"a server error", http.StatusInternalServerError, `{"error":{"status":"INTERNAL"}}`},
		{"a refused credential", http.StatusUnauthorized, `{"error":{"status":"UNAUTHENTICATED"}}`},
		{
			"an argument refusal with no UNREGISTERED code",
			http.StatusBadRequest,
			`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid JSON payload"}}`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			capture := newCapture(t)
			capture.refuse(row.status, row.body)
			err := capture.sender(t).Send(context.Background(), sampleMessage())
			if err == nil {
				t.Fatal("Send answered nil for a refusal")
			}
			if errors.Is(err, ErrTokenGone) {
				t.Fatalf("Send = %v, want a plain error: only UNREGISTERED drops a row", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(row.status)) {
				t.Errorf("the error %q does not name the status the owner would see", err)
			}
		})
	}
}

// Shape validation, and no more: whether Google ACCEPTS the credential is a
// question only a real send can ask, and the answer to that one reaches the
// owner through the sender status.
func TestParseCredentialChecksShapeWithoutANetwork(t *testing.T) {
	good := credentialJSON(t, "https://example.invalid/token")
	cred, err := ParseCredential(good)
	if err != nil {
		t.Fatalf("ParseCredential refused a well-formed key: %v", err)
	}
	if cred.ProjectID != "ao-test-project" || cred.ClientEmail == "" {
		t.Fatalf("ParseCredential = %+v, want the project and account read out", cred)
	}
	if string(cred.Raw()) != string(good) {
		t.Error("Raw must answer the document as pasted: the OAuth flow is built from the whole key file")
	}

	for _, row := range []struct{ name, json string }{
		{"not JSON at all", "sorry, wrong file"},
		{"an OAuth client id rather than a service account", `{"type":"authorized_user","project_id":"p"}`},
		{"no project", `{"type":"service_account","client_email":"a@b","private_key":"x"}`},
		{"no account", `{"type":"service_account","project_id":"p","private_key":"x"}`},
		{"a private key that is not PEM", `{"type":"service_account","project_id":"p","client_email":"a@b","private_key":"not-a-key"}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := ParseCredential([]byte(row.json)); err == nil {
				t.Fatalf("ParseCredential accepted %s", row.name)
			}
		})
	}
}
