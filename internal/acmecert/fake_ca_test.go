package acmecert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

// fixedAccountKey is a P-256 key checked in so the challenge value below
// is a fixed vector rather than a fresh number every run. Nothing has
// ever registered it with a certificate authority; it exists to make the
// derivation reproducible.
const fixedAccountKey = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgeGMNS6YLu4Uu/aU6
XxTce5PMxOnR5xhExVaCBOhjssmhRANCAARcSkGGEfQ02IwsVrPu4JpWoX/hr/6K
tqJx4vZCpNW1d+VXFtfa+BqzceOA4Yo64ktLhTmtUui3QeclRc3Z1Sch
-----END PRIVATE KEY-----
`

// The challenge token and the TXT value it derives to under
// fixedAccountKey. Pinning the pair is what catches a token that got
// mangled on the way to the hook, and an account key that stopped
// round-tripping through its file — either of which produces a record the
// certificate authority reads as somebody else's.
const (
	fixedToken     = "w8d-fixed-token"
	fixedTXTRecord = "jsCFVPKivEhM6CbWzjb1Ij7WvTzq0hjZt1wEgP_0V4U"
)

// hookCall is one invocation the fake hook recorded.
type hookCall struct {
	action string
	fqdn   string
	value  string
}

// fakeCA drives the order flow without a network. Every field is a knob
// one test turns; the defaults are a CA that says yes to everything.
type fakeCA struct {
	t   *testing.T
	key crypto.Signer

	// steps records the order calls interleaved with the hook calls, so a
	// test can assert that the record was published BEFORE the CA was
	// asked to look at it.
	steps *[]string

	authorizationStatus string
	challengeType       string

	registerErr  error
	authorizeErr error
	acceptErr    error
	waitErr      error
	finalizeErr  error
}

func newFakeCA(t *testing.T, steps *[]string) *fakeCA {
	return &fakeCA{t: t, steps: steps, authorizationStatus: acme.StatusPending, challengeType: "dns-01"}
}

func (f *fakeCA) record(step string) { *f.steps = append(*f.steps, step) }

func (f *fakeCA) Register(context.Context, *acme.Account, func(string) bool) (*acme.Account, error) {
	f.record("register")
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return &acme.Account{}, nil
}

func (f *fakeCA) AuthorizeOrder(context.Context, []acme.AuthzID, ...acme.OrderOption) (*acme.Order, error) {
	f.record("order")
	if f.authorizeErr != nil {
		return nil, f.authorizeErr
	}
	return &acme.Order{
		URI:         "https://ca.invalid/order/1",
		AuthzURLs:   []string{"https://ca.invalid/authz/1"},
		FinalizeURL: "https://ca.invalid/order/1/finalize",
	}, nil
}

func (f *fakeCA) GetAuthorization(context.Context, string) (*acme.Authorization, error) {
	f.record("authorization")
	return &acme.Authorization{
		Status: f.authorizationStatus,
		Challenges: []*acme.Challenge{
			{Type: "http-01", Token: "not-this-one", URI: "https://ca.invalid/challenge/http"},
			{Type: f.challengeType, Token: fixedToken, URI: "https://ca.invalid/challenge/dns"},
		},
	}, nil
}

func (f *fakeCA) DNS01ChallengeRecord(token string) (string, error) {
	// The real derivation, over the account key the issuer loaded. That
	// is the point: the value the hook is handed has to be the one the
	// certificate authority will compute from the same key.
	return (&acme.Client{Key: f.key}).DNS01ChallengeRecord(token)
}

func (f *fakeCA) Accept(context.Context, *acme.Challenge) (*acme.Challenge, error) {
	f.record("accept")
	if f.acceptErr != nil {
		return nil, f.acceptErr
	}
	return &acme.Challenge{}, nil
}

func (f *fakeCA) WaitAuthorization(context.Context, string) (*acme.Authorization, error) {
	f.record("wait")
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return &acme.Authorization{Status: acme.StatusValid}, nil
}

func (f *fakeCA) CreateOrderCert(_ context.Context, _ string, csr []byte, _ bool) ([][]byte, string, error) {
	f.record("finalize")
	if f.finalizeErr != nil {
		return nil, "", f.finalizeErr
	}
	return [][]byte{signCSR(f.t, csr)}, "https://ca.invalid/cert/1", nil
}

// signCSR issues against the request the issuer built, so the certificate
// the flow ends with carries the names the flow asked for — which is what
// the "is this certificate actually for my domain" check reads.
func signCSR(t *testing.T, csrDER []byte) []byte {
	t.Helper()
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse the signing request the issuer built: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("the signing request is not signed by its own key: %v", err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a signing key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               csr.Subject,
		DNSNames:              csr.DNSNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, csr.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue against the signing request: %v", err)
	}
	return der
}

// issuance is one issuer wired to the fakes above, plus what they
// recorded. Production values for the CA and the hook are installed by
// New and replaced here, which is the only way to reach them: nothing
// outside this package can configure a double into a running app.
type issuance struct {
	issuer *Issuer
	// steps interleaves the order calls with the hook calls, so a test
	// can assert the record was published BEFORE the certificate
	// authority was asked to look at it.
	steps []string
	calls []hookCall
	// hookErr, when set, is what the hook returns for that action.
	hookErr map[string]error
}

func newIssuance(t *testing.T, dir string, prepare func(*fakeCA)) *issuance {
	t.Helper()
	issuer, err := New(Config{
		Dir:    dir,
		Domain: "backend.example",
		Hook:   []string{"dns-hook", "--zone", "example"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run := &issuance{issuer: issuer, hookErr: map[string]error{}}
	issuer.newCA = func(key crypto.Signer) CA {
		ca := newFakeCA(t, &run.steps)
		ca.key = key
		if prepare != nil {
			prepare(ca)
		}
		return ca
	}
	issuer.runHook = func(_ context.Context, argv []string, _ time.Duration, action, fqdn, value string) error {
		if len(argv) == 0 || argv[0] != "dns-hook" {
			t.Errorf("the hook was invoked as %v, not the configured argv", argv)
		}
		run.steps = append(run.steps, "hook "+action)
		run.calls = append(run.calls, hookCall{action: action, fqdn: fqdn, value: value})
		return run.hookErr[action]
	}
	return run
}

func seedAccountKey(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, AccountFileName), []byte(fixedAccountKey), 0o600); err != nil {
		t.Fatalf("seed the account key: %v", err)
	}
}

func requireStages(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
}

// requireNames fails unless the error exists and says which stage failed.
// Every failure path in this package is expected to name itself; a bare
// "issuance failed" is what this catches.
func requireNames(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a failure naming %q, got none", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not name %q", err, fragment)
	}
}
