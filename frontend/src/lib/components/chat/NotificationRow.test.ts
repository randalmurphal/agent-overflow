import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import NotificationRow from './NotificationRow.svelte';
import type { Item } from '../../types/models';

function makeNotification(meta: Record<string, unknown>, summary: string): Item {
  return {
    id: 'permission-denied:toolu_01',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'notification',
    role: 'system',
    status: 'completed',
    summary,
    meta: JSON.stringify(meta),
    createdAt: 0,
    updatedAt: 0,
  } as Item;
}

describe('<NotificationRow> permission notices', () => {
  it('renders the denial sentence and the deciding component reason', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          {
            kind: 'permission_denied',
            toolName: 'Bash',
            toolUseId: 'toolu_01',
            decisionReasonType: 'rule',
            decisionReason: 'Denied by alwaysDenyRules: Bash(rm:*)',
          },
          'Bash was denied by a permission rule',
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain('Bash was denied by a permission rule');
    expect(getByTestId('permission-denied-reason').textContent).toContain(
      'Denied by alwaysDenyRules: Bash(rm:*)',
    );
    // A denial is user-facing state, so it takes the warning treatment and
    // announces itself.
    expect(row.getAttribute('role')).toBe('status');
  });

  it('shows the addDirectories remedy for a workspace-boundary denial', () => {
    const boundary = render(NotificationRow, {
      props: {
        item: makeNotification(
          {
            kind: 'permission_denied',
            toolName: 'Read',
            decisionReasonType: 'workingDir',
            decisionReason: 'Path is outside allowed working directories',
            workspaceBoundary: true,
          },
          "Read was denied — the path is outside this workspace's allowed directories",
        ),
      },
    });
    expect(boundary.getByTestId('permission-denied-remedy').textContent).toContain(
      'not a tool permission rule',
    );
  });

  it('omits the remedy for a rule denial — the wrong remedy fixes nothing', () => {
    const rule = render(NotificationRow, {
      props: {
        item: makeNotification(
          {
            kind: 'permission_denied',
            toolName: 'Bash',
            decisionReasonType: 'rule',
            decisionReason: 'Denied by alwaysDenyRules',
          },
          'Bash was denied by a permission rule',
        ),
      },
    });
    expect(
      rule.container.querySelector('[data-testid="permission-denied-remedy"]'),
    ).toBeNull();
  });

  it('renders a denial with no reason without an empty Reason line', () => {
    const { queryByTestId, getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'permission_denied', toolName: 'Bash' },
          'Bash was denied by the permission system',
        ),
      },
    });
    expect(getByTestId('notification-row').textContent).toContain(
      'Bash was denied by the permission system',
    );
    expect(queryByTestId('permission-denied-reason')).toBeNull();
  });

  // A queue write from outside Agent Overflow (`codex queue --thread ...`)
  // is the one notification that reports someone ELSE acting on this thread.
  // Informational, not a warning — nothing is wrong — but it must not share
  // the generic bell, or the row that says "a stranger queued work here"
  // looks exactly like every other provider notice.
  it('gives an external_queue notice its own glyph and keeps the informational tone', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'external_queue', origin: 'external-queue' },
          'A message was queued for this thread from outside Agent Overflow.',
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain('queued for this thread from outside');
    expect(row.getAttribute('role')).toBeNull();
    expect(row.className).not.toContain('text-warning');
    // Distinct from the default bell: lucide stamps the icon name on the svg.
    expect(row.querySelector('.lucide-icon')?.getAttribute('class')).toContain('inbox');
  });

  it.each([
    // A downgraded Codex leaves messages parked in the app-server's own queue
    // with no API to read, purge or run them. Nothing is lost, but nothing
    // happens either until the user upgrades — so unlike external_queue this
    // one carries the warning tone, or a standing request for action reads as
    // chatter.
    {
      kind: 'codex_queue_unsupported',
      summary: "2 queued messages were handed to Codex 0.148+ and this Codex version can't see them. They run when Codex is upgraded.",
    },
    // The other half of the same family: Codex has the queue API, but this
    // session could not read it, so a message AO handed over can be neither
    // confirmed nor handed back. Same warning tone for the same reason — the
    // user has a message in limbo and reopening the thread is what resolves it.
    {
      kind: 'codex_queue_unreconciled',
      summary: "Codex couldn't be asked about its message queue when this thread reopened, so 1 queued message can't be confirmed. It runs if Codex still has it; reopen the thread later to check.",
    },
    {
      kind: 'transcript_mirror_degraded',
      summary: "Some early agent activity could not be shown because Claude's transcript mirror exceeded Agent Overflow's safety bound.",
    },
  ])('gives a $kind notice the warning tone', ({ kind, summary }) => {
    const { getByTestId } = render(NotificationRow, { props: { item: makeNotification({ kind }, summary) } });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain(summary);
    expect(row.className).toContain('text-warning');
    expect(row.getAttribute('role')).toBe('status');
    expect(row.querySelector('.lucide-icon')?.getAttribute('class')).toContain('triangle');
  });

  // The merged-PR worktree cleanup reports what the app did on its own in
  // the neutral tone, and why it kept a worktree in the warning tone
  // (internal/app/app_worktree_merge_cleanup.go). Text only: no action.
  it('gives a worktree_cleanup notice the neutral tone and a kept worktree the warning tone', () => {
    const removed = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'worktree_cleanup' },
          'PR #90 merged. Removed worktree fix-pr-diff-fetch; this thread now runs in Base.',
        ),
      },
    });
    const row = removed.getByTestId('notification-row');
    expect(row.textContent).toContain('Removed worktree fix-pr-diff-fetch');
    expect(row.className).toContain('text-fg-subtle');
    expect(row.className).not.toContain('text-warning');
    expect(row.getAttribute('role')).toBeNull();
    expect(row.querySelector('a, button')).toBeNull();
    removed.unmount();

    const kept = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'warning' },
          'PR #90 merged. Kept worktree fix-pr-diff-fetch: 3 uncommitted files.',
        ),
      },
    });
    const keptRow = kept.getByTestId('notification-row');
    expect(keptRow.className).toContain('text-warning');
    expect(keptRow.querySelector('a, button')).toBeNull();
  });

  it('lists the commands a permission_retry re-allowed, without the warning tone', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'permission_retry', commands: ['git status', 'ls'] },
          'Allowed git status, ls',
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain('Allowed git status, ls');
    // Informational: a retry is progress, not a refusal.
    expect(row.getAttribute('role')).toBeNull();
    const commands = getByTestId('permission-retry-commands');
    expect(commands.textContent).toContain('git status');
    expect(commands.textContent).toContain('ls');
  });
});

