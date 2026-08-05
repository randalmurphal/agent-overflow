import { describe, expect, it, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/svelte";
import ProviderSettings from "./ProviderSettings.svelte";
import { loadSettings } from "../../stores/settings.svelte";
import { getToasts } from "../../stores/toast.svelte";
import type { ModelInfo } from "../../types/settings";
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
  setBindingMock("GetProviderStatuses", async () => []);
  setBindingMock("GetModelsForProvider", async () => []);
  setBindingMock("ListProviderAccounts", async () => []);
  await loadSettings();
  return merged;
}

const CLAUDE_CATALOG: ModelInfo[] = [
  { slug: "claude-fable-5", name: "Claude Fable 5", provider: "claude" },
  { slug: "claude-opus-4-8", name: "Claude Opus 4.8", provider: "claude" },
];

const CODEX_CATALOG: ModelInfo[] = [
  { slug: "gpt-5.6-sol", name: "GPT-5.6-Sol", provider: "codex" },
  { slug: "gpt-5.5", name: "GPT-5.5", provider: "codex" },
];

describe("<ProviderSettings> — model visibility toggles", () => {
	it("renders friendly Codex model aliases", async () => {
		await seed();
		setBindingMock("GetModelsForProvider", async (provider: unknown) =>
			provider === "codex" ? CODEX_CATALOG : [],
		);
		const { findByTestId } = render(ProviderSettings);

		const chip = await findByTestId("settings-model-toggle-codex-gpt-5.6-sol");
		expect(chip.textContent?.trim()).toBe("GPT 5.6 Sol");
	});

  it("dispatches a hide patch when clicking a visible model chip", async () => {
    await seed();
    setBindingMock("GetModelsForProvider", async () => CLAUDE_CATALOG);
    const { findByTestId } = render(ProviderSettings);

    const chip = await findByTestId("settings-model-toggle-claude-claude-opus-4-8");
    expect(chip.getAttribute("data-hidden")).toBe("false");
    await fireEvent.click(chip);

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls.at(-1)![0]).toEqual({
      claudeHiddenModels: ["claude-opus-4-8"],
    });
  });

  it("marks hidden models and dispatches an unhide patch on click", async () => {
    await seed({ claudeHiddenModels: ["claude-opus-4-8"] });
    setBindingMock("GetModelsForProvider", async () => CLAUDE_CATALOG);
    const { findByTestId } = render(ProviderSettings);

    const chip = await findByTestId("settings-model-toggle-claude-claude-opus-4-8");
    expect(chip.getAttribute("data-hidden")).toBe("true");
    await fireEvent.click(chip);

    const mock = getBindingMock("UpdateSettings");
    expect(mock!.mock.calls.at(-1)![0]).toEqual({ claudeHiddenModels: [] });
  });

  it("refuses to hide the last visible model", async () => {
    await seed({ claudeHiddenModels: ["claude-opus-4-8"] });
    setBindingMock("GetModelsForProvider", async () => CLAUDE_CATALOG);
    const { findByTestId } = render(ProviderSettings);

    const chip = await findByTestId("settings-model-toggle-claude-claude-fable-5");
    await fireEvent.click(chip);

    const mock = getBindingMock("UpdateSettings");
    expect(mock!.mock.calls.length).toBe(0);
    // The refusal must be visible, not a silent no-op.
    expect(
      getToasts().some((toast) => toast.message.includes("At least one model must stay visible")),
    ).toBe(true);
  });
});

