// Tool-name classification table for the ToolCallCard header. Pure
// function; the tests walk every branch including MCP/ prefix priority
// over the switch table and the null/empty fallback.

import { describe, expect, it } from "vitest";
import { classifyToolName } from "./toolCardHeader";

describe("classifyToolName", () => {
  it("returns the generic fallback for null", () => {
    expect(classifyToolName(null)).toEqual({
      icon: "generic",
      label: "Tool",
      displayName: "Tool",
      isSubagent: false,
    });
  });

  it("returns the generic fallback for undefined", () => {
    expect(classifyToolName(undefined)).toEqual({
      icon: "generic",
      label: "Tool",
      displayName: "Tool",
      isSubagent: false,
    });
  });

  it("returns the generic fallback for empty string", () => {
    expect(classifyToolName("")).toEqual({
      icon: "generic",
      label: "Tool",
      displayName: "Tool",
      isSubagent: false,
    });
  });

  it("trims whitespace-only tool names to the generic fallback", () => {
    // Whitespace-only strings are functionally unnamed tools; we
    // should not fall through to a bare "Tool" display with a
    // whitespace displayName.
    expect(classifyToolName("   ")).toEqual({
      icon: "generic",
      label: "Tool",
      displayName: "Tool",
      isSubagent: false,
    });
  });

  it("MCP/ prefix wins over the switch table", () => {
    // Even a suffix that matches an entry in the switch (e.g. "Bash"
    // as an MCP tool name) must still be classified as MCP.
    const out = classifyToolName("MCP/Bash");
    expect(out.icon).toBe("puzzle");
    expect(out.label).toBe("MCP");
    expect(out.displayName).toBe("Bash");
    expect(out.isSubagent).toBe(false);
  });

  it('MCP/ with empty suffix falls back to "MCP tool" display', () => {
    expect(classifyToolName("MCP/").displayName).toBe("MCP tool");
  });

  it("Bash maps to terminal icon", () => {
    expect(classifyToolName("Bash").icon).toBe("terminal");
  });

  it.each(["Edit", "Write", "MultiEdit"])(
    "%s maps to file icon with matching label",
    (name) => {
      const out = classifyToolName(name);
      expect(out.icon).toBe("file");
      expect(out.label).toBe(name);
      expect(out.displayName).toBe(name);
      expect(out.isSubagent).toBe(false);
    },
  );

  it("Read maps to the eye icon", () => {
    const out = classifyToolName("Read");
    expect(out.icon).toBe("eye");
    expect(out.label).toBe("Read");
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
      expect(out.label).toBe("Image");
      expect(out.displayName).toBe(name);
      expect(out.isSubagent).toBe(false);
    },
  );

  it("bare MCP maps to the MCP tool bucket", () => {
    const out = classifyToolName("MCP");
    expect(out.icon).toBe("puzzle");
    expect(out.label).toBe("MCP");
    expect(out.displayName).toBe("MCP tool");
  });

  it("Agent classifies as subagent and uses robot icon", () => {
    const out = classifyToolName("Agent");
    expect(out.icon).toBe("robot");
    expect(out.label).toBe("Subagent");
    expect(out.displayName).toBe("Agent");
    expect(out.isSubagent).toBe(true);
  });

  it("Task no longer carries a special classification (Claude renamed the tool)", () => {
    const out = classifyToolName("Task");
    expect(out.icon).toBe("generic");
    expect(out.isSubagent).toBe(false);
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
    expect(out.label).toBe("Wait");
    expect(out.displayName).toBe("Wait for agent");
    expect(out.isSubagent).toBe(false);
  });

  it.each([
    ["close_agent", "Close", "Close agent"],
    ["resume_agent", "Resume", "Resume agent"],
  ])(
    "%s maps to a non-parent collab control row",
    (name, label, displayName) => {
      const out = classifyToolName(name);
      expect(out.icon).toBe("robot");
      expect(out.label).toBe(label);
      expect(out.displayName).toBe(displayName);
      expect(out.isSubagent).toBe(false);
    },
  );

  it.each(["Plan", "ExitPlanMode"])("%s maps to checklist icon", (name) => {
    expect(classifyToolName(name).icon).toBe("checklist");
  });

  it("unknown tool names fall through to the generic category but preserve the display name", () => {
    const out = classifyToolName("SomeCustomTool");
    expect(out.icon).toBe("generic");
    expect(out.label).toBe("Tool");
    expect(out.displayName).toBe("SomeCustomTool");
    expect(out.isSubagent).toBe(false);
  });

  it('case-sensitive matching — "bash" (lowercase) falls through to generic', () => {
    // The switch table uses exact provider tool names; a lowercase
    // variant is treated as custom/unknown so the display shows what
    // the provider actually sent.
    expect(classifyToolName("bash").icon).toBe("generic");
    expect(classifyToolName("bash").displayName).toBe("bash");
  });

  it("preserves leading/trailing whitespace around the display name after trim", () => {
    // Whitespace is trimmed at entry so "  Bash  " routes to the Bash
    // branch rather than falling to generic.
    expect(classifyToolName("  Bash  ").icon).toBe("terminal");
  });
});
