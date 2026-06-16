package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

// stubProvider is a controllable updater.Provider for verifiedProvider tests.
type stubProvider struct {
	rel *updater.Release
	err error
}

func (s stubProvider) Name() string { return "stub" }

func (s stubProvider) Check(context.Context, updater.CheckRequest) (*updater.Release, error) {
	return s.rel, s.err
}

func (s stubProvider) Download(context.Context, *updater.Release, io.Writer, func(written, total int64)) error {
	return nil
}

func TestVerifiedProviderFailsClosedWithoutVerification(t *testing.T) {
	// A release with no Verification block must be rejected: installing it
	// would skip integrity checking entirely (the stock GitHub provider's
	// silent fall-open when the checksum sidecar is missing).
	p := verifiedProvider{inner: stubProvider{rel: &updater.Release{Version: "1.2.3"}}}
	rel, err := p.Check(context.Background(), updater.CheckRequest{})
	if err == nil {
		t.Fatal("expected error for release without verification, got nil")
	}
	if rel != nil {
		t.Fatalf("expected nil release on rejection, got %+v", rel)
	}
}

func TestVerifiedProviderFailsClosedWithEmptyDigest(t *testing.T) {
	// A Verification block present but carrying an empty digest is still
	// unverifiable and must be rejected.
	p := verifiedProvider{inner: stubProvider{rel: &updater.Release{
		Version:      "1.2.3",
		Verification: &updater.Verification{DigestAlgo: "sha256", Digest: nil},
	}}}
	if _, err := p.Check(context.Background(), updater.CheckRequest{}); err == nil {
		t.Fatal("expected error for release with empty digest, got nil")
	}
}

func TestVerifiedProviderPassesThroughVerifiedRelease(t *testing.T) {
	want := &updater.Release{
		Version:      "1.2.3",
		Verification: &updater.Verification{DigestAlgo: "sha256", Digest: []byte{0x01, 0x02}},
	}
	p := verifiedProvider{inner: stubProvider{rel: want}}
	got, err := p.Check(context.Background(), updater.CheckRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("expected the verified release passed through unchanged, got %+v", got)
	}
}

func TestVerifiedProviderUpToDateIsNotAnError(t *testing.T) {
	// A nil release means "already up to date" — it must pass through without
	// error so the Updater reports up-to-date rather than failing the check.
	p := verifiedProvider{inner: stubProvider{rel: nil}}
	rel, err := p.Check(context.Background(), updater.CheckRequest{})
	if err != nil {
		t.Fatalf("unexpected error on up-to-date: %v", err)
	}
	if rel != nil {
		t.Fatalf("expected nil release, got %+v", rel)
	}
}

func TestVerifiedProviderPropagatesCheckError(t *testing.T) {
	sentinel := errors.New("network boom")
	p := verifiedProvider{inner: stubProvider{err: sentinel}}
	if _, err := p.Check(context.Background(), updater.CheckRequest{}); !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying check error to propagate, got %v", err)
	}
}

func TestVerifiedProviderForwardsName(t *testing.T) {
	p := verifiedProvider{inner: stubProvider{}}
	if got := p.Name(); got != "stub" {
		t.Fatalf("Name() = %q, want %q", got, "stub")
	}
}

// TestUpdaterRPCsUnsupportedWhenNil verifies the RPC surface degrades cleanly
// on builds without a configured updater (the headless WSL backend and tests):
// no panics, and the unsupported state is reported rather than thrown for the
// read paths.
func TestUpdaterRPCsUnsupportedWhenNil(t *testing.T) {
	a := &App{} // no updater configured

	avail, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: unexpected error: %v", err)
	}
	if avail.Supported {
		t.Fatal("CheckForUpdate: expected Supported=false when updater is nil")
	}

	if _, err := a.ListReleases(); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("ListReleases: want ErrUpdatesUnsupported, got %v", err)
	}
	if err := a.DownloadUpdate(""); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("DownloadUpdate: want ErrUpdatesUnsupported, got %v", err)
	}
	if err := a.RestartToUpdate(); !errors.Is(err, ErrUpdatesUnsupported) {
		t.Fatalf("RestartToUpdate: want ErrUpdatesUnsupported, got %v", err)
	}
}
