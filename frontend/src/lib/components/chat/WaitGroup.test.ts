import { describe, expect, it } from "vitest";
import { fireEvent, render } from "@testing-library/svelte";
import WaitGroup from "./WaitGroup.svelte";
import { buildPane, makeItem, makeThread } from "../../../test/helpers/chat";
import type { WaitGroupNode } from "../../utils/subagentGrouping";

describe("<WaitGroup>", () => {
  it("renders the wait carrier and nested target completion rows", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const group: WaitGroupNode = {
      kind: "wait_group",
      groupKey: "wait:wait-1",
      parent: makeItem({
        id: "wait-1",
        kind: "tool_call",
        toolName: "wait_agent",
        status: "completed",
        summary: "wait_agent",
        meta: JSON.stringify({ input: { tool: "wait_agent" } }),
      }),
      children: [
        {
          kind: "leaf",
          item: makeItem({
            id: "complete-spawn-1",
            kind: "tool_completion",
            toolName: "collab_agent",
            completionOf: "spawn-1",
            summary: "Spawned Galileo -> done",
          }),
        },
      ],
      descendantCount: 1,
    };

    const { getByTestId, getByText } = render(WaitGroup, {
      props: { pane, group },
    });

    expect(getByTestId("wait-group")).toBeInTheDocument();
    expect(getByTestId("wait-group-children").className).toContain("max-h-[20rem]");
    expect(getByTestId("wait-group-children").className).toContain("overflow-y-auto");
    expect(getByText(/Waiting for agents/)).toBeInTheDocument();
    expect(getByText(/Spawned Galileo -> done/)).toBeInTheDocument();
  });

  it("indents the child rail to line up with the parent row's body column", async () => {
    // The wait_agent's body column ("Waiting for N agents") starts at
    // 6.375rem from the row's outer edge (px-1 + chevron + gap +
    // icon + gap + label + gap, all defined in
    // TranscriptDisclosureHeader). The child rail's ml + border + pl
    // must add up to the same offset; if the disclosure primitive
    // grows or shrinks any of those gutter elements, recompute the
    // margin and update both this expectation and the comment in
    // WaitGroup.svelte. Without this, the run-of-children renders
    // hugging the row's left edge instead of indented under the body
    // text.
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const group: WaitGroupNode = {
      kind: "wait_group",
      groupKey: "wait:wait-1",
      parent: makeItem({
        id: "wait-1",
        kind: "tool_call",
        toolName: "wait_agent",
        status: "completed",
        summary: "wait_agent",
        meta: JSON.stringify({ input: { tool: "wait_agent" } }),
      }),
      children: [
        {
          kind: "leaf",
          item: makeItem({
            id: "complete-spawn-1",
            kind: "tool_completion",
            toolName: "collab_agent",
            completionOf: "spawn-1",
            summary: "Spawned Galileo -> done",
          }),
        },
      ],
      descendantCount: 1,
    };

    const { getByTestId } = render(WaitGroup, { props: { pane, group } });
    expect(getByTestId("wait-group-children").className).toContain("ml-[5.5625rem]");
  });

  it("does not render an empty child rail for timeout waits", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const group: WaitGroupNode = {
      kind: "wait_group",
      groupKey: "wait:wait-1",
      parent: makeItem({
        id: "wait-1",
        kind: "tool_call",
        toolName: "wait_agent",
        status: "completed",
        summary: "wait_agent",
        meta: JSON.stringify({ input: { tool: "wait_agent" } }),
      }),
      children: [],
      descendantCount: 0,
    };

    const { getByTestId, queryByTestId } = render(WaitGroup, {
      props: { pane, group },
    });

    expect(getByTestId("wait-group")).toBeInTheDocument();
    expect(queryByTestId("wait-group-children")).not.toBeInTheDocument();
  });

  it("does not mount every child row until the user asks for the full wait group", async () => {
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const children = Array.from({ length: 30 }, (_, index) => ({
      kind: "leaf" as const,
      item: makeItem({
        id: `complete-spawn-${index}`,
        kind: "tool_completion",
        toolName: "collab_agent",
        completionOf: `spawn-${index}`,
        summary: `Spawned Agent ${index} -> done`,
      }),
    }));
    const group: WaitGroupNode = {
      kind: "wait_group",
      groupKey: "wait:wait-many",
      parent: makeItem({
        id: "wait-many",
        kind: "tool_call",
        toolName: "wait_agent",
        status: "completed",
        summary: "wait_agent",
        meta: JSON.stringify({ input: { tool: "wait_agent" } }),
      }),
      children,
      descendantCount: children.length,
    };

    const { getByTestId, queryByText, getByText } = render(WaitGroup, {
      props: { pane, group },
    });

    expect(getByText(/Spawned Agent 24 -> done/)).toBeInTheDocument();
    expect(queryByText(/Spawned Agent 25 -> done/)).not.toBeInTheDocument();

    const showAll = getByTestId("wait-group-show-all");
    expect(showAll.textContent).toContain("Show 5 more");
    await fireEvent.click(showAll);

    expect(getByText(/Spawned Agent 29 -> done/)).toBeInTheDocument();
  });
});
