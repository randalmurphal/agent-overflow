package triage

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// Live 2.1.284 captures (docs/references/fixtures/claude/README.md). A
// forked skill runs as a local_agent task bound to its Skill call and
// ends with a task_notification alone (claude-wire.md §E9).
const (
	fixtureForkedSkillTaskStarted        = "../../docs/references/fixtures/claude/forked_skill_task_started_20261014.ndjson"
	fixtureForkedSkillStopTask           = "../../docs/references/fixtures/claude/forked_skill_stop_task_20261014.ndjson"
	fixtureForkedSkillInterrupt          = "../../docs/references/fixtures/claude/forked_skill_interrupt_20261014.ndjson"
	fixtureForkedSkillBackgroundChildren = "../../docs/references/fixtures/claude/forked_skill_background_children_20261014.ndjson"
	fixtureAgentBackgroundedMidrun       = "../../docs/references/fixtures/claude/local_agent_backgrounded_midrun_20261014.ndjson"
)

func replayedItem(t *testing.T, st *store.Store, id string) store.Item {
	t.Helper()
	item, found, err := st.GetThreadItem("t1", id)
	if err != nil || !found {
		t.Fatalf("item %s: found=%v err=%v", id, found, err)
	}
	return item
}

func requireNoItem(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, found, err := st.GetThreadItem("t1", id); err != nil || found {
		t.Fatalf("item %s: found=%v err=%v, want none", id, found, err)
	}
}

// The Skill row is an awaited launch: it settles in place on its own
// result, whether the fork finished, was stopped by its task id, or was
// cut off by an interrupt. It never joins the background set, so nothing
// writes a completion sibling for it.
func TestForkedSkillTaskSettlesTheSkillRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		skill   string
		status  string
	}{
		{"completed", fixtureForkedSkillTaskStarted, "toolu_01YPMQv1H356djgueDWN7TbT", statusCompleted},
		{"stop_task", fixtureForkedSkillStopTask, "toolu_01WzQJmHfkXq8U1KSsYTEdGo", statusErrored},
		{"interrupt", fixtureForkedSkillInterrupt, "toolu_014zopYoeDRcTkpwUa6ExouN", statusErrored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := replayCapture(t, tc.fixture)
			skill := replayedItem(t, st, tc.skill)
			if skill.Status != tc.status || skill.IsBackground {
				t.Fatalf("Skill row = status %q background %v, want %q in place", skill.Status, skill.IsBackground, tc.status)
			}
			if TaskIDFromItemMeta(skill.Meta) == "" {
				t.Fatalf("Skill row has no task id to stop it by: %s", skill.Meta)
			}
			requireNoItem(t, st, ToolCompletionID(tc.skill))
		})
	}
}

// What a fork leaves behind follows the CLI: its background shell dies
// with it, and an async agent it launched outlives it. That agent's stop
// files at top level, as an Agent's orphaned child's does, because the
// fork's card is a snapshot that ends at the fork's own stop.
func TestForkedSkillBackgroundChildrenOutliveTheFork(t *testing.T) {
	const (
		skill      = "toolu_01P6zTMpbMg6cDwyJcwVbMxJ"
		shell      = "toolu_01TYPNtRhkY3QQPANHHvYby3"
		asyncChild = "toolu_01HZmA8duqxECEJooBeiiFLg"
	)
	st, _ := replayCapture(t, fixtureForkedSkillBackgroundChildren)
	if fork := replayedItem(t, st, skill); fork.Status != statusCompleted || fork.IsBackground {
		t.Fatalf("Skill row = status %q background %v, want completed in place", fork.Status, fork.IsBackground)
	}
	if stop := replayedItem(t, st, ToolCompletionID(shell)); stop.Status != statusKilled || stop.ParentID != skill {
		t.Fatalf("shell stop = status %q parent %q, want killed under the fork", stop.Status, stop.ParentID)
	}
	if stop := replayedItem(t, st, ToolCompletionID(asyncChild)); stop.Status != statusCompleted || stop.ParentID != "" {
		t.Fatalf("async child stop = status %q parent %q, want completed at top level", stop.Status, stop.ParentID)
	}
}

// An awaited Agent moved to the background mid-run keeps its label when
// the session mirror takes over its rows, and the backgrounding write
// stores no served card keys (storetest's cleanup check fails the test if
// it does).
func TestBackgroundedAgentKeepsItsLabel(t *testing.T) {
	const agent = "toolu_01UzSaE8xZC8Lm39DYG46AFG"
	st, _ := replayCapture(t, fixtureAgentBackgroundedMidrun)
	launch := replayedItem(t, st, agent)
	if !launch.IsBackground || !strings.HasPrefix(launch.Summary, "Agent: ") {
		t.Fatalf("backgrounded Agent = background %v summary %q, want its Agent label", launch.IsBackground, launch.Summary)
	}
	if stop := replayedItem(t, st, ToolCompletionID(agent)); stop.Status != statusCompleted || !strings.HasPrefix(stop.Summary, "Agent: ") {
		t.Fatalf("backgrounded Agent stop = status %q summary %q", stop.Status, stop.Summary)
	}
}
