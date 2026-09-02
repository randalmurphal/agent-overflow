package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// The Firebase Cloud Messaging HTTP v1 sender.
//
// WHY THE OWNER'S BACKEND AND NO OTHER SENDS (§18 item 1, ruled 2026-09-01):
// sending needs the APP's Firebase service credential, which belongs to the
// app's project and cannot be handed to every self-hosted backend. So the
// owner pastes that credential into their own backend once and it sends
// directly; any other backend registers its phones' tokens and sends
// nothing. The later "the owner's home backend is the wake relay" answer is
// a second `Sender`, which is why the fan-out never mentions this type.

// fcmSendURL is the v1 endpoint, with the project id from the credential.
// A constant format string rather than a field, so the only thing a
// credential decides is which project.
const fcmSendURL = "https://fcm.googleapis.com/v1/projects/%s/messages:send"

// messagingScope is the one OAuth scope a send needs.
const messagingScope = "https://www.googleapis.com/auth/firebase.messaging"

// sendTimeout bounds one delivery, token exchange included.
//
// A push is fire-and-forget work on a queue behind the desktop's own toast,
// so nothing waits on it — but the queue is ORDERED, and a send that hung
// would hold every later moment behind it. Ten seconds is far past a
// healthy round trip to Google and far short of a person noticing.
const sendTimeout = 10 * time.Second

// maxErrorBody bounds what is read off a refusal before it is classified.
// A remote server decides this body's length; nothing here may let it
// decide this process's allocation.
const maxErrorBody = 8 << 10

