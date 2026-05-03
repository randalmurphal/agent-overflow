import { beforeEach, describe, expect, it } from "vitest";
import { render } from "@testing-library/svelte";
import ToolCallCard from "./ToolCallCard.svelte";
import { buildPane, makeItem, makeThread } from "../../../test/helpers/chat";
import {
  resetBindingMocks,
  setBindingMock,
} from "../../../test/mocks/bindings-app";

beforeEach(() => {
  resetBindingMocks();
  // Payload fetches are triggered when the user expands the body; the tests
  // below never click the toggle, so the mocks just need to exist for the
  // chevron path to not blow up if something races.
  setBindingMock("GetPayloadPreview", async () => ({
    data: "",
    totalSize: 0,
    isComplete: true,
  }));
  setBindingMock("GetPayloadData", async () => ({ data: "" }));
  setBindingMock("GetPayloadChunk", async () => ({
    data: "",
    offset: 0,
    nextOffset: 0,
    totalSize: 0,
    isComplete: true,
  }));
});

describe("<ToolCallCard> header dispatcher", () => {
  it("renders the shared command row for a Claude Bash tool call", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "tool-1",
      kind: "tool_call",
      status: "running",
      toolName: "Bash",
      summary: "ls -la",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("command-output-row")).toBeInTheDocument();
    expect(getByTestId("command-output-icon")).toBeInTheDocument();
    expect(getByTestId("command-output-label").textContent?.trim()).toBe("Bash");
    expect(getByTestId("command-output-command").textContent).toContain(
      "ls -la",
    );
  });

  it("renders Codex command_execution without output as the shared command row", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "codex-empty-output",
      kind: "tool_call",
      status: "completed",
      toolName: "command_execution",
      summary: "Bash: /usr/bin/zsh -lc 'git status --short'",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(getByTestId("command-output-row")).toBeInTheDocument();
    expect(getByTestId("command-output-icon")).toBeInTheDocument();
    expect(getByTestId("command-output-label").textContent?.trim()).toBe("Bash");
    expect(getByTestId("command-output-command").textContent).toBe(
      "git status --short",
    );
  });

  it("renders no-payload command completions without outcome suffixes", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "command-completion-summary",
      kind: "tool_completion",
      status: "completed",
      toolName: "Bash",
      summary: "Bash: sleep 10 -> done",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("command-output-label").textContent?.trim()).toBe("Bash");
    expect(getByTestId("command-output-command").textContent).toBe("sleep 10");
  });

  it("dispatches AskUserQuestion items to AskUserQuestionCard", async () => {
    // Branch must run BEFORE the generic payloadKind dispatch — the
    // tool_result content for AskUserQuestion is a JSON-stringified
    // answers blob, not a structured payload, so a wrong branch order
    // would route to a renderer that doesn't know how to read it.
    const pane = await buildPane();
    const item = makeItem({
      id: "ask-1",
      kind: "tool_call",
      status: "running",
      toolName: "AskUserQuestion",
      meta: JSON.stringify({
        toolName: "AskUserQuestion",
        input: {
          questions: [
            {
              id: "q1",
              header: "Q",
              question: "Pick one?",
              options: [
                { label: "A", description: "" },
                { label: "B", description: "" },
              ],
            },
          ],
        },
      }),
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("ask-user-question-card")).toBeInTheDocument();
    // Generic tool-call card chrome should not appear.
    expect(queryByTestId("tool-call-card")).toBeNull();
  });

  it("renders a file icon for Edit/Write/MultiEdit tools", async () => {
    const pane = await buildPane();
    for (const toolName of ["Edit", "Write", "MultiEdit"]) {
      const item = makeItem({
        id: `tool-${toolName}`,
        kind: "tool_call",
        status: "running",
        toolName,
        summary: "foo.ts",
      });

      const { getByTestId, unmount } = render(ToolCallCard, {
        props: { pane, item },
      });

      expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
        "file",
      );
      expect(getByTestId("tool-call-card-label").textContent).toBe(toolName);
      unmount();
    }
  });

  it("renders an eye icon for Read", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "r",
      kind: "tool_call",
      status: "running",
      toolName: "Read",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
      "eye",
    );
    expect(getByTestId("tool-call-card-label").textContent).toBe("Read");
  });

  it("renders a search icon for Grep and Glob", async () => {
    const pane = await buildPane();
    for (const toolName of ["Grep", "Glob"]) {
      const item = makeItem({
        id: toolName,
        kind: "tool_call",
        status: "running",
        toolName,
      });
      const { getByTestId, unmount } = render(ToolCallCard, {
        props: { pane, item },
      });
      expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
        "search",
      );
      expect(getByTestId("tool-call-card-label").textContent).toBe(toolName);
      unmount();
    }
  });

  it("renders a globe icon for WebFetch and WebSearch", async () => {
    const pane = await buildPane();
    for (const toolName of ["WebFetch", "WebSearch", "webSearch"]) {
      const item = makeItem({
        id: toolName,
        kind: "tool_call",
        status: "running",
        toolName,
      });
      const { getByTestId, unmount } = render(ToolCallCard, {
        props: { pane, item },
      });
      expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
        "globe",
      );
      unmount();
    }
  });

  it("surfaces Codex webSearch metadata and the row timestamp", async () => {
    const pane = await buildPane();
    const createdAt = Date.UTC(2026, 0, 2, 15, 4);
    const item = makeItem({
      id: "web-1",
      kind: "tool_call",
      status: "completed",
      toolName: "WebSearch",
      summary: "WebSearch: codex app-server webSearch",
      createdAt,
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
      "globe",
    );
    expect(getByTestId("tool-call-card-preview").textContent).toContain(
      "codex app-server webSearch",
    );
    expect(getByTestId("tool-call-card-time").getAttribute("datetime")).toBe(
      new Date(createdAt).toISOString(),
    );
    expect(getByTestId("tool-call-card-toggle")).toHaveClass("cursor-default");
    expect(queryByTestId("tool-call-card-body")).toBeNull();
  });

  it("omits the dropdown control when a generic tool has no payload or deferred output", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "empty-result",
      kind: "tool_call",
      status: "completed",
      toolName: "WebSearch",
      summary: "WebSearch: current Wails release",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("tool-call-card-toggle")).toHaveClass("cursor-default");
    expect(queryByTestId("tool-call-card-body")).toBeNull();
  });

  it('renders a robot icon + "Subagent" label for Agent and collab_agent', async () => {
    const pane = await buildPane();
    for (const toolName of ["Agent", "collab_agent"]) {
      const item = makeItem({
        id: toolName,
        kind: "tool_call",
        status: "running",
        toolName,
      });
      const { getByTestId, unmount } = render(ToolCallCard, {
        props: { pane, item },
      });
      expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
        "robot",
      );
      expect(getByTestId("tool-call-card-label").textContent).toBe("Subagent");
      unmount();
    }
  });

  it("renders a speech-bubble icon for send_input", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "s",
      kind: "tool_call",
      status: "running",
      toolName: "send_input",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("collab-tool-row").textContent).toContain("Sent input");
  });

  it("keeps Codex control rendering scoped to Codex threads", async () => {
    const pane = await buildPane(makeThread({ provider: "claude" }));
    const item = makeItem({
      id: "s",
      kind: "tool_call",
      status: "running",
      toolName: "send_input",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("collab-tool-row")).toBeNull();
    expect(getByTestId("tool-call-card")).toBeInTheDocument();
  });

  it("renders wait_agent as a human wait row with receiver count", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const cases = [
      {
        status: "running" as const,
        meta: JSON.stringify({
          input: { receiverThreadIds: ["child-1", "child-2"] },
        }),
        expected: "Waiting for 2 agents",
      },
      {
        status: "running" as const,
        meta: JSON.stringify({ input: { receiverThreadIds: ["child-1"] } }),
        expected: "Waiting for child-1",
      },
      {
        status: "completed" as const,
        meta: JSON.stringify({ input: { receiverThreadIds: ["child-1"] } }),
        expected: "Waiting for child-1",
      },
      {
        status: "completed" as const,
        meta: "",
        expected: "Waiting for agents",
      },
      {
        status: "running" as const,
        meta: "{",
        expected: "Waiting for agents",
      },
      {
        status: "running" as const,
        meta: JSON.stringify({ input: { receiverThreadIds: "child-1" } }),
        expected: "Waiting for agents",
      },
    ];

    for (const testCase of cases) {
      const item = makeItem({
        id: `wait-agent-${testCase.expected}`,
        kind: "tool_call",
        status: testCase.status,
        toolName: "wait_agent",
        summary: "raw summary should not win",
        meta: testCase.meta,
      });

      const { getByTestId, unmount } = render(ToolCallCard, {
        props: { pane, item },
      });

      expect(getByTestId("collab-tool-row").textContent).toContain(
        testCase.expected,
      );
      unmount();
    }
  });

  it("renders wait_agent completion rows with per-agent statuses", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "complete-wait-1",
      kind: "tool_completion",
      status: "completed",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          receiverThreadIds: ["child-1", "child-2"],
          agentsStates: {
            "child-1": { status: "completed", message: "done" },
            "child-2": { status: "failed", message: "boom" },
          },
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    const text = getByTestId("collab-tool-row").textContent ?? "";

    expect(text).toContain("Finished waiting");
    expect(text).toContain("child-1: completed - done");
    expect(text).toContain("child-2: failed - boom");
  });

  it("renders a checklist icon for Plan / ExitPlanMode", async () => {
    const pane = await buildPane();
    for (const toolName of ["Plan", "ExitPlanMode"]) {
      const item = makeItem({
        id: toolName,
        kind: "tool_call",
        status: "running",
        toolName,
      });
      const { getByTestId, unmount } = render(ToolCallCard, {
        props: { pane, item },
      });
      expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
        "checklist",
      );
      unmount();
    }
  });

  it("renders a puzzle icon for MCP/<tool> and preserves the suffix as preview", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "mcp-1",
      kind: "tool_call",
      status: "running",
      toolName: "MCP/browser_snapshot",
      summary: "",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
      "puzzle",
    );
    expect(getByTestId("tool-call-card-label").textContent).toBe("MCP");
    expect(getByTestId("tool-call-card-preview").textContent).toContain(
      "browser_snapshot",
    );
  });

  it("surfaces Codex mcpToolCall metadata", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "mcp-2",
      kind: "tool_call",
      status: "completed",
      toolName: "MCP/lookup",
      summary: "MCP/lookup: docs/lookup",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
      "puzzle",
    );
    expect(getByTestId("tool-call-card-label").textContent).toBe("MCP");
    expect(getByTestId("tool-call-card-preview").textContent).toContain(
      "docs/lookup",
    );
  });

  it('falls back to the generic icon + "Tool" label for an unknown tool', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "unk",
      kind: "tool_call",
      status: "running",
      toolName: "CompletelyNovelTool",
      summary: "doing something",
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe(
      "generic",
    );
    expect(getByTestId("tool-call-card-label").textContent).toBe("Tool");
    expect(getByTestId("tool-call-card-preview").textContent).toContain(
      "doing something",
    );
  });

  it("delegates to ProposedPlanCard when payloadKind=proposed_plan", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "plan",
      kind: "tool_call",
      status: "completed",
      toolName: "ExitPlanMode",
      payloadId: "payload-plan",
      payloadKind: "proposed_plan",
      payloadMeta: JSON.stringify({
        title: "Deploy thing",
        lineCount: 3,
        charCount: 120,
        preview: "# plan",
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, {
      props: { pane, item },
    });

    // The generic fallback card must NOT render when a structured payload
    // renderer takes over.
    expect(queryByTestId("tool-call-card")).toBeNull();
    // ProposedPlanCard puts the title in a heading; sanity-check a fragment.
    expect(container.textContent).toContain("Deploy thing");
  });

  it("does not show the plan-sidebar action on an older proposed plan", async () => {
    const olderPlan = makeItem({
      id: "plan-old",
      kind: "tool_call",
      status: "completed",
      toolName: "ExitPlanMode",
      payloadId: "payload-old",
      payloadKind: "proposed_plan",
      payloadMeta: JSON.stringify({
        title: "Older plan",
        lineCount: 1,
        charCount: 10,
        preview: "# older",
      }),
      turnIndex: 0,
      itemIndex: 0,
      updatedAt: 1,
    });
    const currentPlan = makeItem({
      id: "plan-current",
      kind: "tool_call",
      status: "completed",
      toolName: "ExitPlanMode",
      payloadId: "payload-current",
      payloadKind: "proposed_plan",
      payloadMeta: JSON.stringify({
        title: "Current plan",
        lineCount: 1,
        charCount: 12,
        preview: "# current",
      }),
      turnIndex: 1,
      itemIndex: 0,
      updatedAt: 2,
    });
    const pane = await buildPane(makeThread(), [olderPlan, currentPlan]);

    const { queryByLabelText } = render(ToolCallCard, {
      props: { pane, item: olderPlan },
    });

    expect(queryByLabelText("Open in plan sidebar")).toBeNull();
  });

  it("delegates to CommandOutput when payloadKind=command_output", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "cmd",
      kind: "tool_call",
      status: "completed",
      toolName: "Bash",
      payloadId: "payload-cmd",
      payloadKind: "command_output",
      payloadMeta: JSON.stringify({
        command: "ls",
        exitCode: 0,
        lineCount: 1,
        preview: "file.txt",
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    // CommandOutput's button surfaces the command text.
    expect(container.textContent).toContain("ls");
  });

  it("keeps failure badges for command payloads with snake-case exit status", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "cmd-exit-code",
      kind: "tool_call",
      status: "completed",
      toolName: "command_execution",
      payloadId: "payload-cmd-exit-code",
      payloadKind: "command_output",
      payloadMeta: JSON.stringify({
        command: "sleep 10",
        exit_code: 137,
        lineCount: 0,
        preview: "",
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("completion-badge").getAttribute("data-status")).toBe(
      "failure",
    );
  });

  it("keeps failure badges for command payloads with is_error", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "cmd-is-error",
      kind: "tool_call",
      status: "completed",
      toolName: "Bash",
      payloadId: "payload-cmd-is-error",
      payloadKind: "command_output",
      payloadMeta: JSON.stringify({
        command: "cat missing.txt",
        is_error: true,
        lineCount: 1,
        preview: "No such file",
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("completion-badge").getAttribute("data-status")).toBe(
      "failure",
    );
  });

  it("delegates to DiffPreview when payloadKind=diff", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "diff",
      kind: "tool_call",
      status: "completed",
      toolName: "Edit",
      payloadId: "payload-diff",
      payloadKind: "diff",
      payloadMeta: JSON.stringify({
        filePath: "foo.ts",
        changeKind: "modified",
        insertions: 1,
        deletions: 1,
        preview: "@@ -1 +1 @@",
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(container.textContent).toContain("foo.ts");
  });

  it("delegates to ToolResultCard when payloadKind=tool_result", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "tr",
      kind: "tool_completion",
      status: "completed",
      toolName: "Edit",
      payloadId: "payload-tr",
      payloadKind: "tool_result",
      payloadMeta: JSON.stringify({
        itemType: "file_change",
        title: "Edit applied",
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(container.textContent).toContain("Edit applied");
  });

  it("delegates exact-patch tool_result rows to a DiffFileBlock per file", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "tool-result-pending-payload",
      kind: "tool_completion",
      status: "completed",
      toolName: "Edit",
      payloadKind: "tool_result",
      payloadMeta: JSON.stringify({
        itemType: "file_change",
        title: "Edit applied",
        inlineDiff: {
          availability: "exact_patch",
          insertions: 2,
          deletions: 1,
          files: [
            {
              path: "src/file.ts",
              insertions: 2,
              deletions: 1,
              kind: "modified",
            },
          ],
        },
      }),
    });

    const { getAllByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    // No outer card wrapper / generic tool-call shell — diff rows
    // render as standalone DiffFileBlocks per file.
    expect(queryByTestId("tool-call-card")).toBeNull();
    const blocks = getAllByTestId("diff-file-block");
    expect(blocks).toHaveLength(1);
    expect(blocks[0]).toHaveAttribute("data-file-path", "src/file.ts");
  });

  it("keeps rich tool_result rendering for command-named tool rows", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "command-tool-result",
      kind: "tool_completion",
      status: "completed",
      toolName: "command_execution",
      payloadId: "payload-command-tool-result",
      payloadKind: "tool_result",
      payloadMeta: JSON.stringify({
        itemType: "file_change",
        title: "Command edited files",
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("command-output-row")).toBeNull();
    expect(queryByTestId("command-output-toggle")).toBeNull();
    expect(container.textContent).toContain("Command edited files");
  });

  it("uses the shared command row for command tools even when payloadKind is unknown", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "unpay",
      kind: "tool_call",
      status: "running",
      toolName: "Bash",
      // payloadKind set to something we don't special-case, with no structured meta.
      payloadKind: "tool_call_result",
      summary: "echo hi",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(getByTestId("command-output-command").textContent).toContain(
      "echo hi",
    );
  });
});

