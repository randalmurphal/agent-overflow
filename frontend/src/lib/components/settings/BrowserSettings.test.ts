import { describe, expect, it } from "vitest";
import { render, fireEvent } from "@testing-library/svelte";
import BrowserSettings from "./BrowserSettings.svelte";
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
  // The section's only other binding, and only on the destructive button.
  // Mocked anyway so a stray call fails as an assertion rather than as a
  // transport error from a component test.
  setBindingMock("ClearBrowserSiteData", async () => undefined);
  await loadSettings();
  return merged;
}

// The Chromium path is the one browser setting with no effect on this
// machine: it names the browser a SERVE host launches, which is why the
// field is a plain path input rather than a toggle, and why an empty value
// has to stay meaningful (search PATH) instead of being an error.
describe("<BrowserSettings> — Chromium path", () => {
  it("shows the saved path, and the PATH hint when there is none", async () => {
    await seed();
    const { findByTestId } = render(BrowserSettings);

    const input = (await findByTestId(
      "settings-browser-chromium-path",
    )) as HTMLInputElement;
    expect(input.value).toBe("");
    expect(input.placeholder).toBe("Found on PATH when empty");
  });

  it("renders the configured path", async () => {
    await seed({ browserChromiumPath: "/opt/chromium/chrome" });
    const { findByTestId } = render(BrowserSettings);

    const input = (await findByTestId(
      "settings-browser-chromium-path",
    )) as HTMLInputElement;
    expect(input.value).toBe("/opt/chromium/chrome");
  });

  it("saves the path on change", async () => {
    await seed();
    const { findByTestId } = render(BrowserSettings);

    const input = await findByTestId("settings-browser-chromium-path");
    await fireEvent.change(input, {
      target: { value: "/usr/bin/chromium-browser" },
    });

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls.at(-1)![0]).toEqual({
      browserChromiumPath: "/usr/bin/chromium-browser",
    });
  });

  // Clearing the field is how an operator goes back to searching PATH, so
  // the empty string has to reach the backend as a value rather than being
  // dropped as falsy.
  it("saves an emptied path", async () => {
    await seed({ browserChromiumPath: "/opt/chromium/chrome" });
    const { findByTestId } = render(BrowserSettings);

    const input = await findByTestId("settings-browser-chromium-path");
    await fireEvent.change(input, { target: { value: "" } });

    const mock = getBindingMock("UpdateSettings");
    expect(mock!.mock.calls.at(-1)![0]).toEqual({ browserChromiumPath: "" });
  });
});
