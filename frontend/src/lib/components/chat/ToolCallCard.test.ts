import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, waitFor } from "@testing-library/svelte";
import ToolCallCard from "./ToolCallCard.svelte";
import { buildPane, makeItem, makeThread } from "../../../test/helpers/chat";
import {
  resetBindingMocks,
  setBindingMock,
} from "../../../test/mocks/bindings-app";
import { codexSubagentReceiverLabels } from "../../utils/subagentLaunch";

beforeEach(() => {
  resetBindingMocks();
  // Default payload mocks keep unrelated rows from hanging if a test path
  // expands a body. Tests that assert payload behavior install their own
  // method-specific mock.
  setBindingMock("GetPayloadPreview", async () => ({
    data: "",
    totalSize: 0,
    nextOffset: 0,
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
    expect(getByTestId("command-output-label").textContent?.trim()).toBe(
      "bash",
    );
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
    expect(getByTestId("command-output-label").textContent?.trim()).toBe(
      "bash",
    );
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

    expect(getByTestId("command-output-label").textContent?.trim()).toBe(
      "bash",
    );
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

  it("dispatches Codex request_user_input items to AskUserQuestionCard", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "codex-user-input-1",
      kind: "tool_call",
      status: "completed",
      toolName: "request_user_input",
      meta: JSON.stringify({
        toolName: "request_user_input",
        input: {
          questions: [
            {
              id: "scope",
              header: "Scope",
              question: "Choose a scope",
              options: [
                { label: "turn", description: "" },
                { label: "session", description: "" },
              ],
            },
          ],
        },
        answers: { scope: "turn" },
      }),
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("ask-user-question-card")).toBeInTheDocument();
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
      expect(getByTestId("tool-call-card-label").textContent).toBe(
        toolName === "Write" ? "write" : "edit",
      );
      unmount();
    }
  });

  it("renders pending structured Edit calls as stable diff placeholders with full paths", async () => {
    const pane = await buildPane(
      makeThread({ provider: "claude", workspacePath: "/home/me/repo" }),
    );
    const item = makeItem({
      id: "edit-pending",
      kind: "tool_call",
      status: "running",
      toolName: "Edit",
      summary: "Edit: Composer.svelte",
      meta: JSON.stringify({
        toolName: "Edit",
        input: {
          file_path: "/home/me/repo/frontend/src/lib/components/composer/Composer.svelte",
        },
      }),
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(getByTestId("diff-file-block")).toHaveAttribute(
      "data-file-path",
      "frontend/src/lib/components/composer/Composer.svelte",
    );
    expect(getByTestId("diff-file-path").textContent).toContain(
      "frontend/src/lib/components/composer/Composer.svelte",
    );
    expect(getByTestId("diff-file-status").getAttribute("data-state")).toBe(
      "running",
    );
    expect(queryByTestId("diff-file-body")).toBeNull();
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
    expect(getByTestId("tool-call-card-label").textContent).toBe("read");
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
      expect(getByTestId("tool-call-card-label").textContent).toBe(
        toolName.toLowerCase(),
      );
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

  it("renders robot rows for Agent and collab_agent", async () => {
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
      if (toolName === "Agent") {
        expect(getByTestId("agent-row").getAttribute("data-tool-kind")).toBe("robot");
      } else {
        expect(getByTestId("tool-call-card").getAttribute("data-tool-kind")).toBe("robot");
      }
      unmount();
    }
  });

  it('renders Agent rows with the title-cased subagent label (falls back to "Agent" when subagent_type is missing)', async () => {
    // GenericToolCallRow uses the same `deriveSubagentLabel` helper
    // SubagentGroup does — so a backgrounded Agent row in chat
    // history / the activity tray matches the inline `EXPLORE` /
    // `GENERAL PURPOSE` shape instead of rendering the bare
    // "Subagent" classifier label.
    const pane = await buildPane();
    const item = makeItem({
      id: "agent",
      kind: "tool_call",
      status: "running",
      toolName: "Agent",
    });
    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    expect(getByTestId("agent-row-preview").textContent).toContain("Agent");
    expect(getByTestId("agent-row-preview").textContent).not.toContain("Subagent");
  });

  it('renders non-Codex collab_agent rows with the "agent" classifier label', async () => {
    // Codex collab_agent uses a different input shape — the inline
    // CollabToolRow renders the agent card. The plain
    // GenericToolCallRow fallback path keeps the classifier label so
    // it never goes blank when something else routes a collab_agent
    // row through here.
    const pane = await buildPane();
    const item = makeItem({
      id: "collab",
      kind: "tool_call",
      status: "running",
      toolName: "collab_agent",
    });
    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    expect(getByTestId("tool-call-card-label").textContent).toContain("agent");
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

  it("renders Codex spawn_agent as a compact spawned row", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "spawn",
      kind: "tool_call",
      status: "running",
      toolName: "collab_agent",
      meta: JSON.stringify({
        input: {
          tool: "spawn_agent",
          prompt: "Run `sleep 20` and report the exit code",
          model: "gpt-5.5",
          reasoningEffort: "low",
          receiverThreadIds: ["child-1"],
          newAgentNickname: "Plato",
          newAgentRole: "default",
        },
      }),
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });
    const row = getByTestId("collab-tool-row");

    expect(row.textContent).toContain("Spawned Plato [default]");
    expect(row.textContent).toContain("(GPT 5.5 low)");
    expect(row.textContent).toContain(
      "Run `sleep 20` and report the exit code",
    );
    expect(row.textContent).not.toContain("running");
    expect(queryByTestId("subagent-group")).toBeNull();
  });

  it("renders terminal Codex spawn_agent failures without receivers as failed spawns", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "spawn-failed",
      kind: "tool_call",
      status: "errored",
      toolName: "collab_agent",
      meta: JSON.stringify({
        input: {
          tool: "spawn_agent",
          prompt: "Spawn beyond the agent thread limit",
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("collab-tool-row").textContent).toContain(
      "Agent spawn failed",
    );
  });

  it("keeps receiver identity on terminal Codex spawn_agent failures with receivers", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "spawn-failed-with-receiver",
      kind: "tool_call",
      status: "errored",
      toolName: "collab_agent",
      meta: JSON.stringify({
        input: {
          tool: "spawn_agent",
          prompt: "Start an agent that later fails",
          receiverThreadIds: ["child-1"],
          newAgentNickname: "Curie",
          newAgentRole: "explorer",
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("collab-tool-row").textContent).toContain(
      "Spawned Curie [explorer]",
    );
  });

  it("expands Codex subagent completion payload output", async () => {
    const output = [
      "Run `sleep 20` in /home/rmurphy/repos/agent-overflow",
      "BASH sleep 20",
      "0",
    ].join("\n");
    setBindingMock("GetPayloadPreview", async () => ({
      data: output,
      totalSize: output.length,
      nextOffset: output.length,
      isComplete: true,
    }));
    const thread = makeThread({ provider: "codex" });
    const item = makeItem({
      id: "complete-subagent",
      kind: "tool_completion",
      status: "completed",
      toolName: "collab_agent",
      summary: "collab_agent: Run `sleep 20` -> done",
      payloadId: "payload-subagent",
      payloadKind: "tool_call_result",
    });
    const pane = await buildPane(thread, [item]);

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("collab-tool-row-output")).toBeNull();
    await fireEvent.click(getByTestId("collab-tool-row-toggle"));

    await waitFor(() => {
      expect(getByTestId("collab-tool-row-output").textContent).toContain(
        "BASH sleep 20",
      );
    });
    expect(getByTestId("collab-tool-row-output").textContent).toContain("0");
  });

  it("shows Codex subagent completion output in the collapsed row", async () => {
    const thread = makeThread({ provider: "codex" });
    const launch = makeItem({
      id: "spawn-preview",
      kind: "tool_call",
      status: "completed",
      toolName: "collab_agent",
      meta: JSON.stringify({
        input: {
          tool: "spawn_agent",
          prompt: "Review the changed files",
          receiverThreadIds: ["child-preview"],
          newAgentNickname: "Ada",
          newAgentRole: "default",
        },
      }),
    });
    const completion = makeItem({
      id: "complete-preview",
      kind: "tool_completion",
      status: "completed",
      toolName: "collab_agent",
      summary: "collab_agent: Review the changed files -> done",
      completionOf: "spawn-preview",
      payloadId: "payload-preview",
      payloadKind: "tool_call_result",
      payloadMeta: JSON.stringify({
        itemStatus: "completed",
        preview: "Blocking | frontend/src/App.svelte:10 | issue found",
      }),
    });
    const pane = await buildPane(thread, [launch, completion]);

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item: completion },
    });

    expect(queryByTestId("collab-tool-row-output")).toBeNull();
    expect(getByTestId("collab-tool-row-preview").textContent).toContain(
      "Blocking | frontend/src/App.svelte:10 | issue found",
    );

    await fireEvent.click(getByTestId("collab-tool-row-toggle"));
    await waitFor(() => {
      expect(queryByTestId("collab-tool-row-preview")).toBeNull();
    });
  });

  it("renders no preview from wait-carrier agentsStates when the completion has no payload preview", async () => {
    // Persisted agentsStates entries are status-only (the Go persist
    // shaping + v9 store migration delete `message`), so the old
    // wait-carrier preview fallback was removed. A carrier row that
    // still carries a message (this fixture mimics one) must NOT leak
    // into the collapsed preview — payload meta is the only source.
    const thread = makeThread({ provider: "codex" });
    const launch = makeItem({
      id: "spawn-history",
      kind: "tool_call",
      status: "completed",
      toolName: "collab_agent",
      meta: JSON.stringify({
        input: {
          tool: "spawn_agent",
          receiverThreadIds: ["child-history"],
          newAgentNickname: "Grace",
          newAgentRole: "default",
        },
      }),
    });
    const wait = makeItem({
      id: "wait-history",
      kind: "tool_call",
      status: "completed",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          tool: "wait_agent",
          receiverThreadIds: ["child-history"],
          agentsStates: {
            "child-history": {
              status: "completed",
              message: "this text must not surface as a preview",
            },
          },
        },
      }),
    });
    const completion = makeItem({
      id: "complete-history",
      kind: "tool_completion",
      status: "completed",
      toolName: "collab_agent",
      completionOf: "spawn-history",
      meta: JSON.stringify({ wait_carrier_id: "wait-history" }),
    });
    const pane = await buildPane(thread, [launch, wait, completion]);

    const { queryByTestId } = render(ToolCallCard, {
      props: { pane, item: completion },
    });

    expect(queryByTestId("collab-tool-row-preview")).toBeNull();
  });

  it("labels Codex subagent completion rows from the spawned agent metadata", async () => {
    const thread = makeThread({ provider: "codex" });
    const launch = makeItem({
      id: "spawn-archimedes",
      kind: "tool_call",
      status: "running",
      toolName: "collab_agent",
      meta: JSON.stringify({
        input: {
          tool: "spawn_agent",
          prompt: "Run `sleep 3` in the shell and report when it completes",
          receiverThreadIds: ["child-archimedes"],
          newAgentNickname: "Archimedes",
          newAgentRole: "default",
        },
      }),
    });
    const completion = makeItem({
      id: "complete-archimedes",
      kind: "tool_completion",
      status: "completed",
      toolName: "collab_agent",
      summary: "collab_agent: Run `sleep 3` -> done",
      completionOf: "spawn-archimedes",
      payloadId: "payload-archimedes",
      payloadKind: "tool_call_result",
    });
    const pane = await buildPane(thread, [launch, completion]);

    const { getByTestId } = render(ToolCallCard, {
      props: { pane, item: completion },
    });
    const text = getByTestId("collab-tool-row").textContent ?? "";

    expect(text).toContain("Archimedes [default]");
    expect(text).not.toContain("collab_agent");
  });

  it("loads the full Codex subagent completion payload on request", async () => {
    setBindingMock("GetPayloadPreview", async () => ({
      data: "first chunk\n",
      totalSize: 24,
      nextOffset: 12,
      isComplete: false,
    }));
    setBindingMock("GetPayloadChunk", async () => ({
      data: "second chunk\n",
      offset: 12,
      nextOffset: 25,
      totalSize: 25,
      isComplete: true,
    }));
    const thread = makeThread({ provider: "codex" });
    const item = makeItem({
      id: "complete-subagent-more",
      kind: "tool_completion",
      status: "completed",
      toolName: "collab_agent",
      summary: "collab_agent: Run `sleep 20` -> done",
      payloadId: "payload-subagent-more",
      payloadKind: "tool_call_result",
    });
    const pane = await buildPane(thread, [item]);

    const { getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    await fireEvent.click(getByTestId("collab-tool-row-toggle"));
    await waitFor(() => {
      expect(getByTestId("collab-tool-row-output").textContent).toContain(
        "first chunk",
      );
    });

    await fireEvent.click(getByTestId("collab-tool-row-show-full"));
    await waitFor(() => {
      expect(getByTestId("collab-tool-row-output").textContent).toContain(
        "second chunk",
      );
    });
  });

  it("retries a failed Codex subagent completion payload preview", async () => {
    let attempts = 0;
    setBindingMock("GetPayloadPreview", async () => {
      attempts += 1;
      if (attempts === 1) {
        throw new Error("temporary read failure");
      }
      return {
        data: "loaded after retry",
        totalSize: 18,
        nextOffset: 18,
        isComplete: true,
      };
    });
    const thread = makeThread({ provider: "codex" });
    const item = makeItem({
      id: "complete-subagent-retry",
      kind: "tool_completion",
      status: "completed",
      toolName: "collab_agent",
      summary: "collab_agent: Run `sleep 20` -> done",
      payloadId: "payload-subagent-retry",
      payloadKind: "tool_call_result",
    });
    const pane = await buildPane(thread, [item]);

    const { getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    await fireEvent.click(getByTestId("collab-tool-row-toggle"));
    await waitFor(() => {
      expect(getByTestId("collab-tool-row-output").textContent).toContain(
        "temporary read failure",
      );
    });

    await fireEvent.click(getByTestId("collab-tool-row-retry"));
    await waitFor(() => {
      expect(getByTestId("collab-tool-row-output").textContent).toContain(
        "loaded after retry",
      );
    });
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
        expected: "Waiting for Agent",
      },
      {
        status: "completed" as const,
        meta: JSON.stringify({ input: { receiverThreadIds: ["child-1"] } }),
        expected: "Waiting for Agent",
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
      expect(getByTestId("collab-tool-row").textContent).not.toContain(
        "running",
      );
      unmount();
    }
  });

  it("renders wait_agent completion rows with the waited agent list only", async () => {
    // The receivers line under "Finished waiting" shows just the agent
    // labels. agentsStates carries each agent's status AND full final
    // message on the wire; surfacing either here would duplicate the
    // spawn completion rows that render beneath the wait group (and the
    // message is unbounded — a review agent's entire findings text).
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "complete-wait-1",
      kind: "tool_completion",
      status: "completed",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          receiverThreadIds: ["child-1", "child-2"],
          receiverAgents: [
            { threadId: "child-1", agentNickname: "Hypatia", agentRole: "default" },
            { threadId: "child-2", agentNickname: "Parfit", agentRole: "default" },
          ],
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
    expect(text).not.toContain("Waited for 2 agents");
    expect(getByTestId("collab-tool-row-receivers").textContent).toBe(
      "Hypatia [default], Parfit [default]",
    );
    expect(text).not.toContain("completed - done");
    expect(text).not.toContain("failed - boom");
    expect(text).not.toContain("child-1");
    expect(text).not.toContain("child-2");
  });

  it("uses linked spawn rows instead of receiver UUIDs for wait_agent labels", async () => {
    const thread = makeThread({ provider: "codex" });
    const spawnItems = [
      makeItem({
        id: "spawn-1",
        kind: "tool_call",
        status: "completed",
        toolName: "collab_agent",
        meta: JSON.stringify({
          input: {
            tool: "spawn_agent",
            prompt: "Run `sleep 3` in the shell",
            receiverThreadIds: ["019df120-4fbb-7390-a468-b47b88221e25"],
            newAgentNickname: "Newton",
            newAgentRole: "default",
          },
        }),
      }),
      makeItem({
        id: "spawn-2",
        kind: "tool_call",
        status: "completed",
        toolName: "collab_agent",
        meta: JSON.stringify({
          input: {
            tool: "spawn_agent",
            prompt: "Run `sleep 5` in the shell",
            receiverThreadIds: ["019df120-5039-73d2-9496-9e862004aa6b"],
            newAgentNickname: "Epicurus",
            newAgentRole: "default",
          },
        }),
      }),
      makeItem({
        id: "spawn-3",
        kind: "tool_call",
        status: "completed",
        toolName: "collab_agent",
        meta: JSON.stringify({
          input: {
            tool: "spawn_agent",
            prompt: "Run `sleep 7` in the shell",
            receiverThreadIds: ["019df120-5088-7860-9961-a5706ec9f5f4"],
            newAgentNickname: "Heisenberg",
            newAgentRole: "default",
          },
        }),
      }),
    ];
    const pane = await buildPane(thread, spawnItems);
    const item = makeItem({
      id: "wait-uuids",
      kind: "tool_call",
      status: "running",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          tool: "wait_agent",
          receiverThreadIds: [
            "019df120-4fbb-7390-a468-b47b88221e25",
            "019df120-5039-73d2-9496-9e862004aa6b",
            "019df120-5088-7860-9961-a5706ec9f5f4",
          ],
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, {
      props: {
        pane,
        item,
        codexSubagentReceiverLabels: codexSubagentReceiverLabels([
          ...spawnItems,
          item,
        ]),
      },
    });
    const text = getByTestId("collab-tool-row").textContent ?? "";

    expect(text).toContain("Waiting for 3 agents");
    expect(text).toContain("Newton [default]");
    expect(text).toContain("Epicurus [default]");
    expect(text).toContain("Heisenberg [default]");
    expect(text).not.toContain("019df120");
  });

  it("does not expose raw receiver ids from unlabeled receiverAgents records", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "mixed-wait-receivers",
      kind: "tool_call",
      status: "completed",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          tool: "wait_agent",
          receiverThreadIds: ["019df120-known", "019df120-unknown-uuid"],
          receiverAgents: [
            {
              threadId: "019df120-known",
              agentNickname: "Hypatia",
              agentRole: "default",
            },
            { threadId: "019df120-unknown-uuid" },
          ],
          agentsStates: {
            "019df120-known": { status: "completed", message: "done" },
            "019df120-unknown-uuid": { status: "completed", message: "done" },
          },
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    const text = getByTestId("collab-tool-row").textContent ?? "";

    expect(text).toContain("Hypatia [default]");
    expect(text).toContain("Agent");
    expect(text).not.toContain("019df120-unknown-uuid");
  });

  it("keeps the full waited-agent list when only one target completed", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "partial-wait",
      kind: "tool_call",
      status: "completed",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          tool: "wait_agent",
          receiverThreadIds: ["child-1"],
          requestedReceiverThreadIds: ["child-1", "child-2", "child-3"],
          receiverAgents: [
            {
              threadId: "child-1",
              agentNickname: "Hypatia",
              agentRole: "default",
            },
          ],
          requestedReceiverAgents: [
            {
              threadId: "child-1",
              agentNickname: "Hypatia",
              agentRole: "default",
            },
            {
              threadId: "child-2",
              agentNickname: "Parfit",
              agentRole: "default",
            },
            { threadId: "child-3", agentNickname: "Ada", agentRole: "default" },
          ],
          agentsStates: {
            "child-1": { status: "completed", message: "done" },
          },
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    const text = getByTestId("collab-tool-row").textContent ?? "";

    expect(text).toContain("Waiting for 3 agents");
    expect(text).toContain("Hypatia [default]");
    expect(text).toContain("Parfit [default]");
    expect(text).toContain("Ada [default]");
    expect(text).not.toContain("Waiting for Hypatia");
  });

  it("renders wait_agent receivers as a single comma-separated line under the parent body", async () => {
    // The receiver list sits below the "Waiting for N agents" header and
    // is meant to read as a continuation of the parent row's body column,
    // not as a separate left-edge list. The body column starts at
    // 6.125rem from the CollabToolRow's `px-1` content edge (chevron +
    // gap + icon + gap + label + gap, all defined in
    // TranscriptDisclosureHeader). Joining the agents with ", " keeps
    // long rosters readable on a single wrapping line rather than one
    // truncated row per agent. The `└` leader is gone — body-column
    // alignment already carries the visual relationship and the user
    // explicitly asked that it not appear. If the disclosure
    // primitive's gutter widths change, recompute the margin and update
    // both this expectation and the comment in CollabToolRowDetails.
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "wait-receivers-indent",
      kind: "tool_call",
      status: "running",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          tool: "wait_agent",
          receiverThreadIds: ["child-1", "child-2"],
          receiverAgents: [
            { threadId: "child-1", agentNickname: "Schrodinger", agentRole: "default" },
            { threadId: "child-2", agentNickname: "Kierkegaard", agentRole: "default" },
          ],
        },
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });
    const receivers = getByTestId("collab-tool-row-receivers");

    expect(receivers.className).toContain("ml-[6.125rem]");
    expect(receivers.className).not.toContain("space-y-");
    expect(receivers.textContent).toBe("Schrodinger [default], Kierkegaard [default]");
    expect(receivers.textContent).not.toContain("└");
  });

  it("renders wait_agent receiver nicknames and keeps completed carriers neutral", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "wait-agent-nickname",
      kind: "tool_call",
      status: "completed",
      toolName: "wait_agent",
      meta: JSON.stringify({
        input: {
          tool: "wait_agent",
          receiverThreadIds: ["child-1"],
          receiverAgents: [
            {
              threadId: "child-1",
              agentNickname: "Galileo",
              agentRole: "explorer",
            },
          ],
        },
      }),
    });

    const { getByTestId, queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });
    const text = getByTestId("collab-tool-row").textContent ?? "";

    expect(text).toContain("Waiting for Galileo [explorer]");
    expect(text).not.toContain("└ Galileo [explorer]");
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
    expect(getByTestId("tool-call-card-label").textContent).toBe("mcp");
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
    expect(getByTestId("tool-call-card-label").textContent).toBe("mcp");
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
    expect(getByTestId("tool-call-card-label").textContent).toBe("tool");
    expect(getByTestId("tool-call-card-preview").textContent).toContain(
      "doing something",
    );
  });

  it("delegates to ProposedPlanCard when payloadKind=proposed_plan", async () => {
    const planMarkdown = "# plan with a long first heading that needs action clearance\n\n## Summary\n\nKeep every heading.";
    setBindingMock("GetPayloadData", async () => ({ data: planMarkdown }));
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
        preview: planMarkdown,
      }),
    });
    const pane = await buildPane(makeThread(), [item]);

    const { getByLabelText, getByTestId, queryByTestId, queryByText } = render(ToolCallCard, {
      props: { pane, item },
    });

    // The generic fallback card must NOT render when a structured payload
    // renderer takes over.
    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(getByTestId("proposed-plan-card")).toBeInTheDocument();
    expect(getByTestId("proposed-plan-body-shell").textContent).toContain("long first heading");
    expect(getByTestId("proposed-plan-body-shell").textContent).toContain("Summary");
    expect(getByTestId("proposed-plan-body-shell").textContent).toContain("Keep every heading.");
    expect(getByTestId("proposed-plan-body-shell").className).not.toContain("pr-24");
    expect(getByTestId("proposed-plan-body-shell").className).not.toContain("ml-[5.25rem]");
    expect(getByTestId("proposed-plan-body-shell").className).not.toContain("px-3");
    expect(getByTestId("proposed-plan-actions")).toBeInTheDocument();
    expect(getByLabelText("Copy full plan")).toBeInTheDocument();
    expect(getByLabelText("Save plan")).toBeInTheDocument();
    expect(getByLabelText("Open in plan sidebar")).toBeInTheDocument();
    expect(queryByTestId("proposed-plan-header")).toBeNull();
    expect(queryByTestId("proposed-plan-label")).toBeNull();
    expect(queryByText("Deploy thing")).toBeNull();
  });

  it("does not append accepted state to proposed plan chat history", async () => {
    const planMarkdown = "# implementation plan\n\nKeep the transcript stable.";
    setBindingMock("GetPayloadData", async () => ({ data: planMarkdown }));
    const item = makeItem({
      id: "plan-accepted",
      kind: "tool_call",
      status: "completed",
      toolName: "ExitPlanMode",
      payloadId: "payload-plan",
      payloadKind: "proposed_plan",
      payloadMeta: JSON.stringify({
        title: "Implementation plan",
        lineCount: 3,
        charCount: 57,
        preview: planMarkdown,
      }),
      meta: JSON.stringify({ planImplementedAt: 123 }),
    });
    const pane = await buildPane(makeThread(), [item]);

    const { getByTestId, queryByTestId, queryByText } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("proposed-plan-card")).toBeInTheDocument();
    expect(getByTestId("proposed-plan-body-shell").textContent).toContain("Keep the transcript stable.");
    expect(queryByTestId("proposed-plan-accepted")).toBeNull();
    expect(queryByText("Accepted")).toBeNull();
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

  it("uses CommandOutput for exec_command rows without payloads", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "exec-cmd",
      kind: "tool_call",
      status: "running",
      toolName: "exec_command",
      summary: "exec_command: pnpm test",
    });

    const { queryByTestId, getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(getByTestId("command-output-command").textContent).toBe("pnpm test");
  });

  it("shows the error indicator and sub-line for command payloads with snake-case exit status", async () => {
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
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("command-output-status").getAttribute("data-state")).toBe("error");
    expect(getByTestId("command-output-error").textContent).toContain("exit 137");
  });

  it("shows the error indicator and sub-line for command payloads with is_error", async () => {
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
        errorMessage: "No such file",
      }),
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId("command-output-status").getAttribute("data-state")).toBe("error");
    expect(getByTestId("command-output-error").textContent).toContain("No such file");
  });

  it("delegates to DiffPreview when payloadKind=diff", async () => {
    const pane = await buildPane();
    const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
    const item = makeItem({
      id: "diff",
      kind: "tool_call",
      status: "completed",
      toolName: "Edit",
      payloadId: "payload-diff",
      payloadKind: "diff",
      createdAt,
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
    // Pins the single-file-diff branch's own createdAt wire — the
    // diff-stack test exercises DiffFileStack's, not this one.
    expect(
      container
        .querySelector('[data-testid="diff-file-time"]')
        ?.getAttribute("datetime"),
    ).toBe(new Date(createdAt).toISOString());
  });

  it("delegates to ToolResultCard when payloadKind=tool_result", async () => {
    const pane = await buildPane();
    const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
    const item = makeItem({
      id: "tr",
      kind: "tool_completion",
      status: "completed",
      toolName: "Edit",
      payloadId: "payload-tr",
      payloadKind: "tool_result",
      createdAt,
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
    // The tool_result fallthrough row carries the clock like every
    // other tool row.
    expect(
      container
        .querySelector('[data-testid="tool-result-time"]')
        ?.getAttribute("datetime"),
    ).toBe(new Date(createdAt).toISOString());
  });

  it("delegates exact-patch tool_result rows to a DiffFileBlock per file", async () => {
    const pane = await buildPane();
    const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
    const item = makeItem({
      id: "tool-result-pending-payload",
      kind: "tool_completion",
      status: "completed",
      toolName: "Edit",
      payloadKind: "tool_result",
      createdAt,
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
    // The item's clock time threads through to the diff row header.
    expect(
      blocks[0]
        .querySelector('[data-testid="diff-file-time"]')
        ?.getAttribute("datetime"),
    ).toBe(new Date(createdAt).toISOString());
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

describe("<ToolCallCard> backgrounded status indicator", () => {
  it("shows the backgrounded indicator when isBackground && status === running", async () => {
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
    expect(status.getAttribute("data-state")).toBe("backgrounded");
    const indicator = status.querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute("aria-label")).toBe("Backgrounded");
    expect(queryByTestId("tool-call-backgrounded-badge")).toBeNull();
  });

  it("shows the backgrounded indicator for running Codex subagent launch rows", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const item = makeItem({
      id: "spawn-bg",
      kind: "tool_call",
      status: "running",
      toolName: "collab_agent",
      summary: "spawn_agent: worker",
      isBackground: true,
    });

    const { queryByTestId, getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("tool-call-card")).toBeNull();
    expect(getByTestId("collab-tool-row-label").textContent).toContain("agent");
    expect(getByTestId("collab-tool-row").textContent).toContain(
      "Spawned agent",
    );
    expect(getByTestId("indicator").getAttribute("data-state")).toBe(
      "backgrounded",
    );
  });

  it("renders the running indicator when !isBackground && status === running", async () => {
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
    expect(getByTestId("command-output-status").getAttribute("data-state")).toBe("running");
    expect(getByTestId("command-output-label").textContent?.trim()).toBe(
      "bash",
    );
  });

  it("shows no status indicator when a backgrounded launch row is completed", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "bg-done",
      kind: "tool_call",
      status: "completed",
      toolName: "Bash",
      summary: "sleep 60 &",
      isBackground: true,
    });

    const { queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("command-output-status")).toBeNull();
  });

  it("renders the Bash label for tool_completion kind even when the flags match", async () => {
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
    expect(getByTestId("command-output-status").getAttribute("data-state")).toBe("running");
    expect(getByTestId("command-output-label").textContent?.trim()).toBe(
      "bash",
    );
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
  it("shows the Bash label for streaming/running command tool calls", async () => {
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
    expect(getByTestId("command-output-label").textContent?.trim()).toBe(
      "bash",
    );
  });

  it("renders no success indicator for an inline tool_call that completed", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_call",
      status: "completed",
      isBackground: false,
      toolName: "Bash",
      summary: "ls",
    });

    const { queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("command-output-status")).toBeNull();
    expect(queryByTestId("indicator")).toBeNull();
  });

  it("renders the error indicator and sub-line for errored tool calls", async () => {
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

    expect(getByTestId("command-output-status").getAttribute("data-state")).toBe("error");
    expect(queryByTestId("row-error-code")).toBeNull();
    expect(getByTestId("command-output-error").textContent).toContain("command failed");
  });

  it("renders no success indicator for completed tool calls", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_completion",
      status: "completed",
      toolName: "Bash",
      summary: "ok",
    });

    const { queryByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(queryByTestId("command-output-status")).toBeNull();
    expect(queryByTestId("indicator")).toBeNull();
  });

  it("collapses killed into the error indicator and sub-line", async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: "t",
      kind: "tool_completion",
      status: "killed",
      toolName: "Bash",
      summary: "stopped",
    });

    const { getByTestId } = render(ToolCallCard, {
      props: { pane, item },
    });

    expect(getByTestId("command-output-status").getAttribute("data-state")).toBe("error");
    expect(getByTestId("command-output-error").textContent).toContain("Tool call stopped");
  });
});
