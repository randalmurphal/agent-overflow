// Per-tool-kind header dispatcher for ToolCallCard.
//
// The chat spec (§ToolCallCard) classifies tool calls into a small set of
// visual kinds — Bash, Edit, Read, Grep, Web, Image, Task/subagent,
// send_input, wait_agent, Plan, MCP/<tool>, unknown — each with its own
// icon + category label. This module
// pins that classification down so the render path stays a thin table
// lookup rather than a nest of `toolName.startsWith` conditionals scattered
// in the template.
//
// Keep this pure: one input (`toolName`), one output (the visual category
// record). Fallback display text for empty summaries is derived in
// `toolCardPreview.ts` from the raw tool name.

import { isCommandToolName } from "./commandDisplay";
import { isCodexCollabControlToolName } from "./codexCollabControls";

export type ToolKindIcon =
  | "terminal"
  | "file"
  | "eye"
  | "search"
  | "globe"
  | "robot"
  | "speech-bubble"
  | "checklist"
  | "puzzle"
  | "clock"
  | "brain"
  | "compaction"
  | "generic";

/**
 * Tailwind class carrying each kind's hue, from the `--ico-*` token family
 * (see `app.css` `:root` and the light-mode block).
 *
 * Here rather than in `ToolKindIcon.svelte` because the hue is the kind's, not
 * the icon's: a collapsed activity run tints its tool NAMES with it, where no
 * icon renders at all. Two copies would let the chip and the icon disagree
 * about what colour Bash is. Every class is written out statically so the
 * Tailwind v4 compiler sees it.
 */
export const TOOL_KIND_COLOR_CLASS: Record<ToolKindIcon, string> = {
  terminal: "text-ico-terminal",
  file: "text-ico-file",
  eye: "text-ico-eye",
  search: "text-ico-search",
  globe: "text-ico-globe",
  robot: "text-ico-robot",
  "speech-bubble": "text-ico-speech-bubble",
  checklist: "text-ico-checklist",
  puzzle: "text-ico-puzzle",
  clock: "text-ico-clock",
  brain: "text-ico-brain",
  compaction: "text-ico-compaction",
  generic: "text-ico-generic",
};

export interface ToolKindVisual {
  /** Icon kind picked by `iconFor`. Rendered via `ToolKindIcon.svelte`. */
  icon: ToolKindIcon;
  /** Short category label shown in the fixed gutter. */
  label: string;
  /** True when the tool represents a subagent parent (Task, collab_agent). */
  isSubagent: boolean;
}

/**
 * Canonical per-tool-name visual classification. Returns a `ToolKindVisual`
 * describing the header presentation for a tool call row.
 *
 * Matching order is important: `MCP/…` prefixes win over tool name equality,
 * and exact Claude/Codex tool names shadow the generic fallback.
 */
export function classifyToolName(
  toolName: string | undefined | null,
): ToolKindVisual {
  const raw = (toolName ?? "").trim();
  if (!raw) {
    return {
      icon: "generic",
      label: "tool",
      isSubagent: false,
    };
  }

  // MCP tools arrive prefixed as `MCP/<toolName>`. The classifier only
  // chooses the visual bucket; preview fallback displays the suffix.
  if (raw === "MCP") {
    return {
      icon: "puzzle",
      label: "mcp",
      isSubagent: false,
    };
  }

  if (raw.startsWith("MCP/")) {
    return {
      icon: "puzzle",
      label: "mcp",
      isSubagent: false,
    };
  }

  if (isCommandToolName(raw)) {
    return {
      icon: "terminal",
      label: "bash",
      isSubagent: false,
    };
  }

  if (isCodexCollabControlToolName(raw)) {
    switch (raw) {
      case "send_input":
        return {
          icon: "speech-bubble",
          label: "send",
          isSubagent: false,
        };
      case "wait_agent":
        return {
          icon: "robot",
          label: "waiting",
          isSubagent: false,
        };
      case "close_agent":
        return {
          icon: "robot",
          label: "closed",
          isSubagent: false,
        };
      case "resume_agent":
        return {
          icon: "robot",
          label: "resume",
          isSubagent: false,
        };
    }
  }

  switch (raw) {
    case "Edit":
    case "MultiEdit":
    case "Write":
    case "NotebookEdit":
    case "apply_patch":
    case "file_change":
    case "fileChange":
      return {
        icon: "file",
        label: raw === "Write" ? "write" : raw === "NotebookEdit" ? "notebook" : raw === "apply_patch" ? "patch" : "edit",
        isSubagent: false,
      };
    case "Read":
      return {
        icon: "eye",
        label: "read",
        isSubagent: false,
      };
    case "Grep":
    case "Glob":
      return {
        icon: "search",
        label: raw === "Glob" ? "glob" : "grep",
        isSubagent: false,
      };
    case "ToolSearch":
      // Claude Code 2.1.150+ deferred-tool schema loader. The model
      // calls this with `select:<ToolName>` (most common: schema
      // hydration before invoking a deferred tool) or with a free-
      // text keyword query (rarer: capability search). Both cases
      // render with the search icon; the preview line in
      // toolCardPreview.ts disambiguates the two shapes.
      return {
        icon: "search",
        label: "tool",
        isSubagent: false,
      };
    case "TaskList":
    case "TaskGet":
      // Read-only members of the Claude Code 2.1.150+ Task* family
      // (the new TodoWrite replacement). TaskCreate / TaskUpdate are
      // intercepted in the Go parser and never reach the timeline;
      // List/Get stay visible as regular tool rows so users can see
      // the model inspecting its task list.
      return {
        icon: "checklist",
        label: raw === "TaskGet" ? "task" : "tasks",
        isSubagent: false,
      };
    case "ListAgents":
      // Claude Code's cross-session peer directory: the other Claude
      // sessions running on this machine, which the model reads before it
      // can address one. Present only when the peer inbox is on, so the
      // row appearing at all is itself information.
      return {
        icon: "search",
        label: "sessions",
        isSubagent: false,
      };
    case "SendMessage":
      // The other half: a message addressed to one of those sessions.
      // Deliberately NOT `isSubagent` — a peer is a separate, independently
      // driven session, not a child this turn spawned and waits on, and
      // marking it as one would put it on the subagent rail with a
      // lifecycle nothing here owns. A §E6 resume CARRIER never reaches
      // this classifier: toolPresentation routes it to the agent
      // presentation (isClaudeResumeCarrierItem) before header lookup.
      return {
        icon: "speech-bubble",
        label: "message",
        isSubagent: false,
      };
    case "WebFetch":
    case "WebSearch":
    case "webSearch":
    case "web_search":
      return { icon: "globe", label: raw === "WebFetch" ? "fetch" : "search", isSubagent: false };
    case "ViewImage":
    case "ImageGeneration":
      return {
        icon: "eye",
        label: "image",
        isSubagent: false,
      };
    case "Agent":
    case "Task":
      return {
        icon: "robot",
        label: "agent",
        isSubagent: true,
      };
    case "collab_agent":
      return {
        icon: "robot",
        label: "agent",
        isSubagent: true,
      };
    case "Plan":
    case "ExitPlanMode":
      return {
        icon: "checklist",
        label: "plan",
        isSubagent: false,
      };
    case "advisor":
      return {
        icon: "brain",
        label: "advisor",
        isSubagent: false,
      };
    case "Skill":
      return {
        icon: "generic",
        label: "skill",
        isSubagent: false,
      };
  }

  return {
    icon: "generic",
    label: "tool",
    isSubagent: false,
  };
}
