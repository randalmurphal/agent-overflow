import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, waitFor } from "@testing-library/svelte";

import EffortMenu from "./EffortMenu.svelte";
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
import {
  buildPane as buildRegisteredPane,
  makeThread as makeBaseThread,
} from "../../../../test/helpers/chat";

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    workspacePath: "/tmp",
    projectPath: "/tmp",
    reasoningEffort: "high",
    fastMode: false,
    contextWindow: 1000000,
    ...overrides,
  });
}

async function buildPane(thread: Thread) {
  return buildRegisteredPane(thread);
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
        reasoningEfforts: [
          { slug: "medium", label: "Medium" },
          { slug: "high", label: "High", default: true },
        ],
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
        reasoningEfforts: [
          { slug: "high", label: "High" },
          { slug: "xhigh", label: "xHigh", default: true },
        ],
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

  it("stays silent when the eager catalog preload fails (no console.error)", async () => {
    // Regression: the eager preload $effect is a best-effort prefetch and must
    // swallow failures — the actionable failure surfaces on the user-initiated
    // open path. EffortMenu mounts inside Composer, so a console.error here
    // fired on every Composer render and tripped unrelated "no console.error"
    // assertions. The trigger must still render the effort, just without the
    // context segment.
    const pane = await buildPane(makeThread({ provider: "claude" }));
    // Clear any catalog warmed by the pane switch and make the fetch fail, so
    // the eager preload actually reaches its error path.
    resetProviderModelsForTest();
    setBindingMock("GetModelsForProvider", async () => {
      throw new Error("catalog unavailable");
    });
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});

    const { getByTestId } = render(EffortMenu, { props: { pane } });
    // Drive the eager preload to settlement deterministically: the $effect's
    // load is single-flight, so awaiting the same call here resolves only once
    // the rejection has propagated. The extra microtask lets the $effect's own
    // .catch continuation run before we assert.
    await ensureProviderModels("claude").catch(() => {});
    await Promise.resolve();

    // The preload genuinely fetched-and-threw (not a vacuous cache hit) ...
    expect(getBindingMock("GetModelsForProvider")).toHaveBeenCalled();
    // ... yet logged nothing, and the trigger still renders the effort.
    expect(consoleError).not.toHaveBeenCalled();
    expect(triggerText(getByTestId)).toBe("High");
    consoleError.mockRestore();
  });

  it("hides context for models with one selectable window", async () => {
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "gpt-5.3-codex-spark",
        name: "GPT-5.3 Codex Spark",
        provider: "codex",
        capabilities: [],
        contextWindows: [{ tokens: 128000, label: "128k", tier: "standard" }],
        reasoningEfforts: [{ slug: "high", label: "High", default: true }],
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

  // A model whose catalog entry declares NO effort tiers is a statement, not a
  // gap: Claude's own model list reports Haiku that way. The generic fallback
  // list is for the other case — no catalog entry at all — and conflating the
  // two offered tiers the model does not have.
  it("hides the effort section for a model that reports no effort tiers", async () => {
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "claude-haiku-4-5",
        name: "Claude Haiku 4.5",
        provider: "claude",
        capabilities: [],
        contextWindows: [
          { tokens: 200000, label: "200k", tier: "standard", default: true },
          { tokens: 1000000, label: "1M", tier: "extended" },
        ],
        reasoningEfforts: [],
      },
    ]);
    await ensureProviderModels("claude");
    const pane = await buildPane(
      makeThread({
        provider: "claude",
        model: "claude-haiku-4-5",
        contextWindow: 200000,
      }),
    );
    const { getByTestId, queryByRole } = render(EffortMenu, { props: { pane } });

    // The label drops to the one thing left that describes the thread.
    expect(triggerText(getByTestId)).toBe("200k");

    await fireEvent.click(getByTestId("composer-effort-trigger"));
    expect(queryByRole("menuitem", { name: /^High$/ })).toBeNull();
    expect(queryByRole("menuitem", { name: /^Medium$/ })).toBeNull();
    // The context rows are still there — only the effort section went away.
    expect(queryByRole("menuitem", { name: /200k/ })).not.toBeNull();
  });

  it("disables the trigger when the model has nothing to configure", async () => {
    setBindingMock("GetModelsForProvider", async () => [
      {
        slug: "claude-haiku-4-5",
        name: "Claude Haiku 4.5",
        provider: "claude",
        capabilities: [],
        contextWindows: [{ tokens: 200000, label: "200k", tier: "standard", default: true }],
        reasoningEfforts: [],
      },
    ]);
    await ensureProviderModels("claude");
    const pane = await buildPane(
      makeThread({
        provider: "claude",
        model: "claude-haiku-4-5",
        contextWindow: 200000,
      }),
    );
    const { getByTestId } = render(EffortMenu, { props: { pane } });

    const trigger = getByTestId("composer-effort-trigger") as HTMLButtonElement;
    expect(trigger.disabled).toBe(true);
    expect(triggerText(getByTestId)).toBe("200k");
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
    expect(queryByRole("menuitem", { name: /^Max$/ })).toBeNull();
    expect(queryByRole("menuitem", { name: /^Ultra$/ })).toBeNull();

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
