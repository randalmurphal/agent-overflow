import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render } from "@testing-library/svelte";

import EffortMenu from "./EffortMenu.svelte";
import { createThreadPane } from "../../../stores/thread.svelte";
import type { Thread } from "../../../types/models";
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from "../../../../test/mocks/bindings-app";

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
  setBindingMock("GetModelsForProvider", async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe("<EffortMenu>", () => {
  beforeEach(() => resetBindingMocks());

  it("renders effort and context on Claude threads", async () => {
    const pane = await buildPane(makeThread({ provider: "claude" }));
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    const label = getByTestId("composer-effort-trigger").textContent ?? "";
    expect(label).toMatch(/High/);
    expect(label).toMatch(/1M/);
  });

  it("renders effort and context on Codex threads", async () => {
    const pane = await buildPane(
      makeThread({ provider: "codex", contextWindow: 272000 }),
    );
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    const label = getByTestId("composer-effort-trigger").textContent ?? "";
    expect(label).toMatch(/High/);
    expect(label).toMatch(/272k/);
  });

  it("renders Spark's smaller context label", async () => {
    const pane = await buildPane(
      makeThread({
        provider: "codex",
        model: "gpt-5.3-codex-spark",
        contextWindow: 128000,
      }),
    );
    const { getByTestId } = render(EffortMenu, { props: { pane } });
    const label = getByTestId("composer-effort-trigger").textContent ?? "";
    expect(label).toMatch(/128k/);
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
      await findByRole("menuitem", { name: /Extra High/ }),
    ).toBeInTheDocument();
    expect(queryByRole("menuitem", { name: /^Max$/ })).toBeNull();
    expect(queryByRole("menuitem", { name: /Greater reasoning depth/ })).toBeNull();
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
});
