import AccountsSettings from './AccountsSettings.svelte';
// The shared provider page, driven through the two pages that mount it, so
// what is asserted is what the settings router actually renders.
import { describe, expect, it, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/svelte";
import ClaudeSettings from "./ClaudeSettings.svelte";
import CodexSettings from "./CodexSettings.svelte";
import { getSettings, loadSettings } from "../../stores/settings.svelte";
import { getToasts } from "../../stores/toast.svelte";
import type { ModelInfo } from "../../types/settings";
import {
  setBindingMock,
  getBindingMock,
} from "../../../test/mocks/bindings-app";
import type { Settings } from "../../types/settings";
import { makeSettings } from "../../../test/helpers/settings";
import { resetForTest as resetProviderAccounts } from "../../stores/providerAccounts.svelte";
import { resetForTest as resetAccountInfo } from "../../stores/accountInfo.svelte";
import { resetForTest as resetRateLimits } from "../../stores/rateLimitsInfo.svelte";
import { resetProviderModelsForTest } from "../../stores/providerModels.svelte";

const BASE_SETTINGS: Settings = makeSettings();

// The account listing and the model catalog live in module-global stores, not
// in the component, so without this every test inherits the previous one's.
beforeEach(() => {
  resetProviderAccounts();
  resetAccountInfo();
  resetRateLimits();
  resetProviderModelsForTest();
});

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

describe("provider page — model visibility toggles", () => {
	it("renders friendly Codex model aliases", async () => {
		await seed();
		setBindingMock("GetModelsForProvider", async (provider: unknown) =>
			provider === "codex" ? CODEX_CATALOG : [],
		);
		const { findByTestId } = render(CodexSettings);

		const chip = await findByTestId("settings-model-toggle-codex-gpt-5.6-sol");
		expect(chip.textContent?.trim()).toBe("GPT 5.6 Sol");
	});

  it("hides a model on this frontend when clicking its chip", async () => {
    await seed();
    setBindingMock("GetModelsForProvider", async () => CLAUDE_CATALOG);
    const { findByTestId } = render(ClaudeSettings);

    const chip = await findByTestId("settings-model-toggle-claude-claude-opus-4-8");
    expect(chip.getAttribute("data-hidden")).toBe("false");
    await fireEvent.click(chip);

    expect(getSettings().claudeHiddenModels).toEqual(["claude-opus-4-8"]);
    expect(chip.getAttribute("data-hidden")).toBe("true");
    expect(getBindingMock("UpdateSettings")).not.toHaveBeenCalled();
  });

  it("unhides a model on this frontend when clicking its chip", async () => {
    await seed({ claudeHiddenModels: ["claude-opus-4-8"] });
    setBindingMock("GetModelsForProvider", async () => CLAUDE_CATALOG);
    const { findByTestId } = render(ClaudeSettings);

    const chip = await findByTestId("settings-model-toggle-claude-claude-opus-4-8");
    expect(chip.getAttribute("data-hidden")).toBe("true");
    await fireEvent.click(chip);

    expect(getSettings().claudeHiddenModels).toEqual([]);
    expect(chip.getAttribute("data-hidden")).toBe("false");
    expect(getBindingMock("UpdateSettings")).not.toHaveBeenCalled();
  });

  it("refuses to hide the last visible model", async () => {
    await seed({ claudeHiddenModels: ["claude-opus-4-8"] });
    setBindingMock("GetModelsForProvider", async () => CLAUDE_CATALOG);
    const { findByTestId } = render(ClaudeSettings);

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

describe("provider page — Claude TUI visibility toggle", () => {
  // claude-tui has no page of its own (it reuses claude's binary, auth and
  // catalog), so its one flag rides in the Claude page's Setup section.
  function tuiToggle(container: HTMLElement): HTMLButtonElement {
    return container.querySelector<HTMLButtonElement>(
      '[data-testid="settings-provider-claude"] button[aria-label="Toggle Claude TUI"]',
    )!;
  }

  it("renders under Claude, off by default, and never under Codex", async () => {
    await seed();
    const { container } = render(ClaudeSettings);

    const toggle = tuiToggle(container);
    expect(toggle).not.toBeNull();
    expect(toggle.getAttribute("aria-checked")).toBe("false");

    const codex = render(CodexSettings);
    expect(
      codex.container.querySelector('button[aria-label="Toggle Claude TUI"]'),
    ).toBeNull();
    // And it must not have grown a page of its own.
    expect(
      codex.container.querySelector('[data-testid="settings-provider-claude-tui"]'),
    ).toBeNull();
  });

  it("writes claudeTuiEnabled — and only that key — when switched on", async () => {
    await seed();
    const { container } = render(ClaudeSettings);
    await fireEvent.click(tuiToggle(container));

    const mock = getBindingMock("UpdateSettings");
    expect(mock!.mock.calls.at(-1)![0]).toEqual({ claudeTuiEnabled: true });
  });

  it("reflects an enabled flag and writes false to turn it back off", async () => {
    await seed({ claudeTuiEnabled: true });
    const { container } = render(ClaudeSettings);

    const toggle = tuiToggle(container);
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    await fireEvent.click(toggle);
    expect(getBindingMock("UpdateSettings")!.mock.calls.at(-1)![0]).toEqual({
      claudeTuiEnabled: false,
    });
  });

  it("goes inert while Claude is disabled, because the value cannot change any picker", async () => {
    await seed({ claudeEnabled: false, claudeTuiEnabled: true });
    const { container } = render(ClaudeSettings);

    const toggle = tuiToggle(container);
    expect(toggle.disabled).toBe(true);
    await fireEvent.click(toggle);
    expect(getBindingMock("UpdateSettings")?.mock.calls.length ?? 0).toBe(0);

    // Claude's own toggle stays live — it is how the user gets back.
    const claudeToggle = container.querySelector<HTMLButtonElement>(
      '[data-testid="settings-provider-claude"] button[aria-label="Toggle Claude"]',
    )!;
    expect(claudeToggle.disabled).toBe(false);
  });

  it("explains what enabling it turns on", async () => {
    await seed();
    const { getByText } = render(ClaudeSettings);
    expect(
      getByText(/Interactive terminal sessions driven through the real Claude TUI/i),
    ).toBeTruthy();
  });
});

describe("provider page — sections render regardless of the enable toggle", () => {
  // A provider is commonly configured before it is switched on, and the old
  // Prompts & Tools tab used to hide every override behind that toggle.
  it("still renders prompt, tool and context sections for a disabled provider", async () => {
    await seed({ claudeEnabled: false, codexEnabled: false });

    const claude = render(ClaudeSettings);
    expect(claude.getByTestId("settings-prompts-claude")).toBeTruthy();
    expect(claude.getByTestId("settings-claude-tools-claude")).toBeTruthy();
    expect(claude.getByTestId("settings-context-claude")).toBeTruthy();
    expect(claude.getByTestId("settings-claude-session-axes")).toBeTruthy();
    expect(claude.getByTestId("settings-claude-cross-session")).toBeTruthy();

    const codex = render(CodexSettings);
    expect(codex.getByTestId("settings-prompts-codex")).toBeTruthy();
    expect(codex.getByTestId("settings-codex-tools-codex")).toBeTruthy();
    expect(codex.getByTestId("settings-context-codex")).toBeTruthy();
  });

  it("keeps the Claude-only sections off the Codex page", async () => {
    await seed();
    const { queryByTestId } = render(CodexSettings);
    expect(queryByTestId("settings-claude-session-axes")).toBeNull();
    expect(queryByTestId("settings-claude-cross-session")).toBeNull();
    expect(queryByTestId("settings-claude-tools-codex")).toBeNull();
  });
});

describe("provider page — when a change takes effect", () => {
  // The section headers are the only place the page states what a save does,
  // and the answer differs per provider: a Claude prompt edit converges live
  // through `set_model.system_prompt`, everything else is spawn-only.
  it("states the live-and-deferred halves on the Claude page", async () => {
    await seed();
    const { getByText } = render(ClaudeSettings);
    expect(
      getByText(/reaches running headless sessions right away/),
    ).toBeTruthy();
    expect(
      getByText(/turning one off applies when the session restarts/),
    ).toBeTruthy();
    expect(getByText(/Claude TUI sessions pick it up on their next start/)).toBeTruthy();
  });

  it("states the spawn-only rule on the Codex page", async () => {
    await seed();
    const { getAllByText } = render(CodexSettings);
    // Both the system-prompt and the tools section carry it.
    expect(getAllByText(/Applies to sessions started later\./).length).toBe(2);
  });
});

describe("provider page — provider accounts", () => {
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

    const { findByTestId, findByText } = render(AccountsSettings);
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

    const { findByRole } = render(AccountsSettings);
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

    const { findByRole, findByText } = render(AccountsSettings);
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

    const { findByRole, findByText } = render(AccountsSettings);
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
