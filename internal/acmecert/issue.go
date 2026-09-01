package acmecert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"agent-overflow/internal/atomicfile"
)

// directoryURL is the certificate authority every production issuance
// goes to. A constant rather than a setting: the CA decides what a
// certificate MEANS, so pointing this at another one is not a preference
// a user can express through a settings field somebody might later fill
// from the wire. Tests replace the whole client (newCA) and reach no
// network at all.
const directoryURL = acme.LetsEncryptURL

// CA is the slice of *acme.Client this package drives. Declared here, on
// the consumer's side, so the order flow can be tested against a fake
// without a network and without an ACME server.
//
// It is narrow on purpose: every method on it is one this package calls,
// and a method appearing here means a new step in the flow below.
type CA interface {
	Register(ctx context.Context, account *acme.Account, acceptTOS func(tosURL string) bool) (*acme.Account, error)
	AuthorizeOrder(ctx context.Context, id []acme.AuthzID, opt ...acme.OrderOption) (*acme.Order, error)
	GetAuthorization(ctx context.Context, url string) (*acme.Authorization, error)
	DNS01ChallengeRecord(token string) (string, error)
	Accept(ctx context.Context, challenge *acme.Challenge) (*acme.Challenge, error)
	WaitAuthorization(ctx context.Context, url string) (*acme.Authorization, error)
	CreateOrderCert(ctx context.Context, url string, csr []byte, bundle bool) ([][]byte, string, error)
}

// Config is what an issuance needs. Every field is required.
type Config struct {
	// Dir is the app's config root: where the account key and the issued
	// certificate live.
	Dir string

	// Domain is the canonical domain the certificate is for. Exactly one:
	// a certificate covering names the user did not ask for is a
	// certificate the CA logs under names they did not ask for.
	Domain string

	// Hook is the argv of the command that publishes and removes the
	// challenge TXT record. See the package doc for its contract.
	Hook []string

	// HookTimeout bounds ONE hook invocation. Zero means
	// DefaultHookTimeout.
	HookTimeout time.Duration
}

// Issuer runs orders for one domain. Build it with New; the zero value
// has no CA and no hook runner and does nothing.
type Issuer struct {
	dir         string
	domain      string
	hook        []string
	hookTimeout time.Duration

	// newCA and runHook are the two seams the tests replace. Production
	// values are set by New and are the only ones any caller outside this
	// package can get, so a test double cannot be configured into a
	// running app.
	newCA   func(key crypto.Signer) CA
	runHook hookRunner
}

// New validates the configuration and returns an issuer for it. The
// checks are the preconditions of an ORDER — there is somewhere to keep
// the result, there is a name to ask about, there is a way to answer the
// challenge — not the user-facing spelling rules, which
// internal/settings applies at the edit that introduced them.
func New(cfg Config) (*Issuer, error) {
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		return nil, errors.New("acmecert: no directory to keep the account key and certificate in")
	}
	domain := strings.ToLower(strings.TrimSpace(cfg.Domain))
	if domain == "" {
		return nil, errors.New("acmecert: no domain to issue a certificate for")
	}
	if strings.ContainsAny(domain, "/:? *") {
		return nil, fmt.Errorf("acmecert: %q is not a bare domain name", domain)
	}
	if len(cfg.Hook) == 0 || strings.TrimSpace(cfg.Hook[0]) == "" {
		return nil, errors.New("acmecert: no DNS hook command configured, so nothing can answer the challenge")
	}
	timeout := cfg.HookTimeout
	if timeout <= 0 {
		timeout = DefaultHookTimeout
	}
	return &Issuer{
		dir:         dir,
		domain:      domain,
		hook:        append([]string(nil), cfg.Hook...),
		hookTimeout: timeout,
		newCA: func(key crypto.Signer) CA {
			return &acme.Client{Key: key, DirectoryURL: directoryURL}
		},
		runHook: runHook,
	}, nil
}

// Domain is the name this issuer orders for, normalized.
func (i *Issuer) Domain() string { return i.domain }

// Issue runs one complete order and persists what it gets.
//
// The stages, each of which names itself in its error: account key,
// account registration, order, authorization, challenge record, hook set,
// validation, finalize, persist, hook clear. Nothing here is retried
// internally — a caller that wants another attempt calls again, on its
// own schedule, so backoff lives in one place instead of two.
func (i *Issuer) Issue(ctx context.Context) (Material, error) {
	key, err := i.accountKey()
	if err != nil {
		return Material{}, err
	}
	ca := i.newCA(key)

	// Registering a key that is already registered is the ordinary case
	// on every renewal after the first, and the CA says so rather than
	// failing. Any other refusal is real: an order made under an account
	// the CA does not recognise cannot succeed.
	if _, err := ca.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil &&
		!errors.Is(err, acme.ErrAccountAlreadyExists) {
		return Material{}, fmt.Errorf("acmecert: register the ACME account for %s: %w", i.domain, err)
	}

	order, err := ca.AuthorizeOrder(ctx, acme.DomainIDs(i.domain))
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: open an order for %s: %w", i.domain, err)
	}
	for _, authzURL := range order.AuthzURLs {
		if err := i.satisfy(ctx, ca, authzURL); err != nil {
			return Material{}, err
		}
	}

	material, err := i.finalize(ctx, ca, order.FinalizeURL)
	if err != nil {
		return Material{}, err
	}
	if err := Persist(i.dir, material.Certificate); err != nil {
		// Loud, and fatal to the call: a certificate held only in memory
		// would be re-issued on the next boot, and the CA's weekly limit
		// is what a restart loop would spend.
		return Material{}, err
	}
	log.Printf("acmecert: issued a certificate for %s, valid until %s",
		i.domain, material.NotAfter.Format(time.RFC3339))
	return material, nil
}

