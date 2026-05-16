// Tool-name classification table for the ToolCallCard header. Pure
// function; the tests walk every branch including MCP/ prefix priority
// over the switch table and the null/empty fallback.

import { describe, expect, it } from "vitest";
import { classifyToolName } from "./toolCardHeader";

describe("classifyToolName", () => {
  it("returns the generic fallback for null", () => {
    expect(classifyToolName(null)).toEqual({
      icon: "generic",
      label: "tool",
      isSubagent: false,
    });
  });

  it("returns the generic fallback for undefined", () => {
    expect(classifyToolName(undefined)).toEqual({
      icon: "generic",
      label: "tool",
      isSubagent: false,
    });
  });

  it("returns the generic fallback for empty string", () => {
    expect(classifyToolName("")).toEqual({
      icon: "generic",
      label: "tool",
      isSubagent: false,
    });
  });

  it("trims whitespace-only tool names to the generic fallback", () => {
    expect(classifyToolName("   ")).toEqual({
      icon: "generic",
      label: "tool",
      isSubagent: false,
    });
  });

  it("MCP/ prefix wins over the switch table", () => {
    // Even a suffix that matches an entry in the switch (e.g. "Bash"
    // as an MCP tool name) must still be classified as MCP.
    const out = classifyToolName("MCP/Bash");
    expect(out.icon).toBe("puzzle");
    expect(out.label).toBe("mcp");
    expect(out.isSubagent).toBe(false);
  });

  it('MCP/ with empty suffix stays in the MCP bucket', () => {
    const out = classifyToolName("MCP/");
    expect(out.icon).toBe("puzzle");
    expect(out.label).toBe("mcp");
    expect(out.isSubagent).toBe(false);
  });

  it("Bash maps to terminal icon", () => {
    expect(classifyToolName("Bash").icon).toBe("terminal");
  });

  it.each([
    ["Edit", "edit"],
    ["Write", "write"],
    ["MultiEdit", "edit"],
    ["file_change", "edit"],
    ["fileChange", "edit"],
    ["apply_patch", "patch"],
  ])(
    "%s maps to file icon with a category label",
    (name, label) => {
      const out = classifyToolName(name);
      expect(out.icon).toBe("file");
      expect(out.label).toBe(label);
      expect(out.isSubagent).toBe(false);
    },
  );

  it("Read maps to the eye icon", () => {
    const out = classifyToolName("Read");
    expect(out.icon).toBe("eye");
    expect(out.label).toBe("read");
  });

  it.each(["Grep", "Glob"])("%s maps to search icon", (name) => {
    expect(classifyToolName(name).icon).toBe("search");
  });

  it.each(["WebFetch", "WebSearch", "webSearch", "web_search"])(
    "%s maps to globe icon",
    (name) => {
      expect(classifyToolName(name).icon).toBe("globe");
    },
  );

  it.each(["ViewImage", "ImageGeneration"])(
    "%s maps to the image bucket",
    (name) => {
      const out = classifyToolName(name);
      expect(out.icon).toBe("eye");
      expect(out.label).toBe("image");
      expect(out.isSubagent).toBe(false);
    },
  );

  it("bare MCP maps to the MCP tool bucket", () => {
    const out = classifyToolName("MCP");
    expect(out.icon).toBe("puzzle");
    expect(out.label).toBe("mcp");
  });

  it("Agent classifies as subagent and uses robot icon", () => {
    const out = classifyToolName("Agent");
    expect(out.icon).toBe("robot");
    expect(out.label).toBe("agent");
    expect(out.isSubagent).toBe(true);
  });

  it("Task uses the agent presentation bucket", () => {
    const out = classifyToolName("Task");
    expect(out.icon).toBe("robot");
    expect(out.label).toBe("agent");
    expect(out.isSubagent).toBe(true);
  });

  it("collab_agent classifies as subagent", () => {
    const out = classifyToolName("collab_agent");
    expect(out.icon).toBe("robot");
    expect(out.isSubagent).toBe(true);
  });

  it("send_input maps to speech-bubble icon", () => {
    const out = classifyToolName("send_input");
    expect(out.icon).toBe("speech-bubble");
    expect(out.isSubagent).toBe(false);
  });

  it("wait_agent maps to robot icon without becoming a subagent parent", () => {
    const out = classifyToolName("wait_agent");
    expect(out.icon).toBe("robot");
    expect(out.label).toBe("waiting");
    expect(out.isSubagent).toBe(false);
  });

  it.each([
    ["close_agent", "closed"],
    ["resume_agent", "resume"],
  ])(
    "%s maps to a non-parent collab control row",
    (name, label) => {
      const out = classifyToolName(name);
      expect(out.icon).toBe("robot");
      expect(out.label).toBe(label);
      expect(out.isSubagent).toBe(false);
    },
  );

  it.each(["Plan", "ExitPlanMode"])("%s maps to checklist icon", (name) => {
    expect(classifyToolName(name).icon).toBe("checklist");
  });

  it("unknown tool names fall through to the generic category", () => {
    const out = classifyToolName("SomeCustomTool");
    expect(out.icon).toBe("generic");
    expect(out.label).toBe("tool");
    expect(out.isSubagent).toBe(false);
  });

  it('case-sensitive matching — "bash" (lowercase) falls through to generic', () => {
    // The switch table uses exact provider tool names; a lowercase
    // variant is treated as custom/unknown.
    expect(classifyToolName("bash").icon).toBe("generic");
  });

  it("trims leading/trailing whitespace before classification", () => {
    // Whitespace is trimmed at entry so "  Bash  " routes to the Bash
    // branch rather than falling to generic.
    expect(classifyToolName("  Bash  ").icon).toBe("terminal");
  });
});
