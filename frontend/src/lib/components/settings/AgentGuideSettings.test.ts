import { describe, expect, it } from "vitest";
import { render, fireEvent } from "@testing-library/svelte";
import AgentGuideSettings from "./AgentGuideSettings.svelte";
import { loadSettings } from "../../stores/settings.svelte";
import { setBindingMock, getBindingMock } from "../../../test/mocks/bindings-app";
import type { Settings } from "../../types/settings";
import { makeSettings } from "../../../test/helpers/settings";
import { setPageGrantsFromBootstrap } from "../../transport/scopes";

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

// The switch decides whether a session spawned on this computer gets the app
// guide appended to its system prompt. It is a single user-tier boolean, so
// the component's whole job is to show the saved value and send the patch.
describe("<AgentGuideSettings>", () => {
  it("shows the switch on when the setting is on", async () => {
    await seed({ agentGuideEnabled: true });
    const { findByRole } = render(AgentGuideSettings);

    const toggle = (await findByRole("switch")) as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(toggle.getAttribute("aria-label")).toBe("Toggle app guide");
    expect(toggle.disabled).toBe(false);
  });

  it("shows the switch off when the setting is off", async () => {
    await seed({ agentGuideEnabled: false });
    const { findByRole } = render(AgentGuideSettings);

    const toggle = (await findByRole("switch")) as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("false");
  });

  it("sends only agentGuideEnabled when it is turned off", async () => {
    await seed({ agentGuideEnabled: true });
    const { findByRole } = render(AgentGuideSettings);

    await fireEvent.click(await findByRole("switch"));

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls.at(-1)![0]).toEqual({ agentGuideEnabled: false });
  });

  it("sends agentGuideEnabled true when it is turned back on", async () => {
    await seed({ agentGuideEnabled: false });
    const { findByRole } = render(AgentGuideSettings);

    await fireEvent.click(await findByRole("switch"));

    expect(getBindingMock("UpdateSettings")!.mock.calls.at(-1)![0]).toEqual({
      agentGuideEnabled: true,
    });
  });

  // agentGuideEnabled is user tier (internal/settings/tier.go): the backend
  // takes the write without a step-up proof, so a networked page with no host
  // presence and no passkey keeps the switch live.
  it("stays writable off the host without a passkey", async () => {
    await seed({ agentGuideEnabled: true });
    setPageGrantsFromBootstrap(true);
    try {
      const { findByRole } = render(AgentGuideSettings);
      const toggle = (await findByRole("switch")) as HTMLButtonElement;
      expect(toggle.disabled).toBe(false);
      expect(toggle.title).toBe("");

      await fireEvent.click(toggle);
      expect(getBindingMock("UpdateSettings")!.mock.calls.at(-1)![0]).toEqual({
        agentGuideEnabled: false,
      });
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });
});
