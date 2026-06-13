import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, waitFor } from "@testing-library/svelte";

import EffortMenu from "./EffortMenu.svelte";
import { createThreadPane } from "../../../stores/thread.svelte";
import type { Thread } from "../../../types/models";
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from "../../../../test/mocks/bindings-app";
import {
  ensureProviderModels,
  invalidateProviderModels,
  resetProviderModelsForTest,
} from "../../../stores/providerModels.svelte";

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: "thread-1",
    title: "Test",
    provider: "claude",
    workspacePath: "/tmp",
    projectPath: "/tmp",
    mode: "chat",
    model: "claude-sonnet-4-6",
    reasoningEffort: "high",
    fastMode: false,
    contextWindow: 1000000,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock("SwitchThread", async () => thread);
  setBindingMock("ListRecentThreadItems", async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock("ListRecentTurns", async () => []);
  setBindingMock("ListPayloadMetas", async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

function triggerText(getByTestId: (id: string) => HTMLElement): string {
  return (getByTestId("composer-effort-trigger").textContent ?? "")
    .replace(/\s+/g, " ")
    .trim();
}

describe("<EffortMenu>", () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProviderModelsForTest();
    setBindingMock("GetModelsForProvider", async () => []);
  });

  it("renders effort without context before model metadata is loaded", async () => {
    const pane = await buildPane(makeThread({ provider: "claude" }));
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    expect(triggerText(getByTestId)).toBe("High");
  });

  it("renders context for models with multiple selectable windows", async () => {
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.4",
        name: "GPT-5.4",
        provider: "codex",
        capabilities: [],
        contextWindows: [
          { tokens: 272000, label: "272k", tier: "standard" },
          { tokens: 1000000, label: "1m", tier: "extended" },
        ],
        reasoningEfforts: [],
      },
    ]);
    await ensureProviderModels("codex");
    const pane = await buildPane(
      makeThread({ provider: "codex", model: "gpt-5.4", contextWindow: 272000 }),
    );
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    expect(triggerText(getByTestId)).toBe("High · 272k");
  });

  it("shows the context window on mount without opening a picker (eager catalog load)", async () => {
    // Regression: the context segment (200k / 1M) used to stay hidden until the
    // user opened the effort or model picker, because the catalog only loaded on
    // picker open. EffortMenu now loads it eagerly, so the label is complete on
    // first render. Mirrors the claude-tui Opus 4.8 case from the bug report.
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "claude-opus-4-8",
        name: "Claude Opus 4.8",
        provider: "claude-tui",
        capabilities: [],
        contextWindows: [
          { tokens: 200000, label: "200k", tier: "standard" },
          { tokens: 1000000, label: "1M", tier: "extended" },
        ],
        reasoningEfforts: [],
      },
    ]);
    const pane = await buildPane(
      makeThread({
        provider: "claude-tui",
        model: "claude-opus-4-8",
        reasoningEffort: "xhigh",
        contextWindow: 200000,
      }),
    );
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    // No menu interaction — the eager $effect load resolves and the label fills in.
    await waitFor(() => expect(triggerText(getByTestId)).toBe("xHigh · 200k"));
  });

  it("hides context for models with one selectable window", async () => {
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.3-codex-spark",
        name: "GPT-5.3 Codex Spark",
        provider: "codex",
        capabilities: [],
        contextWindows: [{ tokens: 128000, label: "128k", tier: "standard" }],
        reasoningEfforts: [],
      },
    ]);
    await ensureProviderModels("codex");
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.3-codex-spark",
        contextWindow: 128000,
      }),
    );
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    expect(triggerText(getByTestId)).toBe("High");
  });

  it("renders Fast in the trigger when fast mode is enabled", async () => {
    const pane = await buildPane(makeThread({ fastMode: true }));
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    expect(triggerText(getByTestId)).toBe("High · Fast");
  });

  it("renders xhigh as xHigh in the trigger", async () => {
    const pane = await buildPane(makeThread({ reasoningEffort: "xhigh" }));
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    expect(triggerText(getByTestId)).toBe("xHigh");
  });

  it("opens the menu and calls UpdateThreadReasoningEffort on row click", async () => {
    const pane = await buildPane(makeThread({ reasoningEffort: "medium" }));
    const updated = makeThread({ reasoningEffort: "low" });
    setBindingMock("UpdateThreadReasoningEffort", async () => updated);
    const { getByTestId, findByRole } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    const lowRow = await findByRole("menuitem", { name: /Low/ });
    await fireEvent.click(lowRow);
    await Promise.resolve();
    await Promise.resolve();

    expect(
      getBindingMock("UpdateThreadReasoningEffort")!.mock.calls[0],
    ).toEqual(["thread-1", "low"]);
  });

  it("calls UpdateThreadFastMode when toggling Fast Mode", async () => {
    const pane = await buildPane(
      makeThread({
        model: "claude-opus-4-6",
        fastMode: false,
      }),
    );
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "claude-opus-4-6",
        name: "Claude Opus 4.6",
        provider: "claude",
        capabilities: ["fast_mode"],
        contextWindows: [],
        reasoningEfforts: [
          { slug: "low", label: "Low" },
          { slug: "medium", label: "Medium" },
          { slug: "high", label: "High" },
          { slug: "max", label: "Max" },
        ],
      },
    ]);
    setBindingMock("UpdateThreadFastMode", async () =>
      makeThread({ fastMode: true }),
    );
    const { getByTestId, findAllByRole } = render(EffortMenu, {
      props: { pane },
    });

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    const rows = await findAllByRole("menuitem", { name: /^On$/ });
    await fireEvent.click(rows[0]);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock("UpdateThreadFastMode")!.mock.calls[0]).toEqual([
      "thread-1",
      true,
    ]);
  });

  it("uses model reasoning metadata so Codex does not show Max", async () => {
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.5",
        contextWindow: 272000,
      }),
    );
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.5",
        name: "GPT-5.5",
        provider: "codex",
        capabilities: ["fast_mode"],
        contextWindows: [],
        reasoningEfforts: [
          { slug: "low", label: "Low" },
          { slug: "medium", label: "Medium" },
          { slug: "high", label: "High" },
          { slug: "xhigh", label: "Extra High" },
        ],
      },
    ]);
    const { getByTestId, queryByRole, findByRole } = render(EffortMenu, {
      props: { pane },
    });

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    expect(
      await findByRole("menuitem", { name: /xHigh/ }),
    ).toBeInTheDocument();
    expect(queryByRole("menuitem", { name: /^Max$/ })).toBeNull();
    expect(queryByRole("menuitem", { name: /Greater reasoning depth/ })).toBeNull();
  });

  it("uses preloaded model metadata without fetching when opened", async () => {
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.5",
        name: "GPT-5.5",
        provider: "codex",
        capabilities: ["fast_mode"],
        contextWindows: [
          { tokens: 272000, label: "272k", tier: "standard" },
          { tokens: 1000000, label: "1m", tier: "extended" },
        ],
        reasoningEfforts: [
          { slug: "medium", label: "Medium" },
          { slug: "xhigh", label: "Extra High" },
        ],
      },
    ]);
    await ensureProviderModels("codex");

    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.5",
        reasoningEffort: "medium",
        contextWindow: 272000,
      }),
    );
    getBindingMock("GetModelsForProvider")!.mockClear();

    const { getByTestId, findByRole } = render(EffortMenu, { props: { pane } });
    await fireEvent.click(getByTestId("composer-effort-trigger"));

    expect(await findByRole("menuitem", { name: /xHigh/ })).toBeInTheDocument();
    expect(await findByRole("menuitem", { name: /^1m$/ })).toBeInTheDocument();
    expect(await findByRole("menuitem", { name: /^On$/ })).toBeInTheDocument();
    expect(getBindingMock("GetModelsForProvider")).not.toHaveBeenCalled();
  });

  it("updates open menus when model metadata finishes loading", async () => {
    type PendingModel = {
      slug: string;
      name: string;
      provider: string;
      contextWindows: Array<{ tokens: number; label: string; tier: string }>;
    };
    let resolveModels!: (models: PendingModel[]) => void;
    const pendingModels = new Promise<PendingModel[]>((resolve) => {
      resolveModels = resolve;
    });
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.5",
        contextWindow: 272000,
      }),
    );
    setBindingMock("GetModelsForProvider", async () => pendingModels);
    const { getByTestId, queryByRole, findByRole } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    expect(queryByRole("menuitem", { name: /^1m$/ })).toBeNull();

    resolveModels([
      {
        slug: "gpt-5.5",
        name: "GPT-5.5",
        provider: "codex",
        contextWindows: [
          { tokens: 272000, label: "272k", tier: "standard" },
          { tokens: 1000000, label: "1m", tier: "extended" },
        ],
      },
    ]);

    expect(await findByRole("menuitem", { name: /^1m$/ })).toBeInTheDocument();
  });

  it("renders available context rows and updates the thread context window", async () => {
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.5",
        contextWindow: 272000,
      }),
    );
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.5",
        name: "GPT-5.5",
        provider: "codex",
        capabilities: [],
        contextWindows: [
          { tokens: 272000, label: "272k", tier: "standard" },
          { tokens: 1000000, label: "1m", tier: "extended" },
        ],
      },
    ]);
    setBindingMock("UpdateThreadContextWindow", async () =>
      makeThread({
        provider: "codex",
        model: "gpt-5.5",
        contextWindow: 1000000,
      }),
    );
    const { getByTestId, findByRole } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    const extended = await findByRole("menuitem", { name: /^1m$/ });
    await fireEvent.click(extended);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock("UpdateThreadContextWindow")!.mock.calls[0]).toEqual([
      "thread-1",
      1000000,
    ]);
  });

  it("does not render a context selector for single-window models", async () => {
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.2",
        contextWindow: 272000,
      }),
    );
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.2",
        name: "GPT-5.2",
        provider: "codex",
        capabilities: [],
        contextWindows: [{ tokens: 272000, label: "272k", tier: "standard" }],
        reasoningEfforts: [
          { slug: "low", label: "Low" },
          { slug: "medium", label: "Medium" },
          { slug: "high", label: "High" },
          { slug: "xhigh", label: "Extra High" },
        ],
      },
    ]);
    const { getByTestId, queryByText } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId("composer-effort-trigger"));

    expect(queryByText("Context")).toBeNull();
  });

  it("refreshes context rows after same-provider model catalog invalidation", async () => {
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.5",
        contextWindow: 272000,
      }),
    );
    let contextWindows = [
      { tokens: 272000, label: "272k", tier: "standard" },
    ];
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.5",
        name: "GPT-5.5",
        provider: "codex",
        capabilities: [],
        contextWindows,
      },
    ]);
    const { getByTestId, queryByRole, findByRole } = render(EffortMenu, { props: { pane } });

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    expect(queryByRole("menuitem", { name: /^1m$/ })).toBeNull();

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    contextWindows = [
      { tokens: 272000, label: "272k", tier: "standard" },
      { tokens: 1000000, label: "1m", tier: "extended" },
    ];
    invalidateProviderModels("codex");
    await fireEvent.click(getByTestId("composer-effort-trigger"));

    expect(await findByRole("menuitem", { name: /^1m$/ })).toBeInTheDocument();
  });
});
