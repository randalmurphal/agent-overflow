// The shared provider page, driven through the two pages that mount it, so
// what is asserted is what the settings router actually renders.
import { afterEach, describe, expect, it, beforeEach } from "vitest";
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
import { modelCatalog } from "../../../test/helpers/modelCatalog";
import { resetForTest as resetProviderAccounts } from "../../stores/providerAccounts.svelte";
import { resetForTest as resetAccountInfo } from "../../stores/accountInfo.svelte";
import { resetForTest as resetRateLimits } from "../../stores/rateLimitsInfo.svelte";
import { resetProviderModelsForTest } from "../../stores/providerModels.svelte";
import { OBSERVE_SCOPES, pairWithScopes, resetToLocalPage } from "../../../test/helpers/scopes";
import { setPageGrantsFromBootstrap } from "../../transport/scopes";
import { HOST_TIER_REASON } from "./settingsComputer";

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
  setBindingMock("GetModelsForProvider", async () => modelCatalog([], "shipped"));
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
			provider === "codex" ? modelCatalog(CODEX_CATALOG) : modelCatalog([], "shipped"),
		);
		const { findByTestId } = render(CodexSettings);

		const chip = await findByTestId("settings-model-toggle-codex-gpt-5.6-sol");
		expect(chip.textContent?.trim()).toBe("GPT 5.6 Sol");
	});

  it("hides a model on this frontend when clicking its chip", async () => {
    await seed();
    setBindingMock("GetModelsForProvider", async () => modelCatalog(CLAUDE_CATALOG));
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
    setBindingMock("GetModelsForProvider", async () => modelCatalog(CLAUDE_CATALOG));
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
    setBindingMock("GetModelsForProvider", async () => modelCatalog(CLAUDE_CATALOG));
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
  // The section headers are the only place the page states what a save does.
  // A prompt change never reaches a running session on any provider: it
  // applies at the next session start, and a running thread gets a banner
  // offering the restart (docs/decisions.md, 2026-10-14). Tools are
  // spawn-only everywhere.
  it("states the next-start rule and the banner for prompts on both pages", async () => {
    await seed();
    for (const Page of [ClaudeSettings, CodexSettings]) {
      const { getByText, unmount } = render(Page);
      expect(getByText(/Applies when a thread's session next starts\./)).toBeTruthy();
      expect(getByText(/Running threads show a banner offering a restart\./)).toBeTruthy();
      expect(getByText(/^Applies to sessions started later\.$/)).toBeTruthy();
      unmount();
    }
  });
});

describe("provider page — host-tier keys off the host", () => {
  // claudeEnabled / claudeTuiEnabled and the binary path are host tier
  // (internal/settings/tier.go): the backend refuses the write without a
  // step-up proof, so a networked page with no host presence and no
  // passkey shows the control inert with the reason.
  afterEach(() => {
    setPageGrantsFromBootstrap(false);
  });

  it("disables the enable toggle, the TUI toggle and the path input with the reason", async () => {
    await seed({ claudeEnabled: true });
    setPageGrantsFromBootstrap(true);
    const { findByRole, container } = render(ClaudeSettings);
    for (const name of ["Toggle Claude", "Toggle Claude TUI"]) {
      const toggle = (await findByRole("switch", { name })) as HTMLButtonElement;
      expect(toggle.disabled, name).toBe(true);
      expect(toggle.title, name).toBe(HOST_TIER_REASON);
    }
    const path = container.querySelector("#claude-path") as HTMLInputElement;
    expect(path.disabled).toBe(true);
    expect(path.title).toBe(HOST_TIER_REASON);
  });
});

describe("provider page — provider accounts", () => {
  it("renders the log-in control inert, saying why, without access:admin", async () => {
    // Every account RPC rides `access:admin`, the listing included, so a
    // device without it sees no account cards; the one control that still
    // renders is the log-in button, inert with the reason.
    await seed();
    await pairWithScopes([...OBSERVE_SCOPES, "threads:operate"]);
    try {
      const { findByRole, queryByTestId } = render(CodexSettings);
      const login = (await findByRole("button", { name: "Log in to another account" })) as HTMLButtonElement;
      expect(login.disabled).toBe(true);
      expect(login.title).toBe("Not granted to this device");
      expect(queryByTestId("provider-account-codex-secondary")).toBeNull();
    } finally {
      resetToLocalPage();
    }
  });

  it("renders the switch, refresh and remove controls inert, saying why, without access:admin", async () => {
    // The card controls, exercised on a listing that arrived while the
    // grant was held and is still on screen after it lapsed.
    await seed();
    setBindingMock("ListProviderAccounts", async () => [{
      id: "codex-secondary",
      provider: "codex",
      email: "second@example.com",
      addedAt: 1,
      autoSwitch: true,
      active: false,
    }]);
    const { findByRole, findByTestId } = render(CodexSettings);
    await findByTestId("provider-account-codex-secondary");
    await pairWithScopes([...OBSERVE_SCOPES, "threads:operate"]);
    try {
      for (const name of [
        "Switch to second@example.com",
        "Refresh usage for second@example.com",
        "Remove second@example.com",
      ]) {
        await waitFor(() => {
          const control = document.querySelector(`button[aria-label="${name}"]`) as HTMLButtonElement | null;
          expect(control, name).not.toBeNull();
          expect(control!.disabled, name).toBe(true);
          expect(control!.title, name).toBe("Not granted to this device");
        });
      }
      expect((await findByRole("button", { name: "Log in to another account" }) as HTMLButtonElement).disabled).toBe(true);
    } finally {
      resetToLocalPage();
    }
  });

  it("renders dynamic account-scoped usage buckets", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => [{
      id: "codex-secondary",
      provider: "codex",
      email: "second@example.com",
      subscriptionType: "pro",
      addedAt: 1,
      autoSwitch: true,
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

    const { findByTestId, findByText } = render(CodexSettings);
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
      autoSwitch: true,
      active: false,
    }]);
    setBindingMock("SwitchProviderAccount", async () => {
      throw new Error("credential file is unavailable");
    });

    const { findByRole } = render(ClaudeSettings);
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
        autoSwitch: true,
        active: true,
      },
      {
        id: "claude-next",
        provider: "claude",
        email: "next@example.com",
        addedAt: 1,
        autoSwitch: true,
        active: false,
      },
    ];
    setBindingMock("ListProviderAccounts", async () => accounts);
    setBindingMock("RemoveProviderAccount", async () => {
      accounts = [{ ...accounts[1], active: true }];
    });

    const { findByRole, findByText } = render(ClaudeSettings);
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
      autoSwitch: true,
      active: true,
    }];
    setBindingMock("ListProviderAccounts", async () => accounts);
    setBindingMock("RemoveProviderAccount", async () => {
      accounts = [];
    });

    const { findByRole, findByText } = render(CodexSettings);
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

