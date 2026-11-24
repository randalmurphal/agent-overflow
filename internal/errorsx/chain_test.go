package errorsx

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

type opaque struct{ inner error }

func (o opaque) Error() string { return "opaque failure" }
func (o opaque) Unwrap() error { return o.inner }

func TestChain(t *testing.T) {
	cause := errors.New(`stored provider uuid "u1" is missing from session /tmp/a: b.jsonl`)
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"splits wrap layers",
			fmt.Errorf("revert and resend: %w", fmt.Errorf("write rolled-back session: %w", fmt.Errorf("claude rollback: %w", cause))),
			[]string{"revert and resend", "write rolled-back session", "claude rollback", cause.Error()}},
		{"keeps a layer that does not end with its cause",
			fmt.Errorf("outer: %w", opaque{inner: errors.New("hidden")}),
			[]string{"outer", "opaque failure"}},
		{"follows joined branches",
			fmt.Errorf("cleanup: %w", errors.Join(fmt.Errorf("remove a: %w", errors.New("busy")), errors.New("close b"))),
			[]string{"cleanup", "remove a", "busy", "close b"}},
		{"includes public cause",
			Public("x", "Could not do it", fmt.Errorf("step: %w", errors.New("root"))),
			[]string{"Could not do it", "step", "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Chain(tc.err); !slices.Equal(got, tc.want) {
				t.Fatalf("Chain = %q, want %q", got, tc.want)
			}
		})
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