describe("<ToolCallCard> backgrounded status label", () => {
  // Per-spec (invariant 24, docs/architecture/turn-lifecycle.md §UI
  // components driven by this state), the status chip replaces "running"
  // with "…" for a backgrounded launch. The "…" is the visual signal
  // that the agent dispatched the tool and moved on — it is NOT a
  // claim that the tool is currently executing. The existing
  // status-chip slot renders it; no separate badge is added to the row.

  it('shows "…" in the status chip when isBackground && status === "running"', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "bg-run",
      kind: "tool_call",
      status: "running",
      toolName: "Bash",
      summary: "sleep 60 &",
      isBackground: true,
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    const status = getByTestId("command-output-status");
    expect(status.textContent?.trim()).toBe("…");
    expect(status.getAttribute("aria-label")).toBe("Backgrounded");
    expect(status.getAttribute("title")).toBe("Running in background");
    // No separate badge element — the label itself carries the signal.
    expect(queryByTestId("tool-call-backgrounded-badge")).toBeNull();
  });

  it('does not show "…" for Codex subagent launch history rows', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "spawn-bg",
      kind: "tool_call",
      status: "running",
      toolName: "collab_agent",
      summary: "spawn_agent: worker",
      isBackground: true,
    });

    const { queryByTestId, getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    expect(getByTestId("tool-call-card-duration").textContent?.trim()).toBe("");
  });

  it('renders the Bash command label when !isBackground && status === "running"', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "inline-run",
      kind: "tool_call",
      status: "running",
      toolName: "Bash",
      summary: "ls -la",
      isBackground: false,
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    expect(getByTestId("command-output-status").textContent?.trim()).toBe("…");
    expect(getByTestId("command-output-status").className).toContain("animate-pulse");
    expect(getByTestId("command-output-label").textContent?.trim()).toBe("Bash");
  });

  it('keeps "…" with no badge when a backgrounded launch row is somehow status=completed', async () => {
    // Per the transcript stability invariant
    // (docs/architecture/turn-lifecycle.md §1, plus the user-facing
    // requirement that persisted launch rows do not mutate visually),
    // a backgrounded tool_call launch row keeps its `…` affordance for
    // the lifetime of the transcript even if its status flips. The
    // completion lands on a separate tool_completion sibling row, and
    // that's where the unified CompletionBadge renders.
    const pane = await buildPane();
    const item = makeItem({
      id: "bg-done",
      kind: "tool_call",
      status: "completed",
      toolName: "Bash",
      summary: "sleep 60 &",
      isBackground: true,
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    const status = getByTestId("command-output-status");
    expect(status.textContent?.trim()).toBe("…");
    expect(status.getAttribute("aria-label")).toBe("Backgrounded");
    expect(queryByTestId("completion-badge")).toBeNull();
  });

  it('renders the Bash label for tool_completion kind even when the flags match', async () => {
    // tool_completion rows are terminal by definition — isBackground+running
    // should never fire on them in practice, but the template guards
    // against this corner. The sibling completion row must not claim
    // "backgrounded still-running" when its completion has already landed.
    const pane = await buildPane();
    const item = makeItem({
      id: "bg-completion",
      kind: "tool_completion",
      status: "running",
      toolName: "Bash",
      summary: "background job",
      isBackground: true,
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    expect(getByTestId("command-output-status").textContent?.trim()).toBe("…");
    expect(getByTestId("command-output-label").textContent?.trim()).toBe("Bash");
  });

  it("shows a loading body when background output ingestion is still in flight", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "bg-loading",
      kind: "tool_completion",
      status: "completed",
      toolName: "Bash",
      summary: "sleep 60 &",
      isBackground: true,
      meta: JSON.stringify({
        task_id: "task-1",
        notification_output_state: "loading",
      }),
    });

    const { getByTestId, getByText } = render(ToolCallCard, {
      props: { pane, item },
    });
    await getByTestId("command-output-toggle").click();

    expect(getByText("Loading…")).toBeInTheDocument();
  });
});