// Credential is a Google service-account key, reduced to the fields a send
// needs plus the raw bytes the token exchange is built from.
//
// SHAPE ONLY, VALIDATED WITHOUT A NETWORK. `ParseCredential` answers
// whether this document could possibly be a service-account key — the right
// type, a project, an account, and a private key that actually parses — and
// nothing more. Whether Google ACCEPTS it is a question only a real send can
// ask, and the answer to that one is reported through the sender status the
// owner reads, not through the paste field. Reaching the network from a
// setter would also put a network call in `make go-test`, which is banned.
type Credential struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`

	// raw is the document as pasted. Kept because the OAuth flow is built
	// from the whole key file (`google.JWTConfigFromJSON`), and re-encoding
	// the parsed fields would drop anything a future key format added.
	raw []byte
}

// Raw is the credential document as it was pasted. Backend-local, stored
// beside the signing keys and never on a read wire shape.
func (c Credential) Raw() []byte { return c.raw }

// ParseCredential reads and shape-checks a service-account key.
func ParseCredential(raw []byte) (Credential, error) {
	var cred Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return Credential{}, fmt.Errorf("push: that is not a JSON key file: %w", err)
	}
	if cred.Type != "service_account" {
		return Credential{}, fmt.Errorf(
			"push: a sender credential must be a service account key, not %q", cred.Type)
	}
	if cred.ProjectID == "" {
		return Credential{}, errors.New("push: the key file names no project_id")
	}
	if cred.ClientEmail == "" {
		return Credential{}, errors.New("push: the key file names no client_email")
	}
	if _, err := parsePrivateKey(cred.PrivateKey); err != nil {
		return Credential{}, err
	}
	cred.raw = bytes.Clone(raw)
	return cred, nil
}

// parsePrivateKey is the parseability half of the shape check: the same PEM
// and PKCS#1/PKCS#8 pair the OAuth library will do at token time, done now
// so a typo in the paste field is refused where a person can see it rather
// than an hour later on the first turn that completes.
func parsePrivateKey(key string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, errors.New("push: the key file's private_key is not PEM")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("push: the key file's private_key is not an RSA key")
		}
		return rsaKey, nil
	}
	parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("push: the key file's private_key did not parse: %w", err)
	}
	return parsed, nil
}

// FCMSender sends through FCM HTTP v1 with a service account.
type FCMSender struct {
	endpoint string
	tokens   oauth2.TokenSource
	client   *http.Client
}

// NewFCMSender builds the sender for one credential.
//
// The token source is the library's REUSE source, so an access token is
// minted once and shared by every send until it is near expiry — a fan-out
// to four phones is one exchange, not four. The same bounded client carries
// the exchange and the send, because a token endpoint that hangs would
// otherwise have no timeout at all.
func NewFCMSender(cred Credential) (*FCMSender, error) {
	return newFCMSenderAt(cred, fmt.Sprintf(fcmSendURL, url.PathEscape(cred.ProjectID)))
}

func newFCMSenderAt(cred Credential, endpoint string) (*FCMSender, error) {
	config, err := google.JWTConfigFromJSON(cred.raw, messagingScope)
	if err != nil {
		return nil, fmt.Errorf("push: read the sender credential: %w", err)
	}
	client := &http.Client{Timeout: sendTimeout}
	// The context is the CARRIER for that client and nothing else: x/oauth2
	// reads it at exchange time, and this source outlives any one call. A
	// per-call context would cancel the shared token source's refresh with
	// whichever send happened to trigger it.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, client)
	return &FCMSender{endpoint: endpoint, tokens: config.TokenSource(ctx), client: client}, nil
}

// fcmRequest is the v1 send body. Data-only: there is deliberately no
// `notification` field on the message, so nothing here can grow one.
type fcmRequest struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android fcmAndroid        `json:"android"`
}

type fcmAndroid struct {
	// Priority is high because a data-only message at normal priority is
	// held until the phone leaves Doze, and the moments this pipe carries
	// are exactly the ones a person is waiting on. The phone still decides
	// whether to make a sound: the tray channel is the device's.
	Priority string `json:"priority"`
	// CollapseKey is the moment's id, so an offline phone that comes back
	// receives the LAST state of a moment rather than its whole history.
	CollapseKey string `json:"collapse_key,omitempty"`
}

// fcmError is the refusal envelope, read only far enough to classify.
type fcmError struct {
	Error struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Details []struct {
			Type      string `json:"@type"`
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

// Send delivers one message.
//
// TWO OUTCOMES ARE DISTINGUISHED AND NO MORE. `ErrTokenGone` for the
// registration that will never work again, a plain error for everything
// else. In particular INVALID_ARGUMENT is NOT read as a dead token even
// though Google documents it as one possibility: it is also what a
// malformed payload of OUR OWN gets, and a bug in this package must not
// unregister every phone the owner has on its way out.
func (s *FCMSender) Send(ctx context.Context, message Message) error {
	body, err := json.Marshal(fcmRequest{Message: fcmMessage{
		Token:   message.Token,
		Data:    message.Data,
		Android: fcmAndroid{Priority: "high", CollapseKey: message.Tag},
	}})
	if err != nil {
		return fmt.Errorf("push: encode the message: %w", err)
	}
	token, err := s.tokens.Token()
	if err != nil {
		return fmt.Errorf("push: the sender credential did not mint a token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: build the send request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("push: send: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		// The answer names the message id and nothing this side needs.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
		return nil
	}
	refusal, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	if tokenGone(response.StatusCode, refusal) {
		return ErrTokenGone
	}
	return fmt.Errorf("push: FCM answered %d: %s", response.StatusCode, refusal)
}

// tokenGone reads Google's two spellings of "this registration is dead":
// the HTTP 404, and the `UNREGISTERED` code in the error details. Either
// alone is enough — the status is what a proxy would preserve and the code
// is what the API documents.
func tokenGone(status int, body []byte) bool {
	if status == http.StatusNotFound {
		return true
	}
	var refusal fcmError
	if err := json.Unmarshal(body, &refusal); err != nil {
		return false
	}
	for _, detail := range refusal.Error.Details {
		if detail.ErrorCode == "UNREGISTERED" {
			return true
		}
	}
	return false
}

var _ Sender = (*FCMSender)(nil)
