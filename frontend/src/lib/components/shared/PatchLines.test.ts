import { render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseReviewFiles, PatchTextLost, type ReviewFile } from '../../utils/patchStore';
import { installDiagnosticsCapture } from '../../../test/helpers/diagnostics';
import PatchLines from './PatchLines.svelte';
import PatchLinesBlock from './PatchLinesBlock.svelte';

const disposers: ReviewFile[][] = [];

afterEach(() => {
  for (const files of disposers.splice(0)) files[0]?.body.segments[0]?.store.dispose();
});

function fileOf(): ReviewFile {
  const files = parseReviewFiles('diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1,2 +1,2 @@\n same\n-before\n+changed\n');
  disposers.push(files);
  return files[0];
}

describe('PatchLines', () => {
  it('renders the patch as plain text by default and in the given box', async () => {
    const view = render(PatchLines, { file: fileOf() });
    const box = view.getByTestId('patch-lines');
    await waitFor(() => expect(box).toHaveTextContent('-before +changed'));
    expect(box.className).toContain('max-h-72');
    expect(box.querySelector('span')).toBeNull();

    const sized = render(PatchLines, { file: fileOf(), boxClass: 'h-full' });
    const sizedBox = sized.getAllByTestId('patch-lines').at(-1)!;
    expect(sizedBox.className).toContain('h-full');
    expect(sizedBox.className).not.toContain('max-h-72');
  });

  it('tints added, removed and header lines when asked, one row per line', async () => {
    const view = render(PatchLines, { file: fileOf(), tint: true });
    const box = view.getByTestId('patch-lines');
    await waitFor(() => expect(box.querySelectorAll('span')).toHaveLength(7));
    const classOf = (text: string) =>
      [...box.querySelectorAll('span')].find((el) => el.textContent === text)?.className ?? 'missing';
    expect(classOf('+changed')).toContain('bg-success/12');
    expect(classOf('-before')).toContain('bg-error/12');
    expect(classOf('@@ -1,2 +1,2 @@')).toContain('text-accent/70');
    expect(classOf(' same')).not.toMatch(/bg-success|bg-error|text-accent/);
  });
});

describe('PatchLinesBlock', () => {
  const diagnostics = installDiagnosticsCapture();

  it('reports text it fails to read for any reason but lost text, which its owner handles', async () => {
    const file = fileOf();
    const whenResident = vi.spyOn(file.body, 'whenResident');
    const props = { file, start: 0, end: file.body.lineCount, padTop: 0, padBottom: 0, onWidth: () => {} };

    whenResident.mockRejectedValueOnce(new PatchTextLost());
    render(PatchLinesBlock, props);
    await waitFor(() => expect(whenResident).toHaveBeenCalledTimes(1));
    // Let the rejection be handled before asserting nothing was reported.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(await diagnostics.messages()).toEqual([]);

    whenResident.mockRejectedValueOnce(new Error('boom'));
    render(PatchLinesBlock, props);
    await waitFor(async () => {
      expect(await diagnostics.messages()).toEqual(['patch lines: block text could not be read']);
    });
  });
});