describe("<ToolCallCard> status dispatch", () => {
  it('shows the Bash label for streaming/running command tool calls', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_call",
      status: "running",
      toolName: "Bash",
      summary: "sleep 10",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    expect(getByTestId("command-output-label").textContent?.trim()).toBe("Bash");
  });

  it("renders the success badge for an inline tool_call that completed (non-background)", async () => {
    // Helper coverage in toolCompletionStatus.test.ts pins the
    // derivation; this pins the wiring in GenericToolCallRow's
    // template — that the badge renders in the same slot the old
    // status text occupied when the row resolves to success.
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_call",
      status: "completed",
      isBackground: false,
      toolName: "Bash",
      summary: "ls",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    const badge = getByTestId("completion-badge");
    expect(badge.getAttribute("data-status")).toBe("success");
  });

  it("renders the failure badge for errored tool calls", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_completion",
      status: "errored",
      toolName: "Bash",
      summary: "oops",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    const badge = getByTestId("completion-badge");
    expect(badge.getAttribute("data-status")).toBe("failure");
    expect(badge.className).toContain("text-error");
  });

  it("renders the success badge for completed tool calls", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_completion",
      status: "completed",
      toolName: "Bash",
      summary: "ok",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    const badge = getByTestId("completion-badge");
    expect(badge.getAttribute("data-status")).toBe("success");
    expect(badge.className).toContain("text-success");
  });

  it("collapses killed into the failure badge (user stop is a non-success terminal)", async () => {
    // Pre-unification the row had a third "stopped" muted style. The
    // unified badge collapses errored/killed/declined into a single
    // failure variant per the design choice in the planning step —
    // simpler vocabulary, fewer ad-hoc colors. The decision chip /
    // tray still differentiate the cause (stopped vs failed).
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_completion",
      status: "killed",
      toolName: "Bash",
      summary: "stopped",
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card-status")).toBeNull();
    const badge = getByTestId("completion-badge");
    expect(badge.getAttribute("data-status")).toBe("failure");
    expect(badge.className).toContain("text-error");
    expect(badge.className).not.toContain("text-success");
  });
});