// satisfy answers one authorization by publishing the DNS-01 record,
// telling the CA to look, and waiting for its verdict.
//
// The record is REMOVED whatever happens, including on the failure paths,
// because a challenge value left behind is a record the user's zone
// carries forever and a second one appears beside it on the next attempt.
func (i *Issuer) satisfy(ctx context.Context, ca CA, authzURL string) error {
	authz, err := ca.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("acmecert: read the authorization for %s: %w", i.domain, err)
	}
	if authz.Status == acme.StatusValid {
		// The CA still remembers a recent validation for this name. Doing
		// the dance again would publish a record for nothing.
		return nil
	}
	challenge := dns01(authz)
	if challenge == nil {
		return fmt.Errorf("acmecert: the certificate authority offered no dns-01 challenge for %s", i.domain)
	}
	value, err := ca.DNS01ChallengeRecord(challenge.Token)
	if err != nil {
		return fmt.Errorf("acmecert: compute the challenge record for %s: %w", i.domain, err)
	}

	fqdn := ChallengePrefix + i.domain
	// The clear is armed BEFORE the set, not after it succeeds. A hook
	// that failed part-way may still have published the record, and a
	// value left behind is one the user's zone carries forever with a
	// second one appearing beside it on the next attempt. Clearing a
	// record that was never written is the cheap half of that trade.
	defer func() {
		// A separate context: the clear must still run when the reason we
		// are leaving is that ctx was cancelled, which is exactly when a
		// record would otherwise be stranded.
		clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), i.hookTimeout)
		defer cancel()
		if err := i.runHook(clearCtx, i.hook, i.hookTimeout, hookClear, fqdn, value); err != nil {
			log.Printf("acmecert: could not remove the challenge record %s: %v — remove it by hand", fqdn, err)
		}
	}()
	if err := i.runHook(ctx, i.hook, i.hookTimeout, hookSet, fqdn, value); err != nil {
		return fmt.Errorf("acmecert: publish the challenge record for %s: %w", i.domain, err)
	}

	if _, err := ca.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("acmecert: ask the certificate authority to validate %s: %w", fqdn, err)
	}
	if _, err := ca.WaitAuthorization(ctx, authzURL); err != nil {
		return fmt.Errorf("acmecert: the certificate authority did not validate %s: %w", fqdn, err)
	}
	return nil
}

// finalize generates the certificate's own key, asks for the certificate,
// and checks that what came back is one this backend can actually serve
// for the domain it asked about.
//
// A FRESH key per issuance, never the account key and never the previous
// certificate's: the account key signs orders and must not also be the
// key a TLS peer talks to, and a renewal that reused its predecessor's
// key would make every certificate this backend ever served
// interchangeable.
func (i *Issuer) finalize(ctx context.Context, ca CA, finalizeURL string) (Material, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: generate the certificate key for %s: %w", i.domain, err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: i.domain},
		DNSNames: []string{i.domain},
	}, key)
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: build the signing request for %s: %w", i.domain, err)
	}
	// bundle: the intermediates come with it. A leaf alone is a chain a
	// browser has to complete itself, and a browser that cannot is a
	// trust warning on the one path this feature exists to remove.
	chain, _, err := ca.CreateOrderCert(ctx, finalizeURL, csr, true)
	if err != nil {
		return Material{}, fmt.Errorf("acmecert: finalize the order for %s: %w", i.domain, err)
	}
	material, err := materialFrom(tlsCertificate(chain, key))
	if err != nil {
		return Material{}, err
	}
	if !material.Covers(i.domain) {
		return Material{}, fmt.Errorf("acmecert: the issued certificate is not valid for %s", i.domain)
	}
	return material, nil
}

// accountKey loads the ACME account key, generating and persisting one on
// first use.
//
// It is persisted for a reason that is easy to miss: the key is what the
// challenge value is DERIVED from, so a key regenerated between the
// record being published and the CA reading it produces a value that
// cannot validate — and a key regenerated per boot would also leave a
// trail of orphan accounts at the CA.
func (i *Issuer) accountKey() (crypto.Signer, error) {
	path := filepath.Join(i.dir, AccountFileName)
	stored, err := os.ReadFile(path)
	if err == nil {
		key, parseErr := parseAccountKey(stored)
		if parseErr == nil {
			return key, nil
		}
		// Not overwritten: the account it names may hold valid
		// authorizations, and a new one silently taking its place would
		// make an unreadable file look like a working renewal.
		return nil, fmt.Errorf("acmecert: %s does not hold a usable ACME account key: %w", path, parseErr)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("acmecert: read %s: %w", path, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("acmecert: generate an ACME account key: %w", err)
	}
	encoded, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(path, encoded); err != nil {
		return nil, fmt.Errorf("acmecert: persist %s: %w", path, err)
	}
	return key, nil
}

// dns01 picks the DNS-01 challenge out of an authorization. The CA offers
// several types; this package can only answer one, and says so by name
// when it is absent rather than attempting whichever came first.
func dns01(authz *acme.Authorization) *acme.Challenge {
	if authz == nil {
		return nil
	}
	for _, challenge := range authz.Challenges {
		if challenge.Type == "dns-01" {
			return challenge
		}
	}
	return nil
}

func parseAccountKey(stored []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(stored)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("a %T cannot sign", parsed)
	}
	return key, nil
}

func encodeKey(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("acmecert: marshal the private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
