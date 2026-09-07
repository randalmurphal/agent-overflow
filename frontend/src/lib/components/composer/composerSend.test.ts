import { buildSendOptions } from '../../utils/sendOptions';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { dispatchSend } from './composerSend';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { SourceProposedPlan, Thread } from '../../types/models';
import { resetForTest as resetWorktreeIntent } from '../../stores/worktreeIntent.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { makeSettings } from '../../../test/helpers/settings';
import { DisconnectedError } from '../../transport/wsClient';
import {
  hasPendingUnsentMessageConfirmation,
  resetUnsentMessageConfirmationForTest,
  resolveUnsentMessageConfirmation,
} from '../../stores/unsentMessageConfirmation.svelte';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Thread',
    provider: 'claude',
    workspacePath: '/tmp/workspace',
    projectPath: '/tmp/workspace',
    model: 'claude-sonnet-4-6',
    createdAt: 1,
    updatedAt: 1,
    archived: false,
    ...overrides,
  };
}

function sendOptions(threadId = 'thread-1') {
  return {
    threadId,
    message: 'Start work',
    options: buildSendOptions({ attachmentIds: [] }),
    snapshot: { content: 'Start work', attachments: [], terminalChips: [] },
    restoreDraft: vi.fn(),
    draftThreadId: () => threadId,
    reportError: vi.fn(),
  };
}

