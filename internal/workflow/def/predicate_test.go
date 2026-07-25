package def

import "testing"

func TestEvaluatePredicateStandalone(t *testing.T) {
	vars := map[string]any{
		"trigger":   map[string]any{"kind": "cron", "scheduled-for": 1700000000000},
		"job-notes": "carry over the failing suite",
		"count":     3,
	}
	cases := []struct {
		name      string
		predicate Predicate
		want      bool
	}{
		{
			name:      "equal on a dotted reference",
			predicate: Predicate{Eq: &Comparison{Ref: "trigger.kind", Value: "cron"}},
			want:      true,
		},
		{
			name:      "absent reference is false, not an error",
			predicate: Predicate{Eq: &Comparison{Ref: "trigger.missing", Value: "cron"}},
			want:      false,
		},
		{
			name:      "exists on a present name",
			predicate: Predicate{Exists: "job-notes"},
			want:      true,
		},
		{
			name: "all short-circuits on a false child",
			predicate: Predicate{All: []Predicate{
				{Gte: &Comparison{Ref: "count", Value: 3}},
				{Eq: &Comparison{Ref: "trigger.kind", Value: "event"}},
			}},
			want: false,
		},
		{
			name: "any matches the second child",
			predicate: Predicate{Any: []Predicate{
				{Eq: &Comparison{Ref: "trigger.kind", Value: "event"}},
				{Eq: &Comparison{Ref: "trigger.kind", Value: "cron"}},
			}},
			want: true,
		},
		{
			name:      "not inverts",
			predicate: Predicate{Not: &Predicate{Exists: "nothing"}},
			want:      true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := EvaluatePredicate(testCase.predicate, vars)
			if err != nil {
				t.Fatalf("EvaluatePredicate() error = %v", err)
			}
			if result != testCase.want {
				t.Fatalf("EvaluatePredicate() = %v, want %v", result, testCase.want)
			}
		})
	}
}

func TestEvaluatePredicateErrors(t *testing.T) {
	cases := []struct {
		name      string
		predicate Predicate
	}{
		{name: "no operator", predicate: Predicate{}},
		{
			name:      "two operators",
			predicate: Predicate{Exists: "a", Eq: &Comparison{Ref: "a", Value: 1}},
		},
		{name: "empty all", predicate: Predicate{All: []Predicate{}}},
		{
			name:      "ordered comparison against a string",
			predicate: Predicate{Gt: &Comparison{Ref: "name", Value: 1}},
		},
	}
	vars := map[string]any{"name": "release"}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := EvaluatePredicate(testCase.predicate, vars); err == nil {
				t.Fatal("EvaluatePredicate() error = nil, want an error")
			}
		})
	}
}

func TestValidatePredicateShape(t *testing.T) {
	cases := []struct {
		name      string
		predicate Predicate
		codes     []string
	}{
		{
			name:      "well formed",
			predicate: Predicate{Eq: &Comparison{Ref: "trigger.kind", Value: "cron"}},
		},
		{
			name:      "no operator",
			predicate: Predicate{},
			codes:     []string{"predicate.operator"},
		},
		{
			name:      "empty in values",
			predicate: Predicate{In: &Membership{Ref: "trigger.kind"}},
			codes:     []string{"predicate.in"},
		},
		{
			// A nested node is reached only because the parent is structurally
			// sound; the recursion is what makes a deep typo visible.
			name: "malformed child of any",
			predicate: Predicate{Any: []Predicate{
				{Exists: "a"},
				{Exists: "b", Eq: &Comparison{Ref: "b", Value: 1}},
			}},
			codes: []string{"predicate.operator"},
		},
		{
			name:      "malformed child of not",
			predicate: Predicate{Not: &Predicate{}},
			codes:     []string{"predicate.operator"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			findings := ValidatePredicateShape(testCase.predicate, "condition")
			if len(findings) != len(testCase.codes) {
				t.Fatalf("findings = %v, want codes %v", findings, testCase.codes)
			}
			for index, code := range testCase.codes {
				if findings[index].Code != code {
					t.Fatalf("findings[%d].Code = %q, want %q", index, findings[index].Code, code)
				}
			}
		})
	}
}
