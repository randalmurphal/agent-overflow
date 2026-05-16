// Per-tool-kind header dispatcher for ToolCallCard.
//
// The chat spec (§ToolCallCard) classifies tool calls into a small set of
// visual kinds — Bash, Edit, Read, Grep, Web, Image, Task/subagent,
// send_input, wait_agent, Plan, MCP/<tool>, unknown — each with its own
// icon + label. This module
// pins that classification down so the render path stays a thin table
// lookup rather than a nest of `toolName.startsWith` conditionals scattered
// in the template.
//
// Keep this pure: one input (`toolName`), one output (the `ToolKindVisual`
// record). Everything that renders the header reads from the returned
// record, never peeks back at the raw tool name.

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
  | "generic";

export interface ToolKindVisual {
  /** Icon kind picked by `iconFor`. Rendered via `ToolKindIcon.svelte`. */
  icon: ToolKindIcon;
  /** Short human label shown before the tool's input preview. */
  label: string;
  /** The raw tool name as it should appear to the user when no richer label fits. */
  displayName: string;
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
      displayName: "Tool",
      isSubagent: false,
    };
  }

  // MCP tools arrive prefixed as `MCP/<toolName>` — preserve the suffix so
  // the user still sees which MCP call ran.
  if (raw === "MCP") {
    return {
      icon: "puzzle",
      label: "mcp",
      displayName: "MCP tool",
      isSubagent: false,
    };
  }

  if (raw.startsWith("MCP/")) {
    const suffix = raw.slice(4);
    return {
      icon: "puzzle",
      label: "mcp",
      displayName: suffix || "MCP tool",
      isSubagent: false,
    };
  }

  if (isCommandToolName(raw)) {
    return {
      icon: "terminal",
      label: "bash",
      displayName: "Bash",
      isSubagent: false,
    };
  }

  if (isCodexCollabControlToolName(raw)) {
    switch (raw) {
      case "send_input":
        return {
          icon: "speech-bubble",
          label: "send",
          displayName: "send_input",
          isSubagent: false,
        };
      case "wait_agent":
        return {
          icon: "robot",
          label: "waiting",
          displayName: "Wait for agent",
          isSubagent: false,
        };
      case "close_agent":
        return {
          icon: "robot",
          label: "closed",
          displayName: "Close agent",
          isSubagent: false,
        };
      case "resume_agent":
        return {
          icon: "robot",
          label: "resume",
          displayName: "Resume agent",
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
        displayName: raw,
        isSubagent: false,
      };
    case "Read":
      return {
        icon: "eye",
        label: "read",
        displayName: "Read",
        isSubagent: false,
      };
    case "Grep":
    case "Glob":
      return {
        icon: "search",
        label: raw === "Glob" ? "glob" : "grep",
        displayName: raw,
        isSubagent: false,
      };
    case "WebFetch":
    case "WebSearch":
    case "webSearch":
    case "web_search":
      return { icon: "globe", label: raw === "WebFetch" ? "fetch" : "search", displayName: raw, isSubagent: false };
    case "ViewImage":
    case "ImageGeneration":
      return {
        icon: "eye",
        label: "image",
        displayName: raw,
        isSubagent: false,
      };
    case "Agent":
    case "Task":
      return {
        icon: "robot",
        label: "agent",
        displayName: raw,
        isSubagent: true,
      };
    case "collab_agent":
      return {
        icon: "robot",
        label: "spawn",
        displayName: "collab_agent",
        isSubagent: true,
      };
    case "Plan":
    case "ExitPlanMode":
      return {
        icon: "checklist",
        label: "plan",
        displayName: raw,
        isSubagent: false,
      };
  }

  return {
    icon: "generic",
    label: "tool",
    displayName: raw,
    isSubagent: false,
  };
}
