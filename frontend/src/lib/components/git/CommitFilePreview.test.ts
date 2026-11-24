import { describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import CommitFilePreview from './CommitFilePreview.svelte';
import { setReviewDiffMock } from '../../../test/mocks/bindings-app';
import { normalizedTextContent } from '../../../test/helpers/textContent';
import type { CommitFile, WorkspaceRef } from '../../types/git';
import type { EntityScopeGrant } from '../../transport/scopes';

const WORKSPACE = { projectId: 'p1', workspacePath: '/tmp/workspace' } as WorkspaceRef;
const FILE: CommitFile = { path: 'src/a.ts', status: 'M', additions: 1, deletions: 0, version: '100644:src/a.ts' };

function mount(grant: EntityScopeGrant) {
  const diff = setReviewDiffMock('OpenCommitFileDiff', () => 'diff --git a/src/a.ts b/src/a.ts\n--- a/src/a.ts\n+++ b/src/a.ts\n@@ -1 +1 @@\n-before\n+after\n');
  const view = render(CommitFilePreview, { props: { workspace: WORKSPACE, backend: '', file: FILE, grant } });
  return { diff, preview: () => normalizedTextContent(view.getByTestId('commit-file-preview')) };
}

// The preview body says why it shows no patch, and reads nothing it may not.
describe('CommitFilePreview without the files:read grant', () => {
  it.each([
    ['says the computer has to connect while its grant has not arrived', 'pending', 'Unavailable until this computer connects'],
    ['says not granted only for a known refusal', 'refused', 'Not granted to this device'],
  ] as const)('%s', (_name, grant, text) => {
    const { diff, preview } = mount(grant);
    expect(preview()).toBe(text);
    expect(diff).not.toHaveBeenCalled();
  });

  it('reads the patch when granted', async () => {
    const { diff, preview } = mount('granted');
    await waitFor(() => expect(preview()).toContain('after'));
    expect(diff).toHaveBeenCalledTimes(1);
  });
});