describe('<NotificationRow> background wake', () => {
  it('names the task, shows what woke the session and exposes its kind', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'background_wake', detail: 'ERROR deploy step 3 failed', taskId: 'bwatch002' },
          'Monitor event: "deploy log errors"',
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.getAttribute('data-notification-kind')).toBe('background_wake');
    expect(row.textContent).toContain('Monitor event: "deploy log errors"');
    expect(getByTestId('background-wake-detail').textContent).toContain('ERROR deploy step 3 failed');
    // Not a warning: nothing failed in the app.
    expect(row.getAttribute('role')).toBeNull();
  });

  it('renders no detail block when the report carried none', () => {
    const { queryByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'background_wake' },
          'Background command "Run the release build" completed (exit code 0)',
        ),
      },
    });
    expect(queryByTestId('background-wake-detail')).toBeNull();
  });
});

describe('<NotificationRow> tracker write', () => {
  it('lists each ticket the call wrote with its key to open it and the steps applied', () => {
    const { getAllByTestId, getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          {
            kind: 'tracker_write',
            tickets: [
              { connection: 'conn-1', issue: '10012', key: 'WEB-12', title: 'Login fails on Safari', applied: ['status', 'comment'] },
              { connection: 'conn-1', issue: '10018', key: 'WEB-18', title: 'Session cookie lost', applied: ['comment'] },
            ],
          },
          'Changed 2 tickets',
        ),
      },
    });
    expect(getByTestId('notification-row').textContent).toContain('Changed 2 tickets');
    const rows = getAllByTestId('tracker-write-ticket');
    expect(rows.map((r) => r.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'WEB-12 Login fails on Safari · status, comment',
      'WEB-18 Session cookie lost · comment',
    ]);
    expect(rows[0]!.querySelector('button')?.getAttribute('title')).toBe('Open WEB-12');
  });
});

describe('<NotificationRow> time draft', () => {
  it('mounts the call\'s card under the row, skipped by in-thread find', async () => {
    const { findByTestId, getByTestId } = render(NotificationRow, {
      props: { item: makeNotification({ kind: 'time_draft', call: 'call-1', connection: 'conn-1', days: ['2026-11-11'] }, 'Drafted time for Wed Nov 11') },
    });
    expect(getByTestId('notification-row').textContent).toContain('Drafted time for Wed Nov 11');
    const card = await findByTestId('time-draft-card');
    expect(card.dataset.call).toBe('call-1');
    expect(card.hasAttribute('data-find-skip')).toBe(true);
  });

  it('mounts no card for a row that names no call', () => {
    const { queryByTestId } = render(NotificationRow, {
      props: { item: makeNotification({ kind: 'time_draft', connection: 'conn-1', days: [] }, 'Drafted time') },
    });
    expect(queryByTestId('time-draft-card-loading')).toBeNull();
  });
});
