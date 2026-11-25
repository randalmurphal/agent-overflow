package threadtools

import (
	"fmt"
	"strings"

	"agent-overflow/internal/agenthome"
)

// The nine tools that change something: spawn, send, ask, reply, status,
// cancel, update, group and remind.

func spawnSchema(shape Shape) map[string]any {
	defaults := shape.Defaults
	properties := map[string]any{
		"prompt": map[string]any{"type": "string", "minLength": 1},
		"title":  map[string]any{"type": "string", "maxLength": MaxTitleRunes},
		"from_thread": map[string]any{
			"type":        "string",
			"description": "Fork thread id (placed like any spawn; exclusive with workspace_path/worktree/base).",
		},
		"project_id":     map[string]any{"type": "string", "description": "Defaults to yours; required remotely."},
		"workspace_path": map[string]any{"type": "string"},
		"worktree": map[string]any{
			"type":        "string",
			"description": "Branch or tag to run on: reuses checked-out branch, checks out existing local/remote branch or tag (detached; refs/heads/ or refs/tags/), or creates new branch from base.",
		},
		"base": map[string]any{
			"type":        "string",
			"description": "Base branch or tag for new worktree (origin's head unless base_local). Refused for existing branches/tags.",
		},
		"base_local": map[string]any{"type": "boolean", "description": "Use local head without fetching origin."},
		"group": map[string]any{
			"type":        "string",
			"maxLength":   MaxTitleRunes,
			"description": "Sidebar group name (implies grouped; never for your workers).",
		},
		"placement": placementProperty("owned (default): your worker, no sidebar row. standalone/grouped: user-facing row."),
		"provider":  map[string]any{"type": "string", "description": spawnDefaultText("Provider", defaults.Provider)},
		"model":     map[string]any{"type": "string", "description": spawnDefaultText("Model or unversioned family (e.g. claude-opus, gpt-sol -> newest)", defaults.Model)},
		"effort":    map[string]any{"type": "string", "description": spawnDefaultText("Effort", defaults.Effort)},
		"mode":      map[string]any{"type": "string", "enum": []string{"chat", "plan"}, "description": spawnDefaultText("Mode", defaults.Mode)},
		"runtime_mode": map[string]any{
			"type":        "string",
			"enum":        runtimeModeEnum(),
			"description": spawnDefaultText("Permission mode", defaults.RuntimeMode) + " read-only blocks shell/writes; see thread_options.",
		},
	}
	properties["profile"] = map[string]any{
		"type":        "string",
		"description": profileDescription(shape.Spawn.Profiles),
	}
	properties["instructions"] = map[string]any{
		"type":        "string",
		"maxLength":   MaxSpawnInstructionsBytes,
		"description": "Persistent system prompt addition.",
	}
	if !shape.AssistantsOff {
		properties["home_files"] = homeFilesProperty("prompt")
	}
	waitProperties(properties, 0, shape.maxWaitSeconds(), "answer")
	computerIDProperty(shape, properties, "the project")
	description := "Spawn a new worker thread with prompt (returns thread_id and token). Inherits settings. Default is your worker with no sidebar row; pass placement standalone or group only for threads meant for the user."
	if shape.Paired() {
		description += " Remote spawns require computer_id and project_id."
	}
	return tool("thread_spawn", description, properties, "prompt")
}

// profileDescription states the profiles on this computer's providers
// beside the parameter (GuideProfileNames), so a spawn by profile needs no
// discovery call for the name.
func profileDescription(names []string) string {
	if len(names) > 0 {
		return "Profile (see thread_options): " + strings.Join(names, ", ") + "."
	}
	return "Profile (see thread_options)."
}

// homeFilesProperty is home_files on thread_spawn and thread_send.
func homeFilesProperty(field string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string", "minLength": 1},
		"minItems":    1,
		"maxItems":    agenthome.MaxHomeFiles,
		"description": fmt.Sprintf("Assistant home files prepended to %s.", field),
	}
}

// spawnDefaultText states the live default beside the parameter, so the
// common case needs no discovery call at all.
func spawnDefaultText(what, value string) string {
	if value == "" {
		return what + " (inherited)."
	}
	return fmt.Sprintf("%s (currently %s).", what, value)
}

func sendSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string"},
		"message":   map[string]any{"type": "string", "minLength": 1},
	}
	if !shape.AssistantsOff {
		properties["home_files"] = homeFilesProperty("message")
	}
	waitProperties(properties, 0, shape.maxWaitSeconds(), "answer")
	computerIDProperty(shape, properties, "the thread")
	description := "Queue a follow-up message on an existing thread" + pairedSuffix(shape, " (local or remote)") + " (not self) and return a token. Use thread_ask for read-only questions."
	return tool("thread_send", description, properties, "thread_id", "message")
}

func askSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string"},
		"question":  map[string]any{"type": "string", "minLength": 1},
	}
	maxWait := shape.maxWaitSeconds()
	defaultWait := min(DefaultAskWaitSeconds, maxWait)
	waitProperties(properties, defaultWait, maxWait, "answer")
	computerIDProperty(shape, properties, "the thread")
	description := fmt.Sprintf("Ask a one-shot question on a hidden read-only throwaway fork (never touches the real thread; not self). Waits %d seconds by default.", defaultWait)
	return tool("thread_ask", description, properties, "thread_id", "question")
}

func replySchema() map[string]any {
	properties := map[string]any{
		"token": map[string]any{"type": "string"},
		"text":  map[string]any{"type": "string", "minLength": 1, "description": "Self-contained answer or approval reason."},
		"decision": map[string]any{
			"type":        "string",
			"enum":        []string{DecisionAllow, DecisionDeny},
			"description": "Worker approval.",
		},
		"answers": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string", "minLength": 1},
			"minItems":    1,
			"maxItems":    MaxReplyAnswers,
			"description": "Multi-part worker question answers.",
		},
	}
	description := "Reply once to an Agent request token or worker question/approval wake."
	return tool("thread_reply", description, properties, "token", "text")
}

func statusSchema(shape Shape) map[string]any {
	maxWait := shape.maxWaitSeconds()
	properties := map[string]any{
		"tokens": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "string"},
			"minItems": 1,
			"maxItems": MaxStatusTokens,
		},
		"thread_ids": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "string"},
			"minItems": 1,
			"maxItems": MaxStatusTokens,
		},
		"wait_seconds": map[string]any{
			"type":    "integer",
			"minimum": 0,
			"maximum": maxWait,
			"default": 0,
		},
		"after_revision": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"description": "Skip settlements <= revision.",
		},
		"max_bytes": map[string]any{
			"type":    "integer",
			"minimum": MinShowBytes,
			"maximum": MaxShowBytes,
			"default": DefaultShowBytes,
		},
		"to_file": map[string]any{"type": "boolean", "default": false, "description": "Write full answer to file."},
		"cursor":  map[string]any{"type": "string"},
	}
	description := "Check/wait on request tokens or thread_ids until rest, or omit both to list active requests, loops and watches."
	return tool("thread_status", description, properties)
}

func cancelSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"token":     map[string]any{"type": "string"},
		"thread_id": map[string]any{"type": "string"},
		"label":     map[string]any{"type": "string", "description": "Loop or watch label."},
	}
	computerIDProperty(shape, properties, "the thread")
	description := "Stop work you started (pass one of token, thread_id, label). Interrupts without reverting."
	return tool("thread_cancel", description, properties)
}

func updateSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_ids": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "string"},
			"minItems": 1,
			"maxItems": MaxUpdateThreads,
		},
		"title":     map[string]any{"type": "string"},
		"archived":  map[string]any{"type": "boolean", "description": "Refused on self."},
		"pin":       map[string]any{"type": "string", "enum": []string{PinFront, PinBack, PinNone}},
		"group":     map[string]any{"type": []string{"string", "null"}, "description": "Group name (null ungroups)."},
		"placement": placementProperty("standalone, grouped, or owned (workers only)."),
	}
	description := "Batch-update threads (title, archived, pin, group, placement)."
	return tool("thread_update", description, properties, "thread_ids")
}

func groupSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"group":      map[string]any{"type": "string"},
		"group_id":   map[string]any{"type": "string"},
		"project_id": map[string]any{"type": "string"},
		"rename":     map[string]any{"type": "string", "maxLength": MaxTitleRunes},
		"delete":     map[string]any{"type": "boolean", "description": "Ungroups threads."},
	}
	computerIDProperty(shape, properties, "the group")
	description := "Rename or delete a thread group (pass group or group_id, and rename or delete)."
	return tool("thread_group", description, properties)
}

func remindSchema() map[string]any {
	properties := map[string]any{
		"after_seconds": map[string]any{"type": "integer", "minimum": 1},
		"at":            map[string]any{"type": "string", "description": "RFC 3339 wake time."},
		"note":          map[string]any{"type": "string", "minLength": 1, "maxLength": MaxNoteRunes, "description": "Brief wake cue (keep concise; thread keeps its context)."},
	}
	description := "Schedule a one-shot wake (pass after_seconds or at). Use instead of sleeping."
	return tool("thread_remind", description, properties, "note")
}

func pairedSuffix(shape Shape, text string) string {
	if shape.Paired() {
		return text
	}
	return ""
}

// placementProperty is placement on thread_spawn and thread_update.
func placementProperty(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"enum":        []string{PlacementOwned, PlacementGrouped, PlacementStandalone},
		"description": description,
	}
}
