import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  resetAppState,
} from './_helpers';
import { setBindingMock } from '../mocks/bindings-app';
import { isWorkflowsOverlayOpen, openWorkflowsOverlay } from '../../lib/stores/workflowsOverlay.svelte';
import { openSettingsOverlay } from '../../lib/stores/settingsOverlay.svelte';
import { openAccountSwitcher } from '../../lib/stores/accountSwitcher.svelte';
import {
  registerCommand,
  type CommandContext,
  type CommandFlags,
} from '../../lib/stores/commandRegistry.svelte';

beforeAll(installAnimateShim);

// Settings mounts as a LAYERED overlay (a sibling of PaneHost), not a surface
// that replaces the pane strip — so the pane tree stays mounted underneath and
// a close rebuilds nothing. `settings-overlay` is the card; the pane host must
// still be in the document while it is up.
const SETTINGS_TESTID = 'settings-overlay';

type Rendered = { getByTestId: (id: string) => HTMLElement };

// The lazy load's own wall-clock budget. 5s was still a TUNED value rather
// than a tripwire, and the first case in the file — the one that pays the
// on-demand transform for the whole suite run — spent 5.3s of it under a
// full-suite fan-out and failed (2026-08-31, green in isolation every time,
// and again once the run grew a few more files). Sized as the tripwire the
// area guide asks for: far above any healthy transform, still under
// LAZY_CASE_TIMEOUT_MS so this wait stays the thing that fails.
const LAZY_LOAD_BUDGET_MS = 15_000;

// A case that waits on the lazy load needs a WALL-CLOCK budget above that
// wait's own, or the inner waitFor can never be the thing that fails: at
// vitest's 5s default the two were equal, so a slow transform timed the
// CASE out and reported a line number instead of the condition that was
// not met (observed on the 2026-08-31 full run, green in isolation every
// time). This is the wedged-runtime tripwire, ~15x the idle cost of the
// whole file, not a tuned value.
const LAZY_CASE_TIMEOUT_MS = 20_000;

async function openSettingsFromSidebar(rendered: Rendered) {
  await fireEvent.click(rendered.getByTestId('sidebar-settings-button'));
  // SettingsOverlay is a lazy import; its first-ever load in a suite run pays
  // the on-demand transform, which can exceed waitFor's default 1s under full
  // suite load.
  await waitFor(() => {
    expect(rendered.getByTestId(SETTINGS_TESTID)).toBeInTheDocument();
  }, { timeout: LAZY_LOAD_BUDGET_MS });
}

