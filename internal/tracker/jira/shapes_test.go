package jira

import (
	"regexp"
	"strings"
	"testing"
)

// shapeExpressions are the regular expressions the matchers in shapes.go
// replace, compiled when a test asks, never at init.
func shapeExpressions() map[string]struct {
	re    *regexp.Regexp
	match func(string) bool
} {
	type pair = struct {
		re    *regexp.Regexp
		match func(string) bool
	}
	return map[string]pair{
		"numeric":   {regexp.MustCompile(`^[0-9]+$`), isNumeric},
		"key name":  {regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`), isKeyName},
		"issue key": {regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`), isIssueKey},
		"media ID":  {regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`), isMediaID},
	}
}

var textOperatorsExpression = `\b(AND|OR|NOT)\b`

// shapeInputs are the edges of each shape.
var shapeInputs = []string{
	"", "0", "10001", "1a", "a1", " 1", "1\n", "١", "WEB", "web_2", "_WEB", "2WEB", "WEB-", "WEB-1", "WEB-01", "WEB-1-2", "-1", "W-E-1",
	"WEB-1\n", "WÉB-1", "customfield_10001", "summary", "field-x",
	"0f1c2a3b-0000-4000-8000-000000000001", "0F1C2A3B-0000-4000-8000-00000000000A", "0f1c2a3b-0000-4000-8000-00000000000g",
	"0f1c2a3b00000-4000-8000-000000000001", "0f1c2a3b-0000-4000-8000-0000000000011", "{0f1c2a3b-0000-4000-8000-000000000001}",
	"AND", "OR", "NOT", "and", "ANDY", "SANDAL", "A AND B", "AND OR NOT", "x_AND", "AND_x", "1AND", "ANDNOT", "(AND)", "éAND", "ANDé",
	"NOT-A-CHANCE OR maybe", "ORANGE OR", "OROR", "AND\nOR", "a AND b AND c",
}

// Each matcher matches exactly what its expression matches.
func TestShapesMatchTheirExpressions(t *testing.T) {
	ops := regexp.MustCompile(textOperatorsExpression)
	for name, s := range shapeExpressions() {
		for _, in := range shapeInputs {
			if got, want := s.match(in), s.re.MatchString(in); got != want {
				t.Errorf("%s(%q) = %v, the expression says %v", name, in, got, want)
			}
		}
	}
	for _, in := range shapeInputs {
		if got, want := lowerTextOperators(in), ops.ReplaceAllStringFunc(in, strings.ToLower); got != want {
			t.Errorf("lowerTextOperators(%q) = %q, the expression gives %q", in, got, want)
		}
	}
	if n := testing.AllocsPerRun(10, func() { _ = lowerTextOperators("plain words and more") }); n != 0 && !raceOn {
		t.Errorf("text with no operator costs %v allocations", n)
	}
}

func FuzzShapes(f *testing.F) {
	for _, in := range shapeInputs {
		f.Add(in)
	}
	exprs := shapeExpressions()
	ops := regexp.MustCompile(textOperatorsExpression)
	f.Fuzz(func(t *testing.T, in string) {
		for name, s := range exprs {
			if got, want := s.match(in), s.re.MatchString(in); got != want {
				t.Errorf("%s(%q) = %v, the expression says %v", name, in, got, want)
			}
		}
		if got, want := lowerTextOperators(in), ops.ReplaceAllStringFunc(in, strings.ToLower); got != want {
			t.Errorf("lowerTextOperators(%q) = %q, the expression gives %q", in, got, want)
		}
	})
}

// BenchmarkSystemField is the lookup of each field of a board row page's
// issue, against the map it replaced.
func BenchmarkSystemField(b *testing.B) {
	ids := []string{"summary", "status", "issuetype", "assignee", "priority", "project", "labels", "parent", "created", "updated",
		"resolutiondate", "customfield_10020", "customfield_10019", "customfield_10016"}
	b.Run("switch", func(b *testing.B) {
		for b.Loop() {
			for _, id := range ids {
				_ = systemField(id)
			}
		}
	})
	b.Run("map", func(b *testing.B) {
		m := map[string]fieldDecoder{}
		for _, id := range []string{"summary", "labels", "status", "issuetype", "priority", "assignee", "reporter", "parent", "resolution",
			"project", "description", "created", "updated", "duedate", "components", "fixVersions", "attachment", "subtasks", "issuelinks"} {
			m[id] = systemField(id)
		}
		for b.Loop() {
			for _, id := range ids {
				_ = m[id]
			}
		}
	})
}

// BenchmarkShapes is each matcher against its expression.
func BenchmarkShapes(b *testing.B) {
	for name, s := range shapeExpressions() {
		in := map[string]string{"numeric": "1000123", "key name": "customfield_10001", "issue key": "WEB-12345",
			"media ID": "0f1c2a3b-0000-4000-8000-000000000001"}[name]
		b.Run(name+"/matcher", func(b *testing.B) {
			for b.Loop() {
				_ = s.match(in)
			}
		})
		b.Run(name+"/expression", func(b *testing.B) {
			for b.Loop() {
				_ = s.re.MatchString(in)
			}
		})
	}
	ops := regexp.MustCompile(textOperatorsExpression)
	text := "find the login page OR the sign in form"
	b.Run("text operators/matcher", func(b *testing.B) {
		for b.Loop() {
			_ = lowerTextOperators(text)
		}
	})
	b.Run("text operators/expression", func(b *testing.B) {
		for b.Loop() {
			_ = ops.ReplaceAllStringFunc(text, strings.ToLower)
		}
	})
}