describe("provider page: account auto-switch pool", () => {
  const two = (autoSwitch: [boolean, boolean]) => [
    { id: "claude-a", provider: "claude", email: "a@x.com", addedAt: 2, active: true, autoSwitch: autoSwitch[0] },
    { id: "claude-b", provider: "claude", email: "b@y.com", addedAt: 1, active: false, autoSwitch: autoSwitch[1] },
  ];
  const box = (id: string) =>
    document.querySelector(`[data-testid="provider-account-auto-switch-${id}"]`) as HTMLInputElement | null;

  it("renders each card's pool membership from the listing", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => two([true, false]));
    const { findByTestId } = render(ClaudeSettings);
    await findByTestId("provider-account-claude-b");
    expect(box("claude-a")!.checked).toBe(true);
    expect(box("claude-b")!.checked).toBe(false);
    expect(box("claude-b")!.closest("label")!.title).toBe(
      "Let Agent Overflow switch to and from this account when a usage limit is near",
    );
  });

  it("writes the account's new value at once, without switching to it", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => two([true, true]));
    let finish!: () => void;
    const write = setBindingMock("SetProviderAccountAutoSwitch", () => new Promise<void>((resolve) => { finish = resolve; }));
    const switched = setBindingMock("SwitchProviderAccount", async () => null);
    const { findByTestId, queryByTestId } = render(ClaudeSettings);
    await findByTestId("provider-account-claude-b");
    expect(queryByTestId("provider-accounts-pool-hint-claude")).toBeNull();

    await fireEvent.click(box("claude-b")!);
    expect(write).toHaveBeenCalledWith("claude", "claude-b", false);
    // The pool line reads the store, so it shows the change before the write lands.
    expect(queryByTestId("provider-accounts-pool-hint-claude")).not.toBeNull();
    finish();
    await waitFor(() => expect(box("claude-b")!.checked).toBe(false));
    expect(switched).not.toHaveBeenCalled();
  });

  it("reverts and says so when the write is refused", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => two([true, true]));
    setBindingMock("SetProviderAccountAutoSwitch", async () => {
      throw new Error("disk full");
    });
    const { findByTestId } = render(ClaudeSettings);
    await findByTestId("provider-account-claude-b");

    await fireEvent.click(box("claude-b")!);
    await waitFor(() => expect(box("claude-b")!.checked).toBe(true));
    expect(
      getToasts().some((toast) => toast.message.includes("Claude account auto-switch was not turned off")),
    ).toBe(true);
  });

  it("is inert without access:admin", async () => {
    await seed();
    setBindingMock("ListProviderAccounts", async () => two([true, true]));
    const { findByTestId } = render(ClaudeSettings);
    await findByTestId("provider-account-claude-b");
    await pairWithScopes([...OBSERVE_SCOPES, "threads:operate"]);
    try {
      await waitFor(() => expect(box("claude-b")!.disabled).toBe(true));
      expect(box("claude-b")!.closest("label")!.title).toBe("Not granted to this device");
    } finally {
      resetToLocalPage();
    }
  });

  it("asks for a second ticked account only when there are two or more and fewer than two are ticked", async () => {
    await seed();
    const cases: { listing: unknown[]; shown: boolean }[] = [
      { listing: two([false, false]), shown: true },
      { listing: two([true, false]), shown: true },
      { listing: two([true, true]), shown: false },
      { listing: [two([false, false])[0]], shown: false },
      { listing: [two([true, true])[0]], shown: false },
    ];
    for (const { listing, shown } of cases) {
      resetProviderAccounts();
      setBindingMock("ListProviderAccounts", async () => listing);
      const view = render(ClaudeSettings);
      await view.findByTestId("provider-account-claude-a");
      expect(view.queryByTestId("provider-accounts-pool-hint-claude") !== null, JSON.stringify(listing)).toBe(shown);
      view.unmount();
    }
  });
});

