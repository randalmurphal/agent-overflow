import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { resolveToolPresentation } from './toolPresentation';

describe('resolveToolPresentation', () => {
  it('routes Claude Bash rows to command presentation', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: ls -la',
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'claude',
      surface: 'timeline',
    });

    expect(presentation.kind).toBe('command');
    if (presentation.kind !== 'command') return;
    expect(presentation.meta.command).toBe('ls -la');
    expect(presentation.item).toBe(item);
  });

  it('routes Codex command_execution rows to command presentation', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'command_execution',
      summary: "Bash: /usr/bin/zsh -lc 'git status --short'",
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'codex',
      surface: 'timeline',
    });

    expect(presentation.kind).toBe('command');
    if (presentation.kind !== 'command') return;
    expect(presentation.meta.command).toBe("/usr/bin/zsh -lc 'git status --short'");
  });

  it('routes user-input tools before payload-specific branches', () => {
    const item = makeItem({
      toolName: 'request_user_input',
      payloadId: 'payload-1',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        itemType: 'tool_result',
        title: 'Answers',
        inlineDiff: {
          availability: 'exact_patch',
          files: [{ path: 'a.ts' }],
        },
      }),
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'codex',
      surface: 'timeline',
    });

    expect(presentation.kind).toBe('user-input');
  });

  it('routes Codex collab tools only for Codex threads', () => {
    const item = makeItem({
      toolName: 'wait_agent',
    });

    expect(resolveToolPresentation({ item, provider: 'codex' }).kind).toBe('collab');
    expect(resolveToolPresentation({ item, provider: 'claude' }).kind).toBe('generic');
  });

  it('trims Codex collab tool names on tray rows', () => {
    const item = makeItem({
      toolName: ' collab_agent ',
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'codex',
      surface: 'tray',
    });

    expect(presentation.kind).toBe('collab');
  });

  it('routes Claude Agent and Task rows to agent presentation', () => {
    for (const toolName of ['Agent', 'Task']) {
      const item = makeItem({
        kind: 'tool_call',
        status: 'running',
        toolName,
      });

      const presentation = resolveToolPresentation({ item, provider: 'claude' });

      expect(presentation.kind).toBe('agent');
      if (presentation.kind !== 'agent') return;
      expect(presentation.item).toBe(item);
    }
  });

  it('keeps non-Codex collab_agent on the generic presentation path', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'collab_agent',
    });

    expect(resolveToolPresentation({ item, provider: 'claude' }).kind).toBe('generic');
  });

  it('routes tray Claude Agent rows with launch and completion context', () => {
    const launch = makeItem({
      id: 'agent-launch',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Agent',
      summary: 'Agent: explore',
    });
    const completion = makeItem({
      id: 'agent-complete',
      kind: 'tool_completion',
      status: 'completed',
      completionOf: launch.id,
      toolName: 'Agent',
      payloadId: 'agent-payload',
    });

    const presentation = resolveToolPresentation({
      item: completion,
      provider: 'claude',
      surface: 'tray',
      displayItem: launch,
      statusItem: completion,
      outputItem: completion,
    });

    expect(presentation.kind).toBe('agent');
    if (presentation.kind !== 'agent') return;
    expect(presentation.item).toBe(completion);
    expect(presentation.displayItem).toBe(launch);
    expect(presentation.statusItem).toBe(completion);
  });

  it('routes proposed plan payloads', () => {
    const item = makeItem({
      payloadId: 'payload-plan',
      payloadKind: 'proposed_plan',
      payloadMeta: JSON.stringify({
        title: 'Plan',
        lineCount: 3,
        charCount: 42,
        preview: '1. Do the thing',
      }),
    });

    const presentation = resolveToolPresentation({ item, provider: 'claude' });

    expect(presentation.kind).toBe('proposed-plan');
    if (presentation.kind !== 'proposed-plan') return;
    expect(presentation.payloadId).toBe('payload-plan');
    expect(presentation.meta.title).toBe('Plan');
  });

  it('routes single-file diff payloads and parses the inline patch preview', () => {
    const item = makeItem({
      payloadId: 'payload-diff',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'src/app.ts',
        changeKind: 'modified',
        insertions: 1,
        deletions: 1,
        preview: [
          'diff --git a/src/app.ts b/src/app.ts',
          '--- a/src/app.ts',
          '+++ b/src/app.ts',
          '@@ -1 +1 @@',
          '-old',
          '+new',
        ].join('\n'),
      }),
    });

    const presentation = resolveToolPresentation({ item, provider: 'claude' });

    expect(presentation.kind).toBe('single-file-diff');
    if (presentation.kind !== 'single-file-diff') return;
    expect(presentation.file.path).toBe('src/app.ts');
    expect(presentation.file.lines.length).toBeGreaterThan(0);
  });

  it('falls back to diff metadata when preview text is not a parseable patch', () => {
    const item = makeItem({
      payloadId: 'payload-diff',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'src/app.ts',
        changeKind: 'modified',
        insertions: 2,
        deletions: 0,
        preview: 'summary only',
      }),
    });

    const presentation = resolveToolPresentation({ item, provider: 'claude' });

    expect(presentation.kind).toBe('single-file-diff');
    if (presentation.kind !== 'single-file-diff') return;
    expect(presentation.file).toMatchObject({
      path: 'src/app.ts',
      kind: 'modified',
      additions: 2,
      deletions: 0,
      lines: [],
    });
  });

  it('routes tool_result inline diffs to the diff stack', () => {
    const item = makeItem({
      payloadId: 'payload-tool-result',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        itemType: 'tool_result',
        title: 'Edited files',
        inlineDiff: {
          availability: 'exact_patch',
          files: [{ path: 'src/app.ts' }],
        },
      }),
    });

    const presentation = resolveToolPresentation({ item, provider: 'codex' });

    expect(presentation.kind).toBe('diff-stack');
    if (presentation.kind !== 'diff-stack') return;
    expect(presentation.meta.inlineDiff?.files[0]?.path).toBe('src/app.ts');
  });

  it('keeps completed structured file edits on the diff stack when inline diff metadata exists', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Edit',
      payloadId: 'payload-tool-result',
      payloadKind: 'tool_result',
      meta: JSON.stringify({
        toolName: 'Edit',
        input: {
          file_path: '/home/me/repo/src/app.ts',
        },
      }),
      payloadMeta: JSON.stringify({
        itemType: 'tool_result',
        title: 'Edited files',
        inlineDiff: {
          availability: 'exact_patch',
          files: [{ path: 'src/app.ts' }],
        },
      }),
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'claude',
      workspacePath: '/home/me/repo',
    });

    expect(presentation.kind).toBe('diff-stack');
    if (presentation.kind !== 'diff-stack') return;
    expect(presentation.meta.inlineDiff?.files[0]?.path).toBe('src/app.ts');
  });

  it('routes non-diff tool_result payloads to the tool result card', () => {
    const item = makeItem({
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        itemType: 'tool_result',
        title: 'Search results',
        preview: 'No matches',
      }),
    });

    const presentation = resolveToolPresentation({ item, provider: 'claude' });

    expect(presentation.kind).toBe('tool-result');
    if (presentation.kind !== 'tool-result') return;
    expect(presentation.meta.preview).toBe('No matches');
  });

  it('routes structured pending file edits to a diff-row placeholder with a full relative path', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'Edit',
      summary: 'Edit: Composer.svelte',
      meta: JSON.stringify({
        toolName: 'Edit',
        input: {
          file_path: '/home/me/repo/frontend/src/lib/components/composer/Composer.svelte',
        },
      }),
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'claude',
      workspacePath: '/home/me/repo',
    });

    expect(presentation.kind).toBe('file-edit-placeholder');
    if (presentation.kind !== 'file-edit-placeholder') return;
    expect(presentation.file).toMatchObject({
      path: 'frontend/src/lib/components/composer/Composer.svelte',
      kind: 'modified',
      additions: 0,
      deletions: 0,
      lines: [],
    });
  });

  it('routes a summary-only multi-file edit to one placeholder per file', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'file_change',
      summary: 'file_change',
      meta: JSON.stringify({
        toolName: 'file_change',
        input: {
          files: [
            '/home/me/dotfiles/install.sh',
            '/home/me/dotfiles/setup/packages.sh',
          ],
        },
      }),
    });

    const presentation = resolveToolPresentation({
      item,
      provider: 'codex',
      workspacePath: '/home/me/repo',
    });

    expect(presentation.kind).toBe('file-edit-placeholders');
    if (presentation.kind !== 'file-edit-placeholders') return;
    expect(presentation.files.map((file) => file.path)).toEqual([
      '/home/me/dotfiles/install.sh',
      '/home/me/dotfiles/setup/packages.sh',
    ]);
  });

  it('uses tray launch and completion items for command presentation', () => {
    const launch = makeItem({
      id: 'tool:0:0',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: sleep 10',
      payloadKind: undefined,
      payloadId: undefined,
    });
    const completion = makeItem({
      id: 'completion:0:0',
      kind: 'tool_completion',
      status: 'completed',
      completionOf: launch.id,
      payloadKind: 'command_output',
      payloadId: 'payload-command',
      payloadMeta: JSON.stringify({
        command: 'sleep 10',
        exitCode: 0,
        lineCount: 3,
        preview: 'done',
      }),
    });

    const presentation = resolveToolPresentation({
      item: completion,
      provider: 'claude',
      surface: 'tray',
      displayItem: launch,
      statusItem: completion,
      outputItem: completion,
    });

    expect(presentation.kind).toBe('command');
    if (presentation.kind !== 'command') return;
    expect(presentation.item).toBe(completion);
    expect(presentation.displayItem).toBe(launch);
    expect(presentation.statusItem).toBe(completion);
    expect(presentation.payloadId).toBe('payload-command');
    expect(presentation.meta.preview).toBe('done');
  });

  it('routes Claude advisor tool calls to advisor presentation', () => {
    // Claude's server-side advisor reuses the tool_call kind with
    // toolName="advisor". The branch must fire BEFORE the generic
    // tool fallthrough so AdvisorRow renders instead of the generic
    // header/body. Verifies both the running launch and the
    // completed row (with a tool_call_result payload) route to the
    // advisor kind — tool_call_result must NOT be intercepted by
    // the tool-result branch (which checks for the `tool_result`
    // payloadKind specifically).
    const running = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'advisor',
      summary: 'advisor',
      meta: JSON.stringify({ toolName: 'advisor', advisor_model: 'claude-opus-4-7' }),
    });
    const runningPresentation = resolveToolPresentation({
      item: running,
      provider: 'claude',
      surface: 'timeline',
    });
    expect(runningPresentation.kind).toBe('advisor');

    const completed = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'advisor',
      summary: 'advisor',
      payloadId: 'tool-call-result:srvtoolu_xyz',
      payloadKind: 'tool_call_result',
      payloadMeta: JSON.stringify({ preview: 'Reviewer suggested two refactors' }),
      meta: JSON.stringify({ toolName: 'advisor', advisor_model: 'claude-opus-4-7' }),
    });
    const completedPresentation = resolveToolPresentation({
      item: completed,
      provider: 'claude',
      surface: 'timeline',
    });
    expect(completedPresentation.kind).toBe('advisor');
  });

  it('does not classify non-advisor tool names as advisor', () => {
    // The advisor predicate hinges on toolName === 'advisor'; a row
    // for a different tool (Bash here) must NOT route to advisor.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: ls',
    });
    const presentation = resolveToolPresentation({
      item,
      provider: 'claude',
      surface: 'timeline',
    });
    expect(presentation.kind).not.toBe('advisor');
  });
});
