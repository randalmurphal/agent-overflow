package threadtools

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// backticked pulls every `code span` out of the instructions. The leading
// identifier is what is checked: the text writes `notify: true`, and the
// parameter is notify.
var backticked = regexp.MustCompile("`([^`]+)`")

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]*`)

// resultVocabulary is the words the guide quotes that are values a RESULT
// carries rather than parameters or enums of a schema: request states,
// answer kinds, call outcomes and the two search result flags. Every other
// backticked word must be a tool name, a parameter or an enum value.
var resultVocabulary = []string{
	"errors", "indexing",
	"reply", "final",
	"backgrounded", "blocked", "unconfirmed",
}

func schemaVocabulary(t *testing.T, shape Shape) (names, properties, enums []string) {
	t.Helper()
	for _, tool := range New(newFakeApp("laptop")).Tools(shape) {
		names = append(names, tool["name"].(string))
		for property, raw := range schemaProperties(t, tool) {
			properties = append(properties, property)
			enums = append(enums, enumValues(raw)...)
		}
	}
	return names, properties, enums
}

func enumValues(raw any) []string {
	property, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	if values, ok := property["enum"].([]string); ok {
		out = append(out, values...)
	}
	if items, ok := property["items"].(map[string]any); ok {
		out = append(out, enumValues(items)...)
	}
	return out
}

// TestInstructionsOnlyNameThingsThatExist: the guide is the decision
// guide, so every tool and parameter it sends the model to has to be in
// that shape's schemas. A parameter dropped from a schema and left in the
// text would send an agent to a tool call that cannot be made.
func TestInstructionsOnlyNameThingsThatExist(t *testing.T) {
	for _, shape := range []Shape{soloShape(), pairedShape()} {
		names, properties, enums := schemaVocabulary(t, shape)
		text := instructionsFor(shape)
		for _, match := range backticked.FindAllStringSubmatch(text, -1) {
			word := identifier.FindString(match[1])
			if word == "" {
				continue
			}
			switch {
			case slices.Contains(names, word):
			case slices.Contains(properties, word), slices.Contains(enums, word), slices.Contains(resultVocabulary, word):
			case strings.HasPrefix(word, "thread_"):
				t.Errorf("paired=%v: the instructions name tool %s, which is not in the tool list", shape.Paired(), word)
			default:
				t.Errorf("paired=%v: the instructions name %q, which is neither a parameter, an enum value nor result vocabulary", shape.Paired(), word)
			}
		}
	}
}

// TestInstructionsMentionEveryTool: a tool the guide never names is a
// tool the model has to discover on its own.
func TestInstructionsMentionEveryTool(t *testing.T) {
	text := instructionsFor(pairedShape())
	for _, name := range ToolNames {
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("the instructions never mention %s", name)
		}
	}
}

// TestSoloInstructionsSayNothingAboutComputers is the load-bearing half of
// amendment 3: with no pairing, nothing about other computers exists.
func TestSoloInstructionsSayNothingAboutComputers(t *testing.T) {
	text := instructionsFor(soloShape())
	for _, forbidden := range []string{"Other computers.", "`computers`", "`computer_id`", "paired computers", "another computer"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the single-computer instructions contain %q", forbidden)
		}
	}
	if !strings.Contains(text, "on this computer, the way the user would from the sidebar") {
		t.Error("the single-computer opening paragraph does not say \"on this computer\"")
	}
}

// TestPairedInstructionsCarryTheOtherComputersParagraph is the other half.
func TestPairedInstructionsCarryTheOtherComputersParagraph(t *testing.T) {
	text := instructionsFor(pairedShape())
	for _, want := range []string{"Other computers.", "`computers`", "`computer_id`", "on the user's other paired computers"} {
		if !strings.Contains(text, want) {
			t.Errorf("the paired instructions are missing %q", want)
		}
	}
}

// TestInstructionsKeepTheDefaultsParagraphVerbatim: amendment 11's wording
// is the point of the paragraph. Agents given a choice deliberate over it,
// and this is the text that forbids the deliberation.
func TestInstructionsKeepTheDefaultsParagraphVerbatim(t *testing.T) {
	want := "Defaults. A spawn inherits your provider, model, effort, and runtime mode. Keep them unless the task needs something else: a different provider or model for a second opinion, or `read-only` when the work is certainly reading and nothing more. Do not choose `read-only` \"to be safe\"; a thread that needs to write and cannot will fail and tell you so. `thread_ask` is always read-only and needs no choice. `thread_options` lists what a computer offers when you need something you do not have."
	for _, shape := range []Shape{soloShape(), pairedShape()} {
		if !strings.Contains(instructionsFor(shape), want) {
			t.Errorf("paired=%v: the Defaults paragraph is not the amendment 11 text", shape.Paired())
		}
	}
}

// TestInstructionsAreParagraphs: the guide is read once per session, so it
// arrives as blank-line separated paragraphs in a fixed order.
func TestInstructionsAreParagraphs(t *testing.T) {
	solo := strings.Split(instructionsFor(soloShape()), "\n\n")
	paired := strings.Split(instructionsFor(pairedShape()), "\n\n")
	if len(solo) != 9 {
		t.Errorf("single-computer instructions have %d paragraphs, want 9", len(solo))
	}
	if len(paired) != len(solo)+1 {
		t.Errorf("paired instructions have %d paragraphs, want one more than %d", len(paired), len(solo))
	}
	if !strings.HasPrefix(paired[len(paired)-1], "Ids are UUIDs") {
		t.Errorf("the last paragraph is %q, want the id rule", paired[len(paired)-1])
	}
	if !strings.HasPrefix(paired[len(paired)-2], "Other computers.") {
		t.Errorf("the Other computers paragraph is not next to last: %q", paired[len(paired)-2])
	}
}

// TestServerInstructionsMatchTheAssembledText pins the exported entry
// point to the same text the tests check.
func TestServerInstructionsMatchTheAssembledText(t *testing.T) {
	server := New(newFakeApp("laptop"))
	for _, shape := range []Shape{soloShape(), pairedShape()} {
		if server.Instructions(shape) != instructionsFor(shape) {
			t.Errorf("paired=%v: Instructions does not return the assembled text", shape.Paired())
		}
	}
}
