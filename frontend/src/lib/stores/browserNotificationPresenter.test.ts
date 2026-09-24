// The REMOTE screen's presenter: which frames it puts on screen, which it
// refuses, and which of them make a sound.
//
// The Web Notification API does not exist in happy-dom, so the constructor is
// a recorder: every construction keeps its title, its options and whether it
// was closed. That is the whole production surface this module touches, and
// it is the only thing an assertion here can honestly be about — a real
// banner is a compositor artefact no DOM test can see.
//
// The GATE itself is not re-tested here. It is pinned case for case against
// the Go gate by the shared decision table (../notifications/gate.test.ts);
// what this suite proves is that the presenter asks it, with THIS page's
// settings and THIS page's focus, and acts on the answer.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { emitWailsEvent, resetWailsMocks } from '../../test/mocks/wailsio-runtime';
import { loadSettingsFixture } from '../../test/helpers/settingsFixture';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeSettings } from '../../test/helpers/settings';
import type { Settings } from '../types/settings';
import {
  browserNotificationPermission,
  browserNotificationsAvailable,
  pagePresentsNotificationsLocally,
  startBrowserNotificationPresenter,
  stopBrowserNotificationPresenter,
} from './browserNotificationPresenter.svelte';

const playNotificationCue = vi.fn();
vi.mock('./notificationSound', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./notificationSound')>()),
  playNotificationCue: (cue: string, event?: string) => playNotificationCue(cue, event),
}));

const reportFrontendDiagnostic = vi.fn();
vi.mock('../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: (message: string, detail?: string) => reportFrontendDiagnostic(message, detail),
}));

const applyNotificationActivated = vi.fn();
vi.mock('./eventsNotification', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./eventsNotification')>()),
  applyNotificationActivated: (target: unknown) => applyNotificationActivated(target),
}));

// The two page facts that decide whether the presenter installs at all.
const page = vi.hoisted(() => ({ loopback: false, shell: false }));
vi.mock('../transport/bootstrap', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../transport/bootstrap')>()),
  pageServedOverLoopback: () => page.loopback,
}));
vi.mock('../native/platform', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../native/platform')>()),
  isNativeShell: () => page.shell,
}));

// This screen's own focus and panes, the facts the attended-screen half of
// the gate reads. Mocked at the presence module rather than by driving a pane
// registry: the composition rule is screenPresence.test.ts's subject.
const screen = vi.hoisted(() => ({ focused: false, threads: [] as string[] }));
vi.mock('./screenPresence', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./screenPresence')>()),
  currentScreenPresence: () => ({ focused: screen.focused, threads: [...screen.threads] }),
}));

interface Raised {
  title: string;
  options: NotificationOptions;
  closed: boolean;
  onclick: (() => void) | null;
}

let raised: Raised[];
let permission: NotificationPermission;
let constructorThrows: Error | null;

/**
 * Install the recorder in place of the engine's own `Notification`. The
 * presenter reads `Notification.permission` and calls the constructor, and
 * nothing else — so this is the whole capability, not a partial stand-in.
 */
function installNotificationRecorder(): void {
  class RecordingNotification {
    static get permission(): NotificationPermission {
      return permission;
    }

    static requestPermission(): Promise<NotificationPermission> {
      return Promise.resolve(permission);
    }

    onclick: (() => void) | null = null;
    onclose: (() => void) | null = null;
    private readonly entry: Raised;

    constructor(title: string, options: NotificationOptions = {}) {
      if (constructorThrows) throw constructorThrows;
      this.entry = { title, options, closed: false, onclick: null };
      // The handle's click binding is assigned after construction, so the
      // recorder reads it back off `this` when a test fires one.
      Object.defineProperty(this.entry, 'onclick', { get: () => this.onclick });
      raised.push(this.entry);
    }

    close(): void {
      this.entry.closed = true;
      this.onclose?.();
    }
  }
  vi.stubGlobal('Notification', RecordingNotification);
}

async function seedSettings(overrides: Partial<Settings> = {}): Promise<void> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  await loadSettingsFixture();
}

/**
 * Start the presenter for one case.
 *
 * Statically imported rather than re-imported per test: `vi.resetModules()`
 * would hand the presenter a second copy of the runtime mock, and the
 * emitter this file drives would then be talking to a registry nothing
 * subscribed to. Its module state is the subscription and the raised-handle
 * map, and `stop()` clears both, which is what afterEach leans on.
 */
