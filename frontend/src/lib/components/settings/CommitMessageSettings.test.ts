// The commit / PR / thread-title text-generation route, which is its own
// page because it is chosen independently of the chat provider.
import { describe, expect, it, beforeEach } from "vitest";
import { render, fireEvent } from "@testing-library/svelte";
import CommitMessageSettings from "./CommitMessageSettings.svelte";
import { loadSettings } from "../../stores/settings.svelte";
import {
  setBindingMock,
  getBindingMock,
} from "../../../test/mocks/bindings-app";
import type { Settings } from "../../types/settings";
import { makeSettings } from "../../../test/helpers/settings";

const BASE_SETTINGS: Settings = makeSettings();

async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged: Settings = { ...BASE_SETTINGS, ...overrides };
  setBindingMock("GetSettings", async () => merged);
  setBindingMock("UpdateSettings", async (patch: unknown) => {
    const p = (patch as Record<string, unknown>) ?? {};
    return { ...merged, ...p };
  });
  await loadSettings();
  return merged;
}

describe("<CommitMessageSettings>", () => {
  beforeEach(async () => {
    await seed();
  });

  it("renders the text-generation section with all three controls", async () => {
    const { getByTestId } = render(CommitMessageSettings);
    expect(getByTestId("settings-text-generation")).toBeTruthy();
    expect(getByTestId("settings-textgen-provider")).toBeTruthy();
    expect(getByTestId("settings-textgen-model")).toBeTruthy();
    expect(getByTestId("settings-textgen-effort")).toBeTruthy();
  });

  it("lists agy, claude, and codex as the provider options", async () => {
    const { getByTestId } = render(CommitMessageSettings);
    const select = getByTestId(
      "settings-textgen-provider",
    ) as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values.sort()).toEqual(["agy", "claude", "codex"]);
  });

  it.each([
    ["codex", "Default: newest gpt-luna"],
    ["claude", "Default: newest claude-haiku"],
  ] as const)("shows the %s default model in the placeholder", async (provider, placeholder) => {
    await seed({ textGenerationProvider: provider });
    const { getByTestId } = render(CommitMessageSettings);
    const input = getByTestId("settings-textgen-model") as HTMLInputElement;
    expect(input.placeholder).toBe(placeholder);
  });

  it.each([
    ["textGenerationProvider", "settings-textgen-provider", "claude"],
    ["textGenerationModel", "settings-textgen-model", "gpt-5.4"],
    ["textGenerationReasoningEffort", "settings-textgen-effort", "medium"],
  ])("dispatches %s patch on change", async (key, testId, value) => {
    const { getByTestId } = render(CommitMessageSettings);
    const control = getByTestId(testId) as HTMLInputElement | HTMLSelectElement;
    control.value = value;
    await fireEvent.change(control);

    expect(getBindingMock("UpdateSettings")!.mock.calls[0][0]).toEqual({ [key]: value });
  });

  it.each([
    ["codex", ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"]],
    ["claude", ["low", "medium", "high", "xhigh", "max"]],
  ] as const)("lists %s reasoning-effort tiers", async (provider, tiers) => {
    await seed({ textGenerationProvider: provider });
    const { getByTestId } = render(CommitMessageSettings);
    const select = getByTestId("settings-textgen-effort") as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.value)).toEqual(tiers);
    const labels = Array.from(select.options).map((o) => o.textContent);
    expect(labels).toContain("xHigh");
    expect(labels).not.toContain("Extra High");
  });

  it("resets incompatible text-generation effort when provider changes", async () => {
    await seed({
      textGenerationProvider: "codex",
      textGenerationReasoningEffort: "ultra",
    });
    const { getByTestId } = render(CommitMessageSettings);
    const select = getByTestId(
      "settings-textgen-provider",
    ) as HTMLSelectElement;
    select.value = "claude";
    await fireEvent.change(select);

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({
      textGenerationProvider: "claude",
      textGenerationReasoningEffort: "low",
    });
  });
});