describe('App integration — settings launcher', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
  });

  it('opens and closes settings from the sidebar footer button', async () => {
    const rendered = render(App);
    await flush(10);

    await openSettingsFromSidebar(rendered);
    expect(rendered.getByRole('tab', { name: 'General' })).toBeInTheDocument();
    // The whole point of the layered mount: the pane strip is never unmounted.
    expect(rendered.getByTestId('pane-host')).toBeInTheDocument();

    await fireEvent.click(rendered.getByRole('button', { name: 'Close Settings' }));

    await waitFor(() => {
      expect(rendered.queryByTestId(SETTINGS_TESTID)).not.toBeInTheDocument();
    });
    expect(rendered.getByTestId('pane-host')).toBeInTheDocument();
  }, LAZY_CASE_TIMEOUT_MS);

  it('closes settings on a scrim click', async () => {
    const rendered = render(App);
    await flush(10);

    await openSettingsFromSidebar(rendered);
    await fireEvent.click(rendered.getByTestId('settings-overlay-scrim'));

    await waitFor(() => {
      expect(rendered.queryByTestId(SETTINGS_TESTID)).not.toBeInTheDocument();
    });
  }, LAZY_CASE_TIMEOUT_MS);

  // Deep links (the context-window meter, `/config`, the account switcher's
  // empty state) call the store directly — App has no listener of its own to
  // route them through any more.
  it('opens the requested settings section from a deep link while mounted', async () => {
    const rendered = render(App);
    await flush(10);

    openSettingsOverlay('providers');

    await waitFor(() => {
      expect(rendered.getByTestId(SETTINGS_TESTID)).toBeInTheDocument();
      expect(rendered.getByRole('tab', { name: 'Providers' })).toHaveAttribute('aria-selected', 'true');
    }, { timeout: 5000 });

    openSettingsOverlay('observability');

    await waitFor(() => {
      expect(rendered.getByRole('tab', { name: 'Observability' })).toHaveAttribute('aria-selected', 'true');
    });
  });

  // Esc is keybinding-driven (`settings.close`, gated on `settingsOpen`). The
  // command is editableReachable so the chord survives a focused text field,
  // and the store's closer blurs BEFORE unmounting — settings fields commit on
  // blur, so unmounting a focused input would silently drop its edit.
  it('esc closes settings from inside a text field, blurring it first', async () => {
    setBindingMock('GetKeybindings', async () => ({
      bindings: [{ key: 'esc', command: 'settings.close', when: 'settingsOpen' }],
    }));
    const rendered = render(App);
    await flush(10);
    const keybindings = await import('../../lib/stores/keybindings.svelte');
    await keybindings.loadKeybindings();

    await openSettingsFromSidebar(rendered);

    // Any settings text input will do; the General tab's are enough to prove
    // the editable-target path.
    await fireEvent.click(rendered.getByRole('tab', { name: 'Providers' }));
    const input = await waitFor(() => {
      const found = rendered.container.querySelector<HTMLInputElement>(
        '[data-testid="settings-overlay"] input[type="text"]',
      );
      if (!found) throw new Error('expected a settings text input');
      return found;
    });

    let blurredWhileMounted: boolean | null = null;
    input.addEventListener('blur', () => {
      blurredWhileMounted = rendered.queryByTestId(SETTINGS_TESTID) !== null;
    });
    input.focus();
    expect(document.activeElement).toBe(input);

    await fireEvent.keyDown(input, { key: 'Escape' });
    await flush();

    await waitFor(() => {
      expect(rendered.queryByTestId(SETTINGS_TESTID)).not.toBeInTheDocument();
    });
    expect(blurredWhileMounted).toBe(true);
  }, LAZY_CASE_TIMEOUT_MS);

  // Two full-height layers over the pane strip means two focus traps and an
  // ambiguous Esc, so each open path closes the other surface.
  it('is mutually exclusive with the workflows overlay', async () => {
    const rendered = render(App);
    await flush(10);

    await openSettingsFromSidebar(rendered);
    openWorkflowsOverlay();
    await waitFor(() => {
      expect(rendered.queryByTestId(SETTINGS_TESTID)).not.toBeInTheDocument();
    });

    openSettingsOverlay('general');
    await waitFor(() => {
      expect(rendered.getByTestId(SETTINGS_TESTID)).toBeInTheDocument();
    }, { timeout: 5000 });
    expect(isWorkflowsOverlayOpen()).toBe(false);
  }, LAZY_CASE_TIMEOUT_MS);
});

// --- the overlay → anyModalOpen link ---
//
// App is the only place that assembles `anyModalOpen`, and the whole Esc
// vocabulary hangs off it: `settings.close` is gated on `settingsOpen` while
// `thread.interrupt` is gated on `!anyModalOpen`, so an open settings surface
// that did not ALSO read as a modal would make one Esc match both. Same for
// the account switcher, which owns its own Esc through Modal. Nothing else in
// the suite reads App's assembled context, so the probe below binds a command
// to a chord and captures the context App dispatches it with.

const PROBE_COMMAND = 'test.probeAppContext';
const PROBE_KEY = 'f9';

async function captureAppFlags(): Promise<CommandFlags> {
  const sink: { flags: CommandFlags | null } = { flags: null };
  registerCommand({
    id: PROBE_COMMAND,
    label: 'Test: Probe App Context',
    // The overlays hold focus, so the chord has to survive an editable target.
    editableReachable: true,
    run: (ctx: CommandContext) => {
      sink.flags = ctx.flags;
    },
  });
  await fireEvent.keyDown(document.body, { key: 'F9' });
  if (!sink.flags) throw new Error('probe command did not run');
  return sink.flags;
}

describe('App integration — overlay modal flags', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
    setBindingMock('GetKeybindings', async () => ({
      bindings: [{ key: PROBE_KEY, command: PROBE_COMMAND }],
    }));
  });

  it('reads an open settings overlay as both settingsOpen and anyModalOpen', async () => {
    render(App);
    await flush(10);
    const keybindings = await import('../../lib/stores/keybindings.svelte');
    await keybindings.loadKeybindings();

    expect(await captureAppFlags()).toMatchObject({
      settingsOpen: false,
      anyModalOpen: false,
    });

    openSettingsOverlay('general');
    await flush();

    expect(await captureAppFlags()).toMatchObject({
      settingsOpen: true,
      anyModalOpen: true,
    });
  });

  it('reads an open account switcher as anyModalOpen and anyPickerOpen', async () => {
    render(App);
    await flush(10);
    const keybindings = await import('../../lib/stores/keybindings.svelte');
    await keybindings.loadKeybindings();

    openAccountSwitcher();
    await flush();

    expect(await captureAppFlags()).toMatchObject({
      anyModalOpen: true,
      anyPickerOpen: true,
    });
  });
});
