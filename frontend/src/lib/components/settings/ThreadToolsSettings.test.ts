import { describe, expect, it } from "vitest";
import { render, fireEvent } from "@testing-library/svelte";
import ThreadToolsSettings from "./ThreadToolsSettings.svelte";
import { loadSettings } from "../../stores/settings.svelte";
import { setBindingMock, getBindingMock } from "../../../test/mocks/bindings-app";
import type { Settings } from "../../types/settings";
import { makeSettings } from "../../../test/helpers/settings";
import { setPageGrantsFromBootstrap } from "../../transport/scopes";
import { HOST_TIER_REASON } from "./settingsComputer";

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

// The switch decides whether a provider session on this machine is handed
// the ao-thread-tools server at all. It is a single host-tier boolean, so
// the component's whole job is to show the saved value and send the patch.
describe("<ThreadToolsSettings>", () => {
  it("shows the switch on when the setting is on", async () => {
    await seed({ threadToolsEnabled: true });
    const { findByRole } = render(ThreadToolsSettings);

    const toggle = (await findByRole("switch")) as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect(toggle.getAttribute("aria-label")).toBe("Toggle Built-in Thread Tools");
    expect(toggle.disabled).toBe(false);
  });

  it("shows the switch off when the setting is off", async () => {
    await seed({ threadToolsEnabled: false });
    const { findByRole } = render(ThreadToolsSettings);

    const toggle = (await findByRole("switch")) as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("false");
  });

  it("sends only threadToolsEnabled when it is turned off", async () => {
    await seed({ threadToolsEnabled: true });
    const { findByRole } = render(ThreadToolsSettings);

    await fireEvent.click(await findByRole("switch"));

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls.at(-1)![0]).toEqual({ threadToolsEnabled: false });
  });

  it("sends threadToolsEnabled true when it is turned back on", async () => {
    await seed({ threadToolsEnabled: false });
    const { findByRole } = render(ThreadToolsSettings);

    await fireEvent.click(await findByRole("switch"));

    expect(getBindingMock("UpdateSettings")!.mock.calls.at(-1)![0]).toEqual({
      threadToolsEnabled: true,
    });
  });

  // threadToolsEnabled is host tier (internal/settings/tier.go): the switch
  // grants a session on this machine authority over this computer's other
  // conversations, so a networked page without the step-up proof sees it
  // inert rather than getting a refusal from the backend.
  it("renders inert off the host without a passkey, saying why", async () => {
    await seed({ threadToolsEnabled: true });
    setPageGrantsFromBootstrap(true);
    try {
      const { findByRole } = render(ThreadToolsSettings);
      const toggle = (await findByRole("switch")) as HTMLButtonElement;
      expect(toggle.disabled).toBe(true);
      expect(toggle.title).toBe(HOST_TIER_REASON);

      await fireEvent.click(toggle);
      expect(getBindingMock("UpdateSettings")?.mock.calls ?? []).toHaveLength(0);
    } finally {
      setPageGrantsFromBootstrap(false);
    }
  });
});
