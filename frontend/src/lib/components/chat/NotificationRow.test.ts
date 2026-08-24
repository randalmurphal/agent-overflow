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
    expect(row.querySelector('svg')?.getAttribute('class')).toContain('inbox');
  });

  // A downgraded Codex leaves messages parked in the app-server's own queue
  // with no API to read, purge or run them. Nothing is lost, but nothing
  // happens either until the user upgrades — so unlike external_queue this one
  // carries the warning tone, or a standing request for action reads as
  // chatter.
  it('gives a codex_queue_unsupported notice the warning tone', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'codex_queue_unsupported' },
          "2 queued messages were handed to Codex 0.148+ and this Codex version can't see them. They run when Codex is upgraded.",
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain('They run when Codex is upgraded.');
    expect(row.className).toContain('text-warning');
    expect(row.getAttribute('role')).toBe('status');
    expect(row.querySelector('svg')?.getAttribute('class')).toContain('triangle');
  });

  // The other half of the same family: Codex has the queue API, but this
  // session could not read it, so a message AO handed over can be neither
  // confirmed nor handed back. Same warning tone for the same reason — the
  // user has a message in limbo and reopening the thread is what resolves it.
  it('gives a codex_queue_unreconciled notice the warning tone', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'codex_queue_unreconciled' },
          "Codex couldn't be asked about its message queue when this thread reopened, so 1 queued message can't be confirmed. It runs if Codex still has it; reopen the thread later to check.",
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain('reopen the thread later to check');
    expect(row.className).toContain('text-warning');
    expect(row.querySelector('svg')?.getAttribute('class')).toContain('triangle');
  });

  it('surfaces transcript mirror data loss as a warning', () => {
    const { getByTestId } = render(NotificationRow, {
      props: {
        item: makeNotification(
          { kind: 'transcript_mirror_degraded' },
          "Some early agent activity could not be shown because Claude's transcript mirror exceeded Agent Overflow's safety bound.",
        ),
      },
    });
    const row = getByTestId('notification-row');
    expect(row.className).toContain('text-warning');
    expect(row.getAttribute('role')).toBe('status');
    expect(row.querySelector('svg')?.getAttribute('class')).toContain('triangle');
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