describe("provider page: auto-switch settings", () => {
  const patches = () =>
    (getBindingMock("UpdateSettings")?.mock.calls ?? []).map((call) => call[0] as Record<string, unknown>);

  it("renders the current values on every provider page and says they apply to every provider", async () => {
    await seed({ accountAutoSwitchStrategy: "account-order", accountAutoSwitchThreshold: 80, accountAutoContinue: true });
    for (const Page of [ClaudeSettings, CodexSettings]) {
      const view = render(Page);
      const strategy = view.getByTestId("settings-auto-switch-strategy") as HTMLSelectElement;
      expect(strategy.value).toBe("account-order");
      expect(view.getByText("The order of the cards above.")).toBeTruthy();
      expect(view.getByLabelText("Use accounts in order of")).toBe(strategy);
      expect([...strategy.options].map((option) => option.text)).toEqual(["Soonest weekly reset", "Card order"]);
      expect((view.getByTestId("settings-auto-switch-threshold") as HTMLInputElement).value).toBe("80");
      expect(view.getByRole("switch", { name: "Continue threads after a usage limit" }).getAttribute("aria-checked")).toBe("true");
      expect(view.getByText(/apply to the accounts of every provider/)).toBeTruthy();
      expect(view.getByTestId("settings-auto-continue-warning").className).toContain("text-warning");
      view.unmount();
    }
  });

  it("writes each setting under its own key", async () => {
    await seed();
    const view = render(ClaudeSettings);
    expect(view.getByText("Uses up the account whose weekly limit resets soonest, so less quota expires unused.")).toBeTruthy();
    await fireEvent.change(view.getByTestId("settings-auto-switch-strategy"), { target: { value: "account-order" } });
    await waitFor(() => expect(patches().at(-1)).toEqual({ accountAutoSwitchStrategy: "account-order" }));
    expect(view.getByText("The order of the cards above.")).toBeTruthy();
    await fireEvent.change(view.getByTestId("settings-auto-switch-threshold"), { target: { value: "90" } });
    await waitFor(() => expect(patches().at(-1)).toEqual({ accountAutoSwitchThreshold: 90 }));
    await fireEvent.click(view.getByRole("switch", { name: "Continue threads after a usage limit" }));
    await waitFor(() => expect(patches().at(-1)).toEqual({ accountAutoContinue: true }));
  });

  it("clamps the threshold into its range and keeps the stored value for an empty entry", async () => {
    await seed({ accountAutoSwitchThreshold: 95 });
    const view = render(ClaudeSettings);
    const input = view.getByTestId("settings-auto-switch-threshold") as HTMLInputElement;
    expect(input.min).toBe("50");
    expect(input.max).toBe("100");

    await fireEvent.change(input, { target: { value: "10" } });
    await waitFor(() => expect(patches().at(-1)).toEqual({ accountAutoSwitchThreshold: 50 }));
    expect(input.value).toBe("50");

    await fireEvent.change(input, { target: { value: "250" } });
    await waitFor(() => expect(patches().at(-1)).toEqual({ accountAutoSwitchThreshold: 100 }));
    expect(input.value).toBe("100");

    const before = patches().length;
    await fireEvent.change(input, { target: { value: "" } });
    expect(input.value).toBe("100");
    expect(patches()).toHaveLength(before);
  });
});