describe("<ProviderSettings> — provider accounts", () => {
  it("renders dynamic account-scoped usage buckets", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => [{
      id: "codex-secondary",
      provider: "codex",
      email: "second@example.com",
      subscriptionType: "pro",
      addedAt: 1,
      lastUsedAt: 2,
      active: true,
      rateLimits: {
        provider: "codex",
        accountId: "codex-secondary",
        updatedAt: 3,
        limits: [
          {
            limitId: "spark",
            limitName: "GPT-5.3-Codex-Spark",
            usedPercent: 46,
            windowMins: 300,
            resetsAt: Math.floor(Date.now() / 1000) + 3600,
          },
        ],
      },
    }]);

    const { findByTestId, findByText } = render(ProviderSettings);
    expect(await findByTestId("provider-account-codex-secondary")).toBeTruthy();
    expect(await findByText("second@example.com")).toBeTruthy();
    expect(await findByText("GPT-5.3-Codex-Spark · 5h")).toBeTruthy();
    expect(await findByText("46%")).toBeTruthy();
  });

  it("states clearly that a failed switch did not happen", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => [{
      id: "claude-secondary",
      provider: "claude",
      email: "second@example.com",
      addedAt: 1,
      lastUsedAt: 2,
      active: false,
    }]);
    setBindingMock("SwitchProviderAccount", async () => {
      throw new Error("credential file is unavailable");
    });

    const { findByRole } = render(ProviderSettings);
    await fireEvent.click(await findByRole("button", { name: "Switch to second@example.com" }));

    expect(
      getToasts().some((toast) => toast.message.includes("Claude account did not switch")),
    ).toBe(true);
  });

  it("removes the active account and shows that the next saved account becomes active", async () => {
    await seed();
    let accounts = [
      {
        id: "claude-active",
        provider: "claude",
        email: "active@example.com",
        addedAt: 2,
        lastUsedAt: 2,
        active: true,
      },
      {
        id: "claude-next",
        provider: "claude",
        email: "next@example.com",
        addedAt: 1,
        lastUsedAt: 1,
        active: false,
      },
    ];
    setBindingMock("ListProviderAccounts", async () => accounts);
    setBindingMock("RemoveProviderAccount", async () => {
      accounts = [{ ...accounts[1], active: true }];
    });

    const { findByRole, findByText } = render(ProviderSettings);
    await fireEvent.click(await findByRole("button", { name: "Remove active@example.com" }));
    expect(await findByText(/next saved Claude account will become active/i)).toBeTruthy();
    await fireEvent.click(await findByRole("button", { name: "Remove" }));

    await waitFor(() => {
      expect(getBindingMock("RemoveProviderAccount")?.mock.calls[0]).toEqual([
        "claude",
        "claude-active",
      ]);
    });
    expect(await findByRole("button", { name: "next@example.com is active" })).toBeTruthy();
  });

  it("allows removing the final saved account with an explicit sign-out confirmation", async () => {
    await seed();
    let accounts = [{
      id: "codex-only",
      provider: "codex",
      email: "only@example.com",
      addedAt: 1,
      lastUsedAt: 1,
      active: true,
    }];
    setBindingMock("ListProviderAccounts", async () => accounts);
    setBindingMock("RemoveProviderAccount", async () => {
      accounts = [];
    });

    const { findByRole, findByText } = render(ProviderSettings);
    await fireEvent.click(await findByRole("button", { name: "Remove only@example.com" }));
    expect(await findByText(/sign out of Codex/i)).toBeTruthy();
    await fireEvent.click(await findByRole("button", { name: "Remove" }));

    await waitFor(() => {
      expect(getBindingMock("RemoveProviderAccount")?.mock.calls[0]).toEqual([
        "codex",
        "codex-only",
      ]);
    });
  });
});

describe("<ProviderSettings> — Text generation section", () => {
  beforeEach(async () => {
    await seed();
  });

  it("renders the text-generation section with all three controls", async () => {
    const { getByTestId } = render(ProviderSettings);
    expect(getByTestId("settings-text-generation")).toBeTruthy();
    expect(getByTestId("settings-textgen-provider")).toBeTruthy();
    expect(getByTestId("settings-textgen-model")).toBeTruthy();
    expect(getByTestId("settings-textgen-effort")).toBeTruthy();
  });

  it("lists codex and claude as the provider options", async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId(
      "settings-textgen-provider",
    ) as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values.sort()).toEqual(["claude", "codex"]);
  });

  it("shows the codex default model in the placeholder when provider is codex", async () => {
    await seed({ textGenerationProvider: "codex" });
    const { getByTestId } = render(ProviderSettings);
    const input = getByTestId("settings-textgen-model") as HTMLInputElement;
    expect(input.placeholder).toContain("gpt-5.6-luna");
  });

  it("shows the claude default model in the placeholder when provider is claude", async () => {
    await seed({ textGenerationProvider: "claude" });
    const { getByTestId } = render(ProviderSettings);
    const input = getByTestId("settings-textgen-model") as HTMLInputElement;
    expect(input.placeholder).toContain("claude-haiku-4-5");
  });

  it("dispatches textGenerationProvider patch on change", async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId(
      "settings-textgen-provider",
    ) as HTMLSelectElement;
    select.value = "claude";
    await fireEvent.change(select);

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({
      textGenerationProvider: "claude",
    });
  });

  it("dispatches textGenerationModel patch on change", async () => {
    const { getByTestId } = render(ProviderSettings);
    const input = getByTestId("settings-textgen-model") as HTMLInputElement;
    input.value = "gpt-5.4";
    await fireEvent.change(input);

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({ textGenerationModel: "gpt-5.4" });
  });

  it("lists codex reasoning-effort tiers", async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId("settings-textgen-effort") as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual([
      "none",
      "minimal",
      "low",
      "medium",
      "high",
      "xhigh",
      "max",
      "ultra",
    ]);
    const labels = Array.from(select.options).map((o) => o.textContent);
    expect(labels).toContain("xHigh");
  });

  it("lists claude reasoning-effort tiers without codex-only values", async () => {
    await seed({ textGenerationProvider: "claude" });
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId("settings-textgen-effort") as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(["low", "medium", "high", "xhigh", "max"]);
    const labels = Array.from(select.options).map((o) => o.textContent);
    expect(labels).toContain("xHigh");
    expect(labels).not.toContain("Extra High");
  });

  it("resets incompatible text-generation effort when provider changes", async () => {
    await seed({
      textGenerationProvider: "codex",
      textGenerationReasoningEffort: "ultra",
    });
    const { getByTestId } = render(ProviderSettings);
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

  it("dispatches textGenerationReasoningEffort patch on change", async () => {
    const { getByTestId } = render(ProviderSettings);
    const select = getByTestId("settings-textgen-effort") as HTMLSelectElement;
    select.value = "medium";
    await fireEvent.change(select);

    const mock = getBindingMock("UpdateSettings");
    expect(mock).toBeDefined();
    expect(mock!.mock.calls[0][0]).toEqual({
      textGenerationReasoningEffort: "medium",
    });
  });
});
