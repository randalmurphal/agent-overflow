package acmecert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/acme"
)

// The happy path, and the ordering that makes it work: the record is
// published before the certificate authority is asked to read it, and
// removed once it has.
func TestIssuePublishesTheChallengeRecordThenRemovesIt(t *testing.T) {
	dir := t.TempDir()
	seedAccountKey(t, dir)
	run := newIssuance(t, dir, nil)

	material, err := run.issuer.Issue(context.Background())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	requireStages(t, run.steps,
		"register", "order", "authorization",
		"hook set", "accept", "wait", "hook clear",
		"finalize",
	)
	if len(run.calls) != 2 {
		t.Fatalf("hook calls = %v, want one set and one clear", run.calls)
	}
	for _, call := range run.calls {
		if call.fqdn != "_acme-challenge.backend.example" {
			t.Fatalf("hook %s named %q, want the _acme-challenge name for the domain", call.action, call.fqdn)
		}
		// The value travels on the clear too, so a hook can delete ONE
		// record rather than every TXT record at that name.
		if call.value != fixedTXTRecord {
			t.Fatalf("hook %s carried %q, want the value derived from the account key", call.action, call.value)
		}
	}
	if !material.Covers("backend.example") {
		t.Fatal("the issued certificate is not valid for the domain it was ordered for")
	}
}

// Persistence is what makes the issuance worth doing: the next boot
// serves this certificate instead of ordering another one.
func TestAnIssuedCertificateIsPersistedAndReloads(t *testing.T) {
	dir := t.TempDir()
	seedAccountKey(t, dir)
	run := newIssuance(t, dir, nil)

	issued, err := run.issuer.Issue(context.Background())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.Loaded() {
		t.Fatal("nothing was persisted")
	}
	if reloaded.Certificate.Leaf.SerialNumber.Cmp(issued.Certificate.Leaf.SerialNumber) != 0 {
		t.Fatal("the reloaded certificate is not the one that was issued")
	}
	if !reloaded.NotAfter.Equal(issued.NotAfter) {
		t.Fatalf("reloaded expiry %s, issued %s", reloaded.NotAfter, issued.NotAfter)
	}
	// The key came back with it, which is the half a chain-only file
	// would have lost.
	if reloaded.Certificate.PrivateKey == nil {
		t.Fatal("the persisted file carried no private key")
	}
	info, err := os.Stat(filepath.Join(dir, CertFileName))
	if err != nil {
		t.Fatalf("stat the certificate: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the certificate file is mode %o, want 600", mode)
	}
}

// Every failure after the record is published still removes it. A value
// left behind is one the user's zone carries forever, with a second one
// appearing beside it on the next attempt.
func TestTheChallengeRecordIsRemovedOnEveryFailurePath(t *testing.T) {
	refusal := errors.New("the certificate authority said no")
	tests := []struct {
		name    string
		prepare func(*fakeCA)
		hookErr error
		names   string
	}{
		{
			name:    "the hook could not publish it",
			hookErr: errors.New("zone is read-only"),
			names:   "publish the challenge record",
		},
		{
			name:    "the authority refused to look",
			prepare: func(ca *fakeCA) { ca.acceptErr = refusal },
			names:   "ask the certificate authority to validate",
		},
		{
			name:    "the authority did not validate it",
			prepare: func(ca *fakeCA) { ca.waitErr = refusal },
			names:   "did not validate",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			seedAccountKey(t, dir)
			run := newIssuance(t, dir, test.prepare)
			if test.hookErr != nil {
				run.hookErr[hookSet] = test.hookErr
			}

			_, err := run.issuer.Issue(context.Background())
			requireNames(t, err, test.names)

			var cleared bool
			for _, call := range run.calls {
				if call.action == hookClear && call.value == fixedTXTRecord {
					cleared = true
				}
			}
			if !cleared {
				t.Fatalf("the challenge record was left behind; hook calls were %v", run.calls)
			}
			if _, statErr := os.Stat(filepath.Join(dir, CertFileName)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("a failed issuance wrote a certificate file")
			}
		})
	}
}

// The stages before the record exists fail without asking a hook to
// remove one, and each says which stage it was.
func TestAFailureBeforeTheRecordNamesItsStage(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*fakeCA)
		names   string
	}{
		{
			name:    "the account could not be registered",
			prepare: func(ca *fakeCA) { ca.registerErr = errors.New("unreachable") },
			names:   "register the ACME account",
		},
		{
			name:    "the order could not be opened",
			prepare: func(ca *fakeCA) { ca.authorizeErr = errors.New("rate limited") },
			names:   "open an order",
		},
		{
			name:    "no dns-01 challenge was offered",
			prepare: func(ca *fakeCA) { ca.challengeType = "tls-alpn-01" },
			names:   "no dns-01 challenge",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			seedAccountKey(t, dir)
			run := newIssuance(t, dir, test.prepare)

			_, err := run.issuer.Issue(context.Background())
			requireNames(t, err, test.names)
			if len(run.calls) != 0 {
				t.Fatalf("the hook ran %v before there was a record to publish", run.calls)
			}
		})
	}
}

