import { createRawSnippet } from 'svelte';
import { describe, expect, it } from "vitest";
import { fireEvent, render } from "@testing-library/svelte";
import { tick } from "svelte";
import WaitGroup from "./WaitGroup.svelte";
import { buildPane, makeItem, makeThread } from "../../../test/helpers/chat";
import type { WaitGroupNode } from "../../utils/subagentGrouping";

// The fixtures here observe plain completions (leaves); the node renderer
// only fires for a detached spawn's card, which MessageTimeline supplies.
const noopRenderNode = createRawSnippet(() => ({ render: () => '<span></span>' }));

describe("<WaitGroup>", () => {
  it("renders the folded completion as a finished header above the nested target completion rows", async () => {
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
      // The folded standalone wait_agent completion (b) is rendered AS the header
      // in place of the carrier, so the group reads "Finished waiting" + the
      // waited agent list instead of the carrier tool_call's "Waiting for N
      // agents". Labels only — each agent's status and final message belong to
      // its spawn completion row on the child rail, not the header line.
      completion: makeItem({
        id: "complete-wait-1",
        kind: "tool_completion",
        toolName: "wait_agent",
        completionOf: "wait-1",
        status: "completed",
        meta: JSON.stringify({
          input: {
            tool: "wait_agent",
            requestedReceiverThreadIds: ["child-1"],
            requestedReceiverAgents: [
              { threadId: "child-1", agentNickname: "Galileo", agentRole: "explorer" },
            ],
            agentsStates: { "child-1": { status: "completed", message: "done" } },
          },
        }),
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

    const { getByTestId, getByText, queryByText } = render(WaitGroup, {
      props: { pane, group, renderNode: noopRenderNode },
    });

    expect(getByTestId("wait-group")).toBeInTheDocument();
    expect(getByTestId("wait-group-children").className).toContain("max-h-[20rem]");
    expect(getByTestId("wait-group-children").className).toContain("overflow-y-auto");
    expect(getByTestId("wait-group-children").className).not.toContain("border-l");
    // Header flipped to the finished state (folded completion), not "Waiting".
    expect(getByText(/Finished waiting/)).toBeInTheDocument();
    expect(queryByText(/Waiting for agents/)).not.toBeInTheDocument();
    // Receivers line carries labels only — no status, no final message.
    expect(getByTestId("collab-tool-row-receivers")).toHaveTextContent(
      "└ Galileo [explorer]",
    );
    expect(queryByText(/completed - done/)).not.toBeInTheDocument();
    // Child rail still renders the nested target completion.
    expect(getByText(/Spawned Galileo -> done/)).toBeInTheDocument();
  });

  it("falls back to the carrier's waiting header before the completion loads", async () => {
    // Pre-completion / page-boundary frame: no folded completion, so the header
    // renders the carrier tool_call ("Waiting for N agents") via the
    // `group.completion ?? group.parent` fallback.
    const pane = await buildPane(makeThread({ provider: "codex" }));
    const group: WaitGroupNode = {
      kind: "wait_group",
      groupKey: "wait:wait-1",
      parent: makeItem({
        id: "wait-1",
        kind: "tool_call",
        toolName: "wait_agent",
        status: "running",
        summary: "wait_agent",
        meta: JSON.stringify({ input: { tool: "wait_agent" } }),
      }),
      children: [],
      descendantCount: 0,
    };

    const { getByText, queryByText } = render(WaitGroup, { props: { pane, group, renderNode: noopRenderNode } });
    expect(getByText(/Waiting for agents/)).toBeInTheDocument();
    expect(queryByText(/Finished waiting/)).not.toBeInTheDocument();
  });

  it("indents the child rail to line up with the parent row's body column", async () => {
    // The wait_agent's body column ("Waiting for N agents") starts at
    // 6.375rem from the row's outer edge (px-1 + chevron + gap +
    // icon + gap + label + gap, all defined in
    // TranscriptDisclosureHeader). With no border / inner padding on
    // the child rail the margin owns the entire offset; if the
    // disclosure primitive grows or shrinks any of those gutter
    // elements, recompute the margin and update both this
    // expectation and the comment in WaitGroup.svelte. Without this,
    // the run-of-children renders hugging the row's left edge
    // instead of indented under the body text.
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

    const { getByTestId } = render(WaitGroup, { props: { pane, group, renderNode: noopRenderNode } });
    expect(getByTestId("wait-group-children").className).toContain("ml-[6.375rem]");
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
      props: { pane, group, renderNode: noopRenderNode },
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

    const view = render(WaitGroup, { props: { pane, group, renderNode: noopRenderNode } });

    expect(view.getByText(/Spawned Agent 24 -> done/)).toBeInTheDocument();
    expect(view.queryByText(/Spawned Agent 25 -> done/)).not.toBeInTheDocument();

    const showAll = view.getByTestId("wait-group-show-all");
    expect(showAll.textContent).toContain("Show 5 more");
    await fireEvent.click(showAll);

    expect(view.getByText(/Spawned Agent 29 -> done/)).toBeInTheDocument();

    // The answer lives in the pane registry under the `wait:` key, not in
    // the row: a windowing remount keeps the full list, and the activity-run
    // auto-collapse gate reads it as engagement with the carrier item.
    expect(pane.isSubagentGroupExpanded("wait:wait-many")).toBe(true);
    expect(pane.hasUserExpansionWithin(["wait-many"])).toBe(true);
    view.unmount();
    const remounted = render(WaitGroup, { props: { pane, group, renderNode: noopRenderNode } });
    expect(remounted.getByText(/Spawned Agent 29 -> done/)).toBeInTheDocument();
  });

  it("re-resolves header and child rows against the pane when a wait completes", async () => {
    // The node is a structural snapshot — the timeline no longer patches
    // fresh item refs into it, so this group has to reach the store
    // itself. It does, through `TimelineLeaf`, which resolves by id for
    // both the header (`group.completion ?? group.parent`) and every
    // child rail row. Asserted on a child row, whose text is the visible
    // proof; the header's own fallback is covered above.
    const pane = await buildPane(makeThread({ id: "thread-1", provider: "codex" }), [
      makeItem({
        id: "wait-1", itemIndex: 0, kind: "tool_call", toolName: "wait_agent",
        status: "running", summary: "wait_agent",
        meta: JSON.stringify({ input: { tool: "wait_agent" } }),
      }),
      makeItem({
        id: "complete-spawn-1", itemIndex: 1, kind: "tool_completion",
        toolName: "collab_agent", completionOf: "spawn-1", status: "running",
        summary: "Spawned Galileo -> running",
      }),
    ]);
    const group: WaitGroupNode = {
      kind: "wait_group",
      groupKey: "wait:wait-1",
      parent: pane.items[0],
      children: [{ kind: "leaf", item: pane.items[1] }],
      descendantCount: 1,
    };

    const view = render(WaitGroup, { props: { pane, group, renderNode: noopRenderNode } });
    expect(view.getByText(/Waiting for agents/)).toBeInTheDocument();
    expect(view.getByText(/Spawned Galileo -> running/)).toBeInTheDocument();

    // Content-only writes: no structural rebuild, so the node still
    // describes the pre-completion world.
    pane.applyItemPatch({
      threadId: "thread-1", itemId: "wait-1", kind: "tool_call",
      patch: { status: "completed", updatedAt: 10 },
    });
    pane.applyItemPatch({
      threadId: "thread-1", itemId: "complete-spawn-1", kind: "tool_completion",
      patch: { summary: "Spawned Galileo -> done", updatedAt: 11 },
    });
    await tick();

    expect(group.parent.status, "node must stay stale for this to prove anything").toBe("running");
    expect(view.getByText(/Spawned Galileo -> done/)).toBeInTheDocument();
    expect(view.queryByText(/Spawned Galileo -> running/)).not.toBeInTheDocument();
  });
});
