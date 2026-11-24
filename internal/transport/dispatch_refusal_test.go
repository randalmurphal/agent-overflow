package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/dispatchrefusal"
)

// The codes the wording reads are the codes the dispatcher sends.
func TestDispatchRefusalCodesAreTheWireCodes(t *testing.T) {
	if dispatchrefusal.CodeMethodNotFound != ErrCodeMethodNotFound || dispatchrefusal.CodeBadParams != ErrCodeBadParams {
		t.Fatalf("dispatchrefusal codes %q %q, want %q %q", dispatchrefusal.CodeMethodNotFound, dispatchrefusal.CodeBadParams, ErrCodeMethodNotFound, ErrCodeBadParams)
	}
}

// refusalApp declares the arguments the refusals below disagree with.
type refusalApp struct{}

func (refusalApp) TakesCount(n int) error       { return nil }
func (refusalApp) TakesThing(t echoThing) error { return nil }

// echoThing rejects every value, repeating it, as a careless decoder would.
type echoThing struct{}

func (*echoThing) UnmarshalJSON(raw []byte) error { return fmt.Errorf("not a thing: %s", raw) }

// A call this computer does not have, and a call whose arguments do not
// fit what it declares, are refused before any method runs with the
// sentence a person can act on, so a reader already shipped shows it, and
// with the dispatch mark that tells every current reader who refused. An
// argument that does not decode keeps its log reference, and nothing it
// sent reaches the frame.
func TestTheDispatcherMarksItsOwnRefusal(t *testing.T) {
	logs := captureLog(t)
	d := NewDispatcher()
	if _, err := d.Register(&refusalApp{}, RegisterOptions{Package: "main", TypeName: "App"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, fe := d.ResolveForOrigin(0, "AddedLater", true); fe == nil || fe.Code != ErrCodeMethodNotFound || fe.Message != dispatchrefusal.Unsupported ||
		fe.Detail != nil || !reflect.DeepEqual(fe.Dispatch, &dispatchrefusal.Refusal{Kind: dispatchrefusal.KindUnsupported}) {
		t.Fatalf("missing method = %+v, want method_not_found, the Unsupported sentence, and the unsupported mark", fe)
	}
	count, fe := d.ResolveForOrigin(0, "TakesCount", true)
	if fe != nil {
		t.Fatalf("resolve TakesCount: %+v", fe)
	}
	thing, fe := d.ResolveForOrigin(0, "TakesThing", true)
	if fe != nil {
		t.Fatalf("resolve TakesThing: %+v", fe)
	}
	for _, tc := range []struct {
		name     string
		m        *Method
		params   []json.RawMessage
		want     dispatchrefusal.Refusal
		detailed bool
	}{
		{"one argument more than declared", count, []json.RawMessage{json.RawMessage(`1`), json.RawMessage(`2`)},
			dispatchrefusal.Refusal{Kind: "unreadable", Method: "TakesCount", Problem: "expected 1 arguments, got 2"}, false},
		{"one argument fewer", count, nil,
			dispatchrefusal.Refusal{Kind: "unreadable", Method: "TakesCount", Problem: "expected 1 arguments, got 0"}, false},
		{"more arguments than any call takes", count, make([]json.RawMessage, MaxRPCParams+1),
			dispatchrefusal.Refusal{Kind: "unreadable", Method: "TakesCount", Problem: fmt.Sprintf("expected at most %d arguments, got %d", MaxRPCParams, MaxRPCParams+1)}, false},
		{"an argument of another type", count, []json.RawMessage{json.RawMessage(`"seven"`)},
			dispatchrefusal.Refusal{Kind: "unreadable", Method: "TakesCount", Param: 1, Problem: "expected int, got string"}, true},
		{"an argument its decoder rejects", thing, []json.RawMessage{json.RawMessage(`"secret-value"`)},
			dispatchrefusal.Refusal{Kind: "unreadable", Method: "TakesThing", Param: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fe := d.Invoke(context.Background(), tc.m, tc.params)
			if fe == nil || fe.Code != ErrCodeBadParams || fe.Message != dispatchrefusal.Unreadable || (fe.Detail != nil) != tc.detailed ||
				fe.Dispatch == nil || *fe.Dispatch != tc.want {
				t.Fatalf("refusal = %+v (dispatch %+v), want bad_params, the Unreadable sentence, dispatch %+v, detail %v", fe, fe.Dispatch, tc.want, tc.detailed)
			}
			wire, err := json.Marshal(fe)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(wire), "secret-value") || strings.Contains(string(wire), "seven") {
				t.Fatalf("the frame repeats what the caller sent: %s", wire)
			}
		})
	}
	if !strings.Contains(logs.String(), "not a thing: \"secret-value\"") {
		t.Fatalf("the decoder's text is not in the log: %s", logs.String())
	}
}