describe('dispatchSend', () => {
  beforeEach(async () => {
    resetBindingMocks();
    resetWorktreeIntent();
    replaceAllThreads([]);
    setBindingMock('GetSettings', async () => makeSettings());
    await loadSettings();
  });
  afterEach(() => {
    resetUnsentMessageConfirmationForTest();
    vi.restoreAllMocks();
  });

  it('passes revision source plan metadata when refining a proposed plan', async () => {
    const thread = { id: 'thread-1' } as Thread;
    const source: SourceProposedPlan = {
      threadId: 'thread-1',
      itemId: 'plan-1',
      payloadId: 'payload-1',
      title: 'Plan',
    };
    setBindingMock('SendMessageWithOptions', async () => thread);

    await dispatchSend({
      threadId: 'thread-1',
      message: 'Tighten the migration step.',
      options: buildSendOptions({ attachmentIds: [], revisionSourceProposedPlan: source }),
      snapshot: { content: 'Tighten the migration step.', attachments: [], terminalChips: [] },
      restoreDraft: vi.fn(),
      draftThreadId: () => 'thread-1',
      reportError: vi.fn(),
    });

    expect(getBindingMock('SendMessageWithOptions')).toHaveBeenCalledWith(
      'thread-1',
      'Tighten the migration step.',
      expect.objectContaining({
        revisionSourceProposedPlan: source,
      }),
    );
  });

  it('restores the draft and reports a normalized error when the send is refused', async () => {
    // The staged branch/worktree intent is applied by the composer BEFORE it
    // materializes and dispatches (Composer.send), so this layer only owns
    // the send itself and its rollback.
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const send = setBindingMock('SendMessageWithOptions', async () => {
      throw new Error('send message: create branch: branch "BLITZ-187" already exists');
    });
    const restoreDraft = vi.fn(async () => {});
    const reportError = vi.fn();

    const sent = await dispatchSend({
      threadId: 'thread-1',
      message: 'continue',
      options: buildSendOptions({ attachmentIds: [] }),
      snapshot: { content: 'continue', attachments: [], terminalChips: [] },
      restoreDraft,
      draftThreadId: () => 'thread-1',
      reportError,
    });

    expect(sent).toBe(false);
    expect(send).toHaveBeenCalled();
    expect(restoreDraft).toHaveBeenCalledWith('thread-1', {
      content: 'continue',
      attachments: [],
      terminalChips: [],
    });
    expect(reportError).toHaveBeenCalledWith(
      'Failed to send message: Branch "BLITZ-187" already exists.',
    );
    expect(consoleErr).toHaveBeenCalled();
  });

  // A socket that died AFTER the frame reached the backend looks exactly like
  // one that died before it, and the transport has already spent its one
  // retry by the time this runs. The message may be with the agent, so
  // putting the text back silently is a guess whose wrong answer sends it
  // twice. The three tests below are the three branches of that decision.
  describe('a send whose connection died', () => {
    function undeliverable() {
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      const send = setBindingMock('SendMessageWithOptions', async () => {
        throw new DisconnectedError('connection lost (close 1006)');
      });
      const restoreDraft = vi.fn(async () => {});
      const reportError = vi.fn();
      const pending = dispatchSend({
        threadId: 'thread-1',
        message: 'continue',
        options: buildSendOptions({ attachmentIds: [] }),
        snapshot: { content: 'continue', attachments: [], terminalChips: [] },
        restoreDraft,
        draftThreadId: () => 'thread-1',
        reportError,
      });
      return { consoleErr, send, restoreDraft, reportError, pending };
    }

    it('asks, and restores the composer when the answer is "Put it back"', async () => {
      const { restoreDraft, reportError, pending } = undeliverable();
      await vi.waitFor(() => expect(hasPendingUnsentMessageConfirmation()).toBe(true));

      resolveUnsentMessageConfirmation(true);
      expect(await pending).toBe(false);

      // From here it is the ordinary failure path, unchanged.
      expect(restoreDraft).toHaveBeenCalledWith('thread-1', {
        content: 'continue',
        attachments: [],
        terminalChips: [],
      });
      expect(reportError).toHaveBeenCalled();
    });

    it('discards the snapshot and says nothing more when the answer is "Leave it"', async () => {
      const { restoreDraft, reportError, pending } = undeliverable();
      await vi.waitFor(() => expect(hasPendingUnsentMessageConfirmation()).toBe(true));

      resolveUnsentMessageConfirmation(false);
      expect(await pending).toBe(false);

      expect(restoreDraft).not.toHaveBeenCalled();
      // Nothing further is reported: the person was shown the ambiguity and
      // decided it, and restating the failure would contradict their answer.
      expect(reportError).not.toHaveBeenCalled();
    });

    it('does not ask when the failure is definite, and restores as it always did', async () => {
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      // A backend that answered with an error is a definite "nothing
      // happened" — as is a TERMINAL disconnect, where the client has
      // stopped trying and no frame is in the air.
      setBindingMock('SendMessageWithOptions', async () => {
        throw new DisconnectedError('client closed', { terminal: true });
      });
      const restoreDraft = vi.fn(async () => {});
      const reportError = vi.fn();

      const sent = await dispatchSend({
        threadId: 'thread-1',
        message: 'continue',
        options: buildSendOptions({ attachmentIds: [] }),
        snapshot: { content: 'continue', attachments: [], terminalChips: [] },
        restoreDraft,
        draftThreadId: () => 'thread-1',
        reportError,
      });

      expect(sent).toBe(false);
      expect(hasPendingUnsentMessageConfirmation()).toBe(false);
      expect(restoreDraft).toHaveBeenCalled();
      expect(reportError).toHaveBeenCalled();
      expect(consoleErr).toHaveBeenCalled();
    });
  });

  it('auto-pins an eligible in-app draft only after its first send succeeds', async () => {
    const draft = makeThread({ isDraft: true });
    const started = makeThread({ isDraft: false, updatedAt: 2 });
    replaceAllThreads([draft]);
    setBindingMock('SendMessageWithOptions', async () => started);
    const pin = setBindingMock('PinThread', vi.fn(async () => ({
      ...started,
      pinnedAt: 10,
      pinGroup: 0,
    })));

    expect(await dispatchSend(sendOptions())).toBe(true);
    expect(pin).toHaveBeenCalledWith('thread-1');
  });

  it('does not auto-pin when the setting is disabled', async () => {
    setBindingMock('GetSettings', async () => makeSettings({ autoPinNewThreads: false }));
    await loadSettings();
    const draft = makeThread({ isDraft: true });
    replaceAllThreads([draft]);
    setBindingMock('SendMessageWithOptions', async () => makeThread({ isDraft: false }));
    const pin = setBindingMock('PinThread', vi.fn());

    expect(await dispatchSend(sendOptions())).toBe(true);
    expect(pin).not.toHaveBeenCalled();
  });

  it('does not auto-pin imported drafts', async () => {
    const draft = makeThread({ isDraft: true, importSource: '/tmp/provider-session' });
    replaceAllThreads([draft]);
    setBindingMock('SendMessageWithOptions', async () => makeThread({
      isDraft: false,
      importSource: draft.importSource,
    }));
    const pin = setBindingMock('PinThread', vi.fn());

    expect(await dispatchSend(sendOptions())).toBe(true);
    expect(pin).not.toHaveBeenCalled();
  });

  it('does not auto-pin provider-spawned child drafts', async () => {
    const draft = makeThread({ isDraft: true, parentThreadId: 'parent-thread' });
    replaceAllThreads([draft]);
    setBindingMock('SendMessageWithOptions', async () => makeThread({
      isDraft: false,
      parentThreadId: draft.parentThreadId,
    }));
    const pin = setBindingMock('PinThread', vi.fn());

    expect(await dispatchSend(sendOptions())).toBe(true);
    expect(pin).not.toHaveBeenCalled();
  });

  it('does not auto-pin a later send on an already-started thread', async () => {
    const started = makeThread({ isDraft: false, sessionRef: 'session-1' });
    replaceAllThreads([started]);
    setBindingMock('SendMessageWithOptions', async () => ({ ...started, updatedAt: 2 }));
    const pin = setBindingMock('PinThread', vi.fn());

    expect(await dispatchSend(sendOptions())).toBe(true);
    expect(pin).not.toHaveBeenCalled();
  });
});
