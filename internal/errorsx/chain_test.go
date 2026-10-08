package errorsx

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestChainSplitsWrapLayers(t *testing.T) {
	cause := errors.New(`stored provider uuid "u1" is missing from session /tmp/a: b.jsonl`)
	err := fmt.Errorf("interrupt-and-revert: %w", fmt.Errorf("write rolled-back session: %w", fmt.Errorf("claude rollback: %w", cause)))
	want := []string{"interrupt-and-revert", "write rolled-back session", "claude rollback", cause.Error()}
	if got := Chain(err); !slices.Equal(got, want) {
		t.Fatalf("Chain = %q, want %q", got, want)
	}
}

type opaque struct{ inner error }

func (o opaque) Error() string { return "opaque failure" }
func (o opaque) Unwrap() error { return o.inner }

func TestChainKeepsALayerThatDoesNotEndWithItsCause(t *testing.T) {
	err := fmt.Errorf("outer: %w", opaque{inner: errors.New("hidden")})
	want := []string{"outer", "opaque failure"}
	if got := Chain(err); !slices.Equal(got, want) {
		t.Fatalf("Chain = %q, want %q", got, want)
	}
}

func TestChainFollowsJoinedBranches(t *testing.T) {
	err := fmt.Errorf("cleanup: %w", errors.Join(fmt.Errorf("remove a: %w", errors.New("busy")), errors.New("close b")))
	want := []string{"cleanup", "remove a", "busy", "close b"}
	if got := Chain(err); !slices.Equal(got, want) {
		t.Fatalf("Chain = %q, want %q", got, want)
	}
}

func TestChainIncludesPublicCause(t *testing.T) {
	err := Public("x", "Could not do it", fmt.Errorf("step: %w", errors.New("root")))
	want := []string{"Could not do it", "step", "root"}
	if got := Chain(err); !slices.Equal(got, want) {
		t.Fatalf("Chain = %q, want %q", got, want)
	}
}

func TestChainIsBounded(t *testing.T) {
	err := errors.New(strings.Repeat("é", chainMaxLayerLen))
	for range chainMaxLayers + 5 {
		err = fmt.Errorf("layer: %w", err)
	}
	got := Chain(err)
	if len(got) != chainMaxLayers {
		t.Fatalf("layers = %d, want %d", len(got), chainMaxLayers)
	}
	long := Chain(errors.New(strings.Repeat("é", chainMaxLayerLen)))[0]
	if !strings.HasSuffix(long, "…") || !utf8Valid(long) {
		t.Fatalf("long layer not truncated cleanly: %d bytes", len(long))
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestChainOfNil(t *testing.T) {
	if got := Chain(nil); got != nil {
		t.Fatalf("Chain(nil) = %q, want nil", got)
	}
}