function startPresenter(): () => void {
  return startBrowserNotificationPresenter();
}

function send(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'thread:t1',
    kind: 'turn-complete',
    title: 'Rewrite the parser',
    body: 'Completed',
    target: { kind: 'thread', threadId: 't1' },
    ...overrides,
  };
}

function emit(payload: Record<string, unknown>, replayed = false): void {
  emitWailsEvent('notification:send', payload, undefined, { replayed });
}

describe('the remote browser notification presenter', () => {
  beforeEach(async () => {
    raised = [];
    permission = 'granted';
    constructorThrows = null;
    page.loopback = false;
    page.shell = false;
    screen.focused = false;
    screen.threads = [];
    playNotificationCue.mockClear();
    applyNotificationActivated.mockClear();
    installNotificationRecorder();
    // "Never" so the attended-screen half is out of the way unless a case is
    // about it; the default reading would suppress on a focused page.
    await seedSettings({ notifyQuietWhen: 'never' });
  });

  afterEach(() => {
    // Unconditional, not only on the happy path: a case that failed mid-way
    // would otherwise leave a live subscription for the next one.
    stopBrowserNotificationPresenter();
    vi.unstubAllGlobals();
    resetWailsMocks();
  });

  it('raises a silent notification and plays the cue for a live send', async () => {
    const stop = startPresenter();
    emit(send());

    expect(raised).toHaveLength(1);
    expect(raised[0].title).toBe('Rewrite the parser');
    expect(raised[0].options.body).toBe('Completed');
    // EXACTLY ONE SOUND: the page is playing the built-in cue, so the banner
    // must not add the platform's own on top of it.
    expect(raised[0].options.silent).toBe(true);
    expect(playNotificationCue).toHaveBeenCalledWith('swoosh', 'turn-complete');
    stop();
  });

  it('lets the banner carry the platform sound when the cue is the system sound', async () => {
    await seedSettings({ notifyQuietWhen: 'never', notifySoundCueTurnComplete: 'system' });
    const stop = startPresenter();
    emit(send());

    expect(raised[0].options.silent).toBe(false);
    // The system sound IS the banner's; a cue frame for it would be a second.
    expect(playNotificationCue).not.toHaveBeenCalled();
    stop();
  });

  it('plays nothing extra but still raises when this screen muted sounds', async () => {
    await seedSettings({ notifyQuietWhen: 'never', notificationSoundsEnabled: false });
    const stop = startPresenter();
    emit(send());

    expect(raised).toHaveLength(1);
    expect(raised[0].options.silent).toBe(true);
    expect(playNotificationCue).not.toHaveBeenCalled();
    stop();
  });

  it('refuses a kind this screen turned off', async () => {
    await seedSettings({ notifyQuietWhen: 'never', notifyTurnComplete: false });
    const stop = startPresenter();
    emit(send());

    expect(raised).toEqual([]);
    expect(playNotificationCue).not.toHaveBeenCalled();
    stop();
  });

  it('refuses a hidden thread until this screen opts in', async () => {
    await seedSettings({ notifyQuietWhen: 'never', notifyHiddenThreads: false });
    const stop = startPresenter();
    emit(send({ hiddenThread: true }));
    expect(raised).toEqual([]);
    stop();

    await seedSettings({ notifyQuietWhen: 'never', notifyHiddenThreads: true });
    const second = startPresenter();
    emit(send({ hiddenThread: true }));
    expect(raised).toHaveLength(1);
    second();
  });

  it('refuses a send about a thread this screen is already looking at', async () => {
    await seedSettings({ notifyQuietWhen: 'focusedAndThreadVisible' });
    screen.focused = true;
    screen.threads = ['t1'];
    const stop = startPresenter();
    emit(send());
    expect(raised).toEqual([]);

    // The same reading raises a send about a DIFFERENT thread: the facts are
    // this page's, and the gate reads both of them.
    emit(send({ id: 'thread:t2', target: { kind: 'thread', threadId: 't2' } }));
    expect(raised).toHaveLength(1);
    stop();
  });

  it('plays the cue but raises nothing when the browser permission is missing', async () => {
    permission = 'denied';
    const stop = startPresenter();
    emit(send());

    expect(raised).toEqual([]);
    // The cue is the channel that survives a denied permission.
    expect(playNotificationCue).toHaveBeenCalledWith('swoosh', 'turn-complete');
    stop();
  });

  it('plays the cue but raises nothing when the engine has no Notification', async () => {
    vi.stubGlobal('Notification', undefined);
    const stop = startPresenter();
    emit(send());

    expect(playNotificationCue).toHaveBeenCalledWith('swoosh', 'turn-complete');
    stop();
  });

  it('survives a constructor that throws, and still plays the cue', async () => {
    constructorThrows = new Error('no service worker');
    const stop = startPresenter();
    emit(send());

    expect(raised).toEqual([]);
    // A diagnostic, not a console line: production builds show no console.
    expect(reportFrontendDiagnostic).toHaveBeenCalledWith(
      'browser notification could not be raised',
      'no service worker',
    );
    expect(playNotificationCue).toHaveBeenCalledWith('swoosh', 'turn-complete');
    stop();
  });

  it('raises a replayed banner but makes no sound for it', async () => {
    const stop = startPresenter();
    emit(send(), true);

    // A banner re-raised after a reconnect replaces itself by tag and still
    // describes something true; a cue names a moment that has passed.
    expect(raised).toHaveLength(1);
    expect(playNotificationCue).not.toHaveBeenCalled();
    stop();
  });

  it('closes the matching notification on a retraction, gated by nothing', async () => {
    const stop = startPresenter();
    emit(send());
    expect(raised[0].closed).toBe(false);

    // Every preference flipped off between the send and the withdrawal: a
    // retraction must not be strandable by a toggle or by replay.
    await seedSettings({ notificationsEnabled: false });
    emit({ id: 'thread:t1', kind: 'turn-complete', retract: true }, true);
    expect(raised[0].closed).toBe(true);

    // A second retraction for a tag nothing holds is a no-op everywhere.
    emit({ id: 'thread:t1', kind: 'turn-complete', retract: true });
    expect(raised).toHaveLength(1);
    stop();
  });

  it('leaves another backend’s notification alone when one is retracted', async () => {
    const stop = startPresenter();
    emitWailsEvent('notification:send', send(), 'backend-a');
    emitWailsEvent('notification:send', send(), 'backend-b');
    expect(raised).toHaveLength(2);

    emitWailsEvent('notification:send', { id: 'thread:t1', kind: 'turn-complete', retract: true }, 'backend-a');
    expect(raised[0].closed).toBe(true);
    expect(raised[1].closed).toBe(false);
    stop();
  });

  it('routes a click through the same activation path the host click uses', async () => {
    const stop = startPresenter();
    emit(send());
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => {});

    raised[0].onclick?.();
    expect(focus).toHaveBeenCalled();
    expect(applyNotificationActivated).toHaveBeenCalledWith({ kind: 'thread', threadId: 't1' });
    // The banner is taken down by the click, so a later retraction has
    // nothing stale to withdraw.
    expect(raised[0].closed).toBe(true);
    focus.mockRestore();
    stop();
  });

  it('installs nothing on a loopback page, where the host already presents', async () => {
    page.loopback = true;
    const stop = startPresenter();
    emit(send());

    expect(raised).toEqual([]);
    expect(playNotificationCue).not.toHaveBeenCalled();
    stop();
  });

  it('installs nothing in the native shell, which has its own presenter', async () => {
    page.shell = true;
    const stop = startPresenter();
    emit(send());

    expect(raised).toEqual([]);
    stop();
  });

  it('closes every open notification when the presenter stops', async () => {
    const stop = startPresenter();
    emit(send());
    emit(send({ id: 'thread:t2', target: { kind: 'thread', threadId: 't2' } }));
    expect(raised.every((entry) => entry.closed)).toBe(false);

    stop();
    expect(raised.every((entry) => entry.closed)).toBe(true);
    // And the subscription is gone: a later frame reaches nothing.
    emit(send({ id: 'thread:t3', target: { kind: 'thread', threadId: 't3' } }));
    expect(raised).toHaveLength(2);
  });

  it('ignores a frame with no id, which no presenter could dedupe or withdraw', async () => {
    const stop = startPresenter();
    emit(send({ id: '' }));
    expect(raised).toEqual([]);
    stop();
  });

  it('reports a page the settings section must offer a permission ask on', async () => {
    const stop = startPresenter();
    expect(pagePresentsNotificationsLocally()).toBe(true);
    expect(browserNotificationPermission()).toBe('granted');

    vi.stubGlobal('Notification', undefined);
    // A capability that does not exist is a third state, not a denial.
    expect(browserNotificationsAvailable()).toBe(false);
    expect(browserNotificationPermission()).toBe('unavailable');
    stop();
  });
});
