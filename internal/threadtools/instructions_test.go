package threadtools

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"agent-overflow/internal/provider"
)

// backticked pulls every `code span` out of the instructions. The leading
// identifier is what is checked: the text writes `notify: true`, and the
// parameter is notify.
var backticked = regexp.MustCompile("`([^`]+)`")

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]*`)

// resultVocabulary is the words the guide quotes that are values a RESULT
// carries rather than parameters or enums of a schema: request states,
// answer kinds, call outcomes, the two search result flags, a loop or
// watch's last outcome and a watch's undelivered output, what a held
// request waits on and an older paired computer's unreported rest. Every
// other backticked word must be a tool name, a parameter or an enum value.
var resultVocabulary = []string{
	"errors", "indexing", "last_message", "self",
	"reply", "final",
	"backgrounded", "blocked", "unconfirmed",
	"last_outcome", "last_output", "waiting_on", "rest_unreported",
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
	atLimit := soloShape()
	atLimit.Spawn = SpawnShape{Level: 1, Limit: 1}
	for _, shape := range []Shape{soloShape(), pairedShape(), atLimit} {
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

// TestTheDefaultsParagraphKeepsInheritingAndTheReadOnlyWarning: inheriting
// is still the default, so an agent does not deliberate over a choice the
// task does not need, and read-only is still named as the exception that
// blocks commands, not the cautious default.
func TestTheDefaultsParagraphKeepsInheritingAndTheReadOnlyWarning(t *testing.T) {
	for _, shape := range []Shape{soloShape(), pairedShape()} {
		text := instructionsDefaults(shape)
		for _, want := range []string{
			"Defaults. A spawn inherits your provider, model, effort, and runtime mode; keep them unless the task suits another agent.",
			"`read-only` is the exception, not the safe choice:",
			"Choose it only when the work needs nothing but reading files",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("paired=%v: the Defaults paragraph is missing %q:\n%s", shape.Paired(), want, text)
			}
		}
	}
}

// The profiles this computer can run are named in the guide by name, with
// how to spawn as one; their use-for lines stay in thread_options.
func TestTheDefaultsParagraphNamesTheProfilesThisComputerRuns(t *testing.T) {
	shape := soloShape()
	shape.Spawn = SpawnShape{Level: 0, Limit: 2, Providers: []string{"claude", "codex"}, Profiles: []string{"Opus", "Sol"}}
	text := instructionsFor(shape)
	for _, want := range []string{"pass `profile` with one of Opus and Sol", "`thread_options` lists each one's"} {
		if !strings.Contains(text, want) {
			t.Errorf("the guide is missing %q", want)
		}
	}
	shape.Spawn.Profiles = nil
	if text := instructionsFor(shape); strings.Contains(text, "`profile`") {
		t.Error("a computer that runs no profile still sends the agent to profile")
	}
}

// A computer with one provider never suggests another, and names what
// read-only does on its own provider alone; a paired one may offer the
// other, so it says where to find it.
func TestTheGuideReadsCorrectlyWithOneProvider(t *testing.T) {
	both := soloShape()
	both.Spawn = SpawnShape{Limit: 2, Providers: []string{"claude", "codex"}}
	if text := instructionsFor(both); !strings.Contains(text, "another provider or model suits the work better") {
		t.Error("with both providers the guide does not say another provider may suit the work")
	}
	for _, only := range []string{"claude", "codex"} {
		shape := soloShape()
		shape.Spawn = SpawnShape{Limit: 2, Providers: []string{only}}
		text := instructionsFor(shape)
		for _, forbidden := range []string{"another provider", "provider or model", "on Codex", "on Claude Code"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s only: the guide says %q", only, forbidden)
			}
		}
		if !strings.Contains(text, "another model suits the work better") {
			t.Errorf("%s only: the guide does not say another model may suit the work", only)
		}
		paired := pairedShape()
		paired.Spawn = shape.Spawn
		if text := instructionsFor(paired); !strings.Contains(text, "a provider another computer offers") {
			t.Errorf("%s only, paired: the guide does not say another computer may offer a provider", only)
		}
	}
}

// A session at the sub-thread limit has no thread_spawn, and its guide says
// nothing about spawning: no Defaults paragraph, a Starting paragraph that
// says why, no Waiting sentences on when a spawn ends, and no backticked
// name of a tool it lacks.
// One level is counted in the singular, as the limit of 1 makes every
// worker's guide say it.
func TestTheGuideCountsOneLevelInTheSingular(t *testing.T) {
	shape := soloShape()
	shape.Spawn = SpawnShape{Level: 1, Limit: 1, Providers: []string{"codex"}}
	if text := instructionsFor(shape); !strings.Contains(text, "You are 1 level of sub-threads below the thread the user talks to, the deepest allowed") {
		t.Fatalf("the guide at level 1 reads %q", text)
	}
}

// spawnEnds opens the Waiting paragraph's sentences on when a spawn ends,
// which only a session that can spawn reads.
const spawnEnds = "A thread you spawned, and a send to it, end only when it has nothing left running"

func TestASessionAtTheLimitHasNoSpawnAndReadsNoWordOfIt(t *testing.T) {
	for _, base := range []Shape{soloShape(), pairedShape()} {
		shape := base
		shape.Spawn = SpawnShape{Level: 2, Limit: 2, Providers: []string{"claude"}}
		if _, ok := toolsByName(t, shape)["thread_spawn"]; ok {
			t.Fatalf("paired=%v: a session at the limit lists thread_spawn", shape.Paired())
		}
		text := instructionsFor(shape)
		for _, forbidden := range []string{"`thread_spawn`", "Defaults.", "`profile`", "`instructions`", "`from_thread`", spawnEnds} {
			if strings.Contains(text, forbidden) {
				t.Errorf("paired=%v: the guide at the limit says %q", shape.Paired(), forbidden)
			}
		}
		if !strings.Contains(text, "You are 2 levels of sub-threads below the thread the user talks to, the deepest allowed") {
			t.Errorf("paired=%v: the guide at the limit does not say why it cannot spawn", shape.Paired())
		}
		shape.Spawn.Level = 1
		if _, ok := toolsByName(t, shape)["thread_spawn"]; !ok {
			t.Fatalf("paired=%v: a session one level above the limit has no thread_spawn", shape.Paired())
		}
		if !strings.Contains(instructionsFor(shape), "A thread you spawn is at the deepest level of sub-threads allowed") {
			t.Errorf("paired=%v: the spawner of the last level is not told its workers cannot spawn", shape.Paired())
		}
		if !strings.Contains(instructionsFor(shape), spawnEnds) {
			t.Errorf("paired=%v: a session that can spawn is not told when a spawn ends", shape.Paired())
		}
	}
}

// TestInstructionsAreParagraphs: the guide is read once per session, so it
// arrives as blank-line separated paragraphs in a fixed order.
func TestInstructionsAreParagraphs(t *testing.T) {
	solo := strings.Split(instructionsFor(soloShape()), "\n\n")
	paired := strings.Split(instructionsFor(pairedShape()), "\n\n")
	if len(solo) != 10 {
		t.Errorf("single-computer instructions have %d paragraphs, want 10", len(solo))
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

// jsLength is a string's length as Claude Code (JavaScript) counts it:
// UTF-16 code units.
func jsLength(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// TestInstructionsFitClaudesMCPDescriptionCap: Claude Code cuts MCP server
// instructions and each tool description at
// CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH and the model reads only the cut
// text. Claude sessions spawn with provider.ClaudeMCPDescriptionLengthLimit,
// so the guide and every description must fit under that value or the
// sections at the end silently stop reaching the model.
func TestInstructionsFitClaudesMCPDescriptionCap(t *testing.T) {
	limit := provider.ClaudeMCPDescriptionLengthLimit
	withProfiles := pairedShape()
	withProfiles.Spawn = SpawnShape{Level: 1, Limit: 2, Providers: []string{"claude", "codex"},
		Profiles: []string{"Astra", "Fable", "Sol", "Opus", "Sonnet", "Terra", "Haiku", "Luna"}}
	for _, shape := range []Shape{soloShape(), pairedShape(), withProfiles} {
		text := New(newFakeApp("laptop")).Instructions(shape)
		t.Logf("paired=%v: guide is %d characters", shape.Paired(), jsLength(text))
		if n := jsLength(text); n > limit {
			t.Errorf("paired=%v: the guide is %d characters, over the %d Claude sessions are given", shape.Paired(), n, limit)
		}
		for _, tool := range New(newFakeApp("laptop")).Tools(shape) {
			description, _ := tool["description"].(string)
			if n := jsLength(description); n > limit {
				t.Errorf("paired=%v: %s's description is %d characters, over %d", shape.Paired(), tool["name"], n, limit)
			}
		}
	}
}
