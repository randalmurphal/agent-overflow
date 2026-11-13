import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ReviewPaneBanners from './ReviewPaneBanners.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import { parseReviewFiles } from '../../utils/patchStore';

const BINARY = parseReviewFiles([
  'diff --git a/logo.png b/logo.png',
  'index 1111111..2222222 100644',
  'Binary files a/logo.png and b/logo.png differ',
  '',
].join('\n'));
const TEXT = parseReviewFiles([
  'diff --git a/a.ts b/a.ts',
  'index 1111111..2222222 100644',
  '--- a/a.ts',
  '+++ b/a.ts',
  '@@ -1 +1 @@',
  '-a',
  '+b',
  '',
].join('\n'));

function review(unavailable: string | null, files: unknown[]): ReviewPaneState {
  return {
    error: null, submitError: null, conflictsError: null, prUpdateError: null, prUpdateFailure: null,
    prStale: false, loading: false, scope: 'workspace', prRef: null, commitSelectionLost: false,
    mediaHost: { media: { unavailable } }, files,
  } as unknown as ReviewPaneState;
}

describe('the diff\'s files notice', () => {
  it('says once why a diff with binary files shows no images', () => {
    const view = render(ReviewPaneBanners, { review: review('The diff\'s files could not be pinned.', [...TEXT, ...BINARY, ...BINARY]), branchesError: null });
    expect(view.getAllByTestId('review-files-unavailable')).toHaveLength(1);
    expect(view.getByTestId('review-files-unavailable').textContent!.trim()).toBe('No images for this diff: The diff\'s files could not be pinned.');
  });

  it('says nothing for a diff with no binary file, or whose files can be read', () => {
    expect(render(ReviewPaneBanners, { review: review('The diff\'s files could not be pinned.', TEXT), branchesError: null }).queryByTestId('review-files-unavailable')).toBeNull();
    expect(render(ReviewPaneBanners, { review: review(null, BINARY), branchesError: null }).queryByTestId('review-files-unavailable')).toBeNull();
  });
});
