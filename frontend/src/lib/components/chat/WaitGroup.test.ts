import { describe, expect, it } from "vitest";
import { render } from "@testing-library/svelte";
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
    expect(getByText(/Waited for agents/)).toBeInTheDocument();
    expect(getByText(/Spawned Galileo -> done/)).toBeInTheDocument();
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
});