// A failed finalize is still a failure that names itself, and it happens
// after the record is already gone.
func TestAFailedFinalizeNamesItselfAndPersistsNothing(t *testing.T) {
	dir := t.TempDir()
	seedAccountKey(t, dir)
	run := newIssuance(t, dir, func(ca *fakeCA) { ca.finalizeErr = errors.New("bad CSR") })

	_, err := run.issuer.Issue(context.Background())
	requireNames(t, err, "finalize the order")
	if _, statErr := os.Stat(filepath.Join(dir, CertFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("a failed finalize wrote a certificate file")
	}
}

// A certificate authority that still remembers a recent validation is the
// common case on a renewal. Publishing a record for it would be work
// nobody asked for, and a zone edit the user would see.
func TestAnAlreadyValidAuthorizationPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	seedAccountKey(t, dir)
	run := newIssuance(t, dir, func(ca *fakeCA) { ca.authorizationStatus = acme.StatusValid })

	if _, err := run.issuer.Issue(context.Background()); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	requireStages(t, run.steps, "register", "order", "authorization", "finalize")
	if len(run.calls) != 0 {
		t.Fatalf("the hook ran %v for an authorization that was already valid", run.calls)
	}
}

// The account key is the identity every renewal is made under, and the
// value the challenge derives from. A key regenerated between two
// issuances would strand the first account and change every record.
func TestTheAccountKeyIsMintedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	run := newIssuance(t, dir, nil)
	if _, err := run.issuer.Issue(context.Background()); err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, AccountFileName))
	if err != nil {
		t.Fatalf("read the account key: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("no account key was persisted")
	}
	firstValue := run.calls[0].value

	second := newIssuance(t, dir, nil)
	if _, err := second.issuer.Issue(context.Background()); err != nil {
		t.Fatalf("second Issue: %v", err)
	}
	again, err := os.ReadFile(filepath.Join(dir, AccountFileName))
	if err != nil {
		t.Fatalf("re-read the account key: %v", err)
	}
	if string(again) != string(first) {
		t.Fatal("the second issuance replaced the account key")
	}
	if second.calls[0].value != firstValue {
		t.Fatal("two issuances under one account key produced different challenge values")
	}
}

// An account key file that cannot be read is not overwritten: the account
// it names may hold live authorizations, and a fresh one silently taking
// its place would make an unreadable file look like a working renewal.
func TestAnUnreadableAccountKeyIsReportedRatherThanReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AccountFileName)
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	run := newIssuance(t, dir, nil)

	_, err := run.issuer.Issue(context.Background())
	requireNames(t, err, "does not hold a usable ACME account key")

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(stored) != "not a key" {
		t.Fatal("the unreadable account key was overwritten")
	}
}

// New refuses a configuration that cannot produce an order, at the point
// it is built rather than at the renewal that needed it.
func TestNewRefusesAConfigurationThatCannotOrder(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		names string
	}{
		{
			name:  "nowhere to keep the result",
			cfg:   Config{Domain: "backend.example", Hook: []string{"hook"}},
			names: "no directory",
		},
		{
			name:  "no domain",
			cfg:   Config{Dir: "/tmp", Hook: []string{"hook"}},
			names: "no domain",
		},
		{
			name:  "a URL rather than a domain",
			cfg:   Config{Dir: "/tmp", Domain: "https://backend.example/", Hook: []string{"hook"}},
			names: "not a bare domain name",
		},
		{
			name:  "no way to answer the challenge",
			cfg:   Config{Dir: "/tmp", Domain: "backend.example"},
			names: "no DNS hook command",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.cfg)
			requireNames(t, err, test.names)
		})
	}
}

// The domain is normalized once, at construction, so the challenge name
// and the certificate's names cannot disagree about its spelling.
func TestNewNormalizesTheDomain(t *testing.T) {
	issuer, err := New(Config{Dir: t.TempDir(), Domain: "  Backend.Example  ", Hook: []string{"hook"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if issuer.Domain() != "backend.example" {
		t.Fatalf("Domain() = %q, want the trimmed lower-cased spelling", issuer.Domain())
	}
}
