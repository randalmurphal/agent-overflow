package threadtools

import "fmt"

// The nine tools that change something: spawn, send, ask, reply, status,
// cancel, update, group and remind.

func spawnSchema(shape Shape) map[string]any {
	defaults := shape.Defaults
	properties := map[string]any{
		"prompt": map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "The first user message of the new thread. Write it so the new thread can act without seeing your conversation: it starts with no context but the prompt, and the history from_thread gives it.",
		},
		"title": map[string]any{
			"type":        "string",
			"maxLength":   MaxTitleRunes,
			"description": "Optional title for the sidebar. Omitted, the app titles the thread itself from its first turn.",
		},
		"from_thread": map[string]any{
			"type":        "string",
			"description": "Fork this thread's history at its tail first, then send prompt there, for trying a second approach without disturbing the original. The fork is a normal visible thread. It runs on the source thread's computer, in its project and workspace; every other setting still defaults to yours. A source mid-turn is forked at its tail with that turn settled as interrupted.",
		},
		"project_id": map[string]any{
			"type":        "string",
			"description": "Project for the new thread. Defaults to yours. Projects are registered per computer, so a spawn on another computer must name one of that computer's project ids; omitting it there is refused with that computer's projects and worktrees listed.",
		},
		"workspace_path": map[string]any{
			"type":        "string",
			"description": "Checkout to run in, as the OWNING computer spells it. Defaults to your workspace locally. Use worktree instead to cut a fresh one.",
		},
		"worktree": map[string]any{
			"type":        "string",
			"description": "Branch name of a fresh worktree to run in instead of an existing checkout. The worktree is cut from the project through the same draft-worktree path the sidebar's new-worktree draft uses; on another computer it is cut from project_id's repository there. Pass workspace_path or worktree, not both.",
		},
		"provider": map[string]any{
			"type":        "string",
			"description": spawnDefaultText("Provider for the new thread", defaults.Provider) + " A provider given without a model uses that provider's default model. A provider the target computer does not offer is refused with the list it does; thread_options lists them.",
		},
		"model": map[string]any{
			"type":        "string",
			"description": spawnDefaultText("Model slug for the new thread", defaults.Model) + " A model the target computer does not offer is refused with the list it does; thread_options lists them per provider.",
		},
		"effort": map[string]any{
			"type":        "string",
			"description": spawnDefaultText("Reasoning effort", defaults.Effort) + " Valid values differ per model; thread_options marks each model's default.",
		},
		"mode": map[string]any{
			"type":        "string",
			"enum":        []string{"chat", "plan"},
			"description": spawnDefaultText("What the new thread does: chat works, plan proposes a plan first", defaults.Mode),
		},
		"runtime_mode": map[string]any{
			"type":        "string",
			"enum":        runtimeModeEnum(),
			"description": spawnDefaultText("Permission level of the new thread, a separate axis from mode", defaults.RuntimeMode) + " " + runtimeModeSentence() + ".",
		},
	}
	waitProperties(properties, 0, "the new thread to answer")
	computerIDProperty(shape, properties, "the project the thread should run in")
	description := "Open a new visible sidebar thread, send prompt as its first user message, start its turn, and return the thread id and a request token. It inherits your project, workspace, provider, model, effort, mode and runtime mode unless you override them. The new thread is an ordinary thread with no special marking: the user sees it in the sidebar and can take it over. Use it when the user asks for a separate thread, when another provider or model should do the work, or when the work should be visible and outlive your turn; use your own subagents for pieces of your current task."
	if shape.Paired() {
		description += " With computer_id the thread is created on that computer, runs on its account, appears in its sidebar, and project_id must name one of its projects."
	}
	return tool("thread_spawn", description, properties, "prompt")
}

// spawnDefaultText states the live default beside the parameter, so the
// common case needs no discovery call at all.
func spawnDefaultText(what, value string) string {
	if value == "" {
		return what + ". Omit to inherit yours."
	}
	return fmt.Sprintf("%s. Omit to inherit yours, currently %s.", what, value)
}

func sendSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string", "description": "The thread to continue. Full id, or an unambiguous prefix of at least 6 characters. You cannot send to your own thread; use thread_remind to wake yourself."},
		"message":   map[string]any{"type": "string", "minLength": 1, "description": "Exactly what the user would have typed there. The receiving agent sees it as a user message with a footer naming this thread and the token, so write it to be read without your context."},
	}
	waitProperties(properties, 0, "that thread to answer")
	computerIDProperty(shape, properties, "the thread")
	description := "Continue an existing thread as if the user had typed message there. A thread mid-turn queues it for the turn boundary with the user's draft preserved; an idle thread receives it now and its session starts lazily. Works across projects" + pairedSuffix(shape, " and computers") + ". Returns a request token. Use send to give a thread work; use thread_ask to consult one without touching it."
	return tool("thread_send", description, properties, "thread_id", "message")
}

func askSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_id": map[string]any{"type": "string", "description": "The thread whose context you want to consult. Full id, or an unambiguous prefix of at least 6 characters. You cannot ask your own thread."},
		"question":  map[string]any{"type": "string", "minLength": 1, "description": "The question. Ask for exactly what you need: the copy answers once and is then gone, so a clarifying question back costs you another call."},
	}
	waitProperties(properties, DefaultAskWaitSeconds, "the answer")
	computerIDProperty(shape, properties, "the thread")
	description := fmt.Sprintf("Ask a one-shot question of a thread's context. The target is forked at its tail, in-flight turn included, into a hidden throwaway copy forced to the read-only runtime mode whatever the source runs; the question goes there and the copy is deleted as soon as its answer is stored. The real thread is never touched and never sees the question. Read-only means the copy refuses writes and mutating commands immediately rather than waiting on a person, so it always comes back; if it needed a write to answer it says so. Waits %d seconds by default. If the answer is a clarifying question, ask again or switch to thread_send on the real thread.", DefaultAskWaitSeconds)
	if shape.Paired() {
		description += " A thread on another computer is forked and answered there; only the answer crosses."
	}
	return tool("thread_ask", description, properties, "thread_id", "question")
}

func replySchema() map[string]any {
	properties := map[string]any{
		"token": map[string]any{"type": "string", "description": "The token from the \"Agent request\" footer of the message you are answering. It resolves to one pending request on this computer; a token from anywhere else is refused."},
		"text":  map[string]any{"type": "string", "minLength": 1, "description": "Your answer. The sender sees only this text and none of your thread, so make it self-contained."},
	}
	description := "Answer the agent in another thread that sent you a message. Call it once, when an answer is due. One reply per token: a retry with the same token and the same text returns the existing acceptance, a different second reply is refused, and an unknown token is an error. If you end your turn without replying, the sender receives your final text marked as not a reply, and a reply after that still reaches it as a follow-up. Your reply is stored where you are, so it succeeds even when the sender's computer is unreachable right now."
	return tool("thread_reply", description, properties, "token", "text")
}

func statusSchema() map[string]any {
	properties := map[string]any{
		"tokens": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"minItems":    1,
			"maxItems":    MaxStatusTokens,
			"description": fmt.Sprintf("Up to %d request tokens from your own spawns, sends, asks and reminders. Duplicates are refused. Each row returns the request's state, the target thread, the answer kind and the answer.", MaxStatusTokens),
		},
		"thread_ids": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"minItems":    1,
			"maxItems":    MaxStatusTokens,
			"description": fmt.Sprintf("Up to %d thread ids to watch instead of tokens, including threads you never messaged. Each row returns that thread's live state. Duplicates and your own thread are refused.", MaxStatusTokens),
		},
		"wait_seconds": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"maximum":     MaxWaitSeconds,
			"default":     0,
			"description": fmt.Sprintf("Wait up to this long, returning as soon as ANY listed request settles, any listed thread rests, or any target becomes blocked on a person, with the request still open. Zero reads the current state and returns. Maximum %d.", MaxWaitSeconds),
		},
		"after_revision": map[string]any{
			"type":        "integer",
			"minimum":     0,
			"description": "Skip settlements you have already seen. Every request carries a revision that increases on each settlement and on a late reply, so passing the revision you last read is how you wait for a late reply on a request that already finished.",
		},
		"max_bytes": map[string]any{
			"type":        "integer",
			"minimum":     MinShowBytes,
			"maximum":     MaxShowBytes,
			"default":     DefaultShowBytes,
			"description": "Inline budget for the answers in this reply. A longer answer returns a cursor to continue, or use to_file.",
		},
		"to_file": map[string]any{"type": "boolean", "default": false, "description": "Write a single request's whole answer to a file and return its path instead of inline text. Requires exactly one token."},
		"cursor":  map[string]any{"type": "string", "description": "Opaque cursor from a previous thread_status result, passed back unchanged to continue a long answer or a listing."},
	}
	description := "Re-attach to the requests this thread made, or watch any thread. With tokens it returns each request's state (unconfirmed, accepted, running, blocked, replied, finished, errored, cancelled, interrupted, expired or refused), the target, the answer kind (reply, final, error or note) and the answer. With thread_ids it returns those threads' live states, so you can wait for a thread the user is running without having sent it anything. With neither it lists this thread's requests, open ones first then newest first, which is how you recover your tokens after losing context. An answer collected here IS the delivery, so no message is queued for it; a message already queued still lands and the reply says so. The answer stays available for the request's lifetime even after the thread that wrote it is gone."
	return tool("thread_status", description, properties)
}

func cancelSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"token":     map[string]any{"type": "string", "description": "Cancel this request of yours: a message still queued is removed from the queue, a turn that request's message started is interrupted, a reminder is dropped. A turn the user or another request started is never interrupted through a token."},
		"thread_id": map[string]any{"type": "string", "description": "Interrupt this thread's running turn. Allowed only for a thread you spawned, sent to or asked; any other thread is refused."},
	}
	computerIDProperty(shape, properties, "the thread")
	description := "Stop work you started. Pass exactly one of token or thread_id. This interrupts, it never reverts: nothing a thread already did is undone. A cancelled request settles cancelled and its answer, if any, is what it had at the time."
	return tool("thread_cancel", description, properties)
}

func updateSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"thread_ids": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"minItems":    1,
			"maxItems":    MaxUpdateThreads,
			"description": fmt.Sprintf("One to %d threads to change, by full id or unambiguous prefix. The same patch applies to all of them and the result reports each id separately, so one refusal does not stop the rest.", MaxUpdateThreads),
		},
		"title":    map[string]any{"type": "string", "description": "New title. It is trimmed, and a title that is empty after trimming is refused."},
		"archived": map[string]any{"type": "boolean", "description": "Archive or unarchive. Archiving the thread you are in is refused."},
		"pin": map[string]any{
			"type":        "string",
			"enum":        []string{PinFront, PinBack, PinNone},
			"description": "Pin tier: front or back burner, or none to unpin. A grouped thread cannot be pinned on its own, because its group carries the pin; ungroup it first or pin the group with thread_group.",
		},
		"group": map[string]any{
			"type":        []string{"string", "null"},
			"description": "Group name inside the thread's own project, created when it does not exist. null ungroups. A thread joins groups only on its own computer, and a group of the same name in another project is a different group. Cannot be set together with pin.",
		},
	}
	description := "Organize threads the way the sidebar does: rename, archive, pin to the front or back burner, or group them, many at once. Set at least one of title, archived, pin or group. The whole patch is checked against each thread before anything is touched, so a thread is either fully updated or untouched with a reason. Do this when the user asks, or for threads you spawned once you are done with them."
	if shape.Paired() {
		description += " Threads on other computers are grouped by computer and applied there; one unreachable computer fails only its own ids."
	}
	return tool("thread_update", description, properties, "thread_ids")
}

func groupSchema(shape Shape) map[string]any {
	properties := map[string]any{
		"group":      map[string]any{"type": "string", "description": "Group name within the project. Use this or group_id."},
		"group_id":   map[string]any{"type": "string", "description": "Group id from thread_options. Use this or group."},
		"project_id": map[string]any{"type": "string", "description": "Project the group belongs to. Defaults to your own project when the group is named rather than given by id; a group is per project."},
		"rename":     map[string]any{"type": "string", "maxLength": MaxTitleRunes, "description": "New name for the group."},
		"pin":        map[string]any{"type": "string", "enum": []string{PinFront, PinBack, PinNone}, "description": "Pin the group to the front or back burner, or none to unpin it. The group's pin is what its threads show."},
		"delete":     map[string]any{"type": "boolean", "description": "Delete the group. Its threads are ungrouped, not deleted, exactly as the sidebar does it."},
	}
	computerIDProperty(shape, properties, "the group")
	description := "Rename, delete or pin one thread group. Name it with group plus project_id, or with group_id. Pass exactly one of rename, pin or delete."
	return tool("thread_group", description, properties)
}

func remindSchema() map[string]any {
	properties := map[string]any{
		"after_seconds": map[string]any{"type": "integer", "minimum": 1, "description": "Wake this thread this many seconds from now. There is no ceiling: next week is a valid reminder."},
		"at":            map[string]any{"type": "string", "description": "RFC 3339 timestamp with an offset to wake at instead. A time already past fires on the next sweep."},
		"note":          map[string]any{"type": "string", "minLength": 1, "maxLength": MaxNoteRunes, "description": "What to tell yourself. It arrives as the reminder's answer, so write what you need to pick the work back up."},
	}
	description := "Wake this thread later with a note. Pass exactly one of after_seconds or at. The reminder is a request like any other: it has a token, it is listed and cancelled through thread_status and thread_cancel, and it fires once. Use it instead of sleeping in a loop when you are waiting on something slow, such as a build or a deploy: end your turn and the reminder starts a new one."
	return tool("thread_remind", description, properties, "note")
}

func pairedSuffix(shape Shape, text string) string {
	if shape.Paired() {
		return text
	}
	return ""
}
