package def

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoopBound is a loop route's `max:`. It is either a literal count authored in
// the definition (`max: 2`) or a reference into the run's variable context
// (`max: fix-budget`), so a campaign can seed its own budget at run start
// instead of editing the YAML for every run. Exactly one form is set; the zero
// value is undeclared, which is what "max and feedback are valid only on loop
// routes" tests and what a loop route without a bound is refused for.
//
// The two forms are distinguished by YAML/JSON node type alone — an integer is
// a literal, a string is a reference — because a bound has no other spelling a
// scalar could mean. Frozen run snapshots persisted before the reference form
// existed carry `"max": 2` as a plain JSON integer and are decoded but never
// re-validated, so the JSON decoder accepts both shapes and each marshals back
// as what it was authored as.
type LoopBound struct {
	Literal int
	Ref     string
}

// LiteralBound is the authored-count form.
func LiteralBound(count int) LoopBound { return LoopBound{Literal: count} }

// RefBound is the seeded form: the bound is whatever `ref` resolves to when the
// gate evaluates.
func RefBound(ref string) LoopBound { return LoopBound{Ref: ref} }

// IsZero reports an undeclared bound. It is what `json:",omitzero"` reads, so a
// route that declares no `max:` persists without the key exactly as it did when
// the field was a plain int.
func (b LoopBound) IsZero() bool { return b.Literal == 0 && b.Ref == "" }

// Resolve answers the bound this route actually gets for one gate evaluation.
// A reference is read through LookupVariable — the same lookup a predicate's
// reference goes through — so a seeded budget and a routing condition can never
// disagree about what a variable holds, and a numeric value in any of the
// forms a decoded envelope carries (int, float64, json.Number, a big literal)
// is accepted through the same conversion predicate comparison uses.
//
// The result must be a whole count of at least one. An absent, fractional,
// negative, or zero bound is an error rather than a coerced value: treating it
// as zero would silently exhaust the loop on its first traversal, and treating
// it as one would invent a budget the author never wrote.
func (b LoopBound) Resolve(vars map[string]any) (int, error) {
	if b.Ref == "" {
		if b.Literal < 1 {
			return 0, fmt.Errorf("loop max %d must be >= 1", b.Literal)
		}
		return b.Literal, nil
	}
	ref := strings.TrimSpace(b.Ref)
	if ref == "" {
		return 0, errors.New("loop max reference is blank")
	}
	value, present := LookupVariable(vars, ref)
	if !present {
		return 0, fmt.Errorf("loop max reference %q does not resolve", ref)
	}
	rational, numeric := number(value)
	if !numeric {
		return 0, fmt.Errorf("loop max reference %q has non-numeric value %T", ref, value)
	}
	resolved, ok := wholeCount(rational)
	if !ok {
		return 0, fmt.Errorf("loop max reference %q resolved to %s; it must be a whole number >= 1", ref, rational.RatString())
	}
	return resolved, nil
}

// wholeCount narrows a rational to a positive Go int, refusing the fractional
// and out-of-range values a variable can legitimately hold but a loop budget
// cannot be.
func wholeCount(value *big.Rat) (int, bool) {
	if !value.IsInt() {
		return 0, false
	}
	numerator := value.Num()
	if !numerator.IsInt64() {
		return 0, false
	}
	count := numerator.Int64()
	if count < 1 || int64(int(count)) != count {
		return 0, false
	}
	return int(count), true
}

func (b *LoopBound) UnmarshalYAML(node *yaml.Node) error {
	*b = LoopBound{}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("max must be a whole number or a variable reference")
	}
	switch node.Tag {
	case "!!int":
		return node.Decode(&b.Literal)
	case "!!str":
		b.Ref = node.Value
		return nil
	default:
		return fmt.Errorf("max must be a whole number or a variable reference, not %s", node.Tag)
	}
}

func (b LoopBound) MarshalYAML() (any, error) {
	if b.Ref != "" {
		return b.Ref, nil
	}
	return b.Literal, nil
}

func (b *LoopBound) UnmarshalJSON(data []byte) error {
	*b = LoopBound{}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &b.Ref)
	}
	if err := json.Unmarshal(trimmed, &b.Literal); err != nil {
		return fmt.Errorf("max must be a whole number or a variable reference: %w", err)
	}
	return nil
}

func (b LoopBound) MarshalJSON() ([]byte, error) {
	if b.Ref != "" {
		return json.Marshal(b.Ref)
	}
	return json.Marshal(b.Literal)
}

// loopBoundShapeFindings checks what a `max:` is without a variable context: a
// literal is at least one, and a reference is a non-empty name. Whether a
// reference resolves, and to what, is checked with the other reference
// resolution in validate_vars.go, which is where the dominance graph lives.
//
// `subject` names the route half being bounded, so one message shape serves
// both a loop route and a human route's reject.
func loopBoundShapeFindings(bound LoopBound, element, subject string) []Finding {
	if bound.Ref != "" {
		if strings.TrimSpace(bound.Ref) == "" {
			return []Finding{finding("gate.loop-max", element, fmt.Sprintf("%s requires a non-empty max reference", subject))}
		}
		return nil
	}
	if bound.Literal < 1 {
		return []Finding{finding("gate.loop-max", element, fmt.Sprintf("%s requires max >= 1", subject))}
	}
	return nil
}