describe("provider page: resolved binary", () => {
  it("names the native binary and the npm wrapper it was resolved from", async () => {
    await seed();
    setBindingMock("GetProviderStatuses", async () => [
      {
        provider: "codex",
        installed: true,
        version: "codex-cli 0.162.1",
        binaryPath: "/opt/node/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex",
        wrapperPath: "/opt/node/lib/node_modules/@openai/codex/bin/codex.js",
        status: "ready",
        message: "",
      },
    ]);
    const { findByTestId } = render(CodexSettings);

    const note = await findByTestId("settings-provider-binary-resolved");
    expect(note.textContent).toContain("/vendor/x86_64-unknown-linux-musl/bin/codex");
    expect(note.textContent).toContain("/@openai/codex/bin/codex.js");
  });

  it("says nothing extra when the binary runs as configured", async () => {
    await seed();
    setBindingMock("GetProviderStatuses", async () => [
      {
        provider: "codex",
        installed: true,
        version: "codex-cli 0.162.1",
        binaryPath: "/usr/local/bin/codex",
        status: "ready",
        message: "",
      },
    ]);
    const { findByTestId, queryByTestId } = render(CodexSettings);

    await waitFor(async () =>
      expect((await findByTestId("settings-provider-status-pill")).getAttribute("data-status")).toBe("ready"),
    );
    expect(queryByTestId("settings-provider-binary-resolved")).toBeNull();
  });
});
