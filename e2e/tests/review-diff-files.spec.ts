// A pull request's commit runs and its image files, through the real
// backend's bound methods and byte route, on a PR seeded with
// `imagePullHistory` (images added, changed, deleted and renamed, a merge
// commit, a non-image binary) on GitHub and GitLab. Covered: the commit list
// the picker reads; a run of commits is the two-tree diff of its ends, and a
// run covering the whole PR is refused; an image's sizes and both sides'
// bytes arrive at the sides the open pinned (held on the server under the
// open's files handle), over the single-use URL the webview fetches; an SVG
// answers sandboxed; the refusals a file read answers carry their codes
// over the wire. The UI that shows them is covered by its own specs.

import { randomBytes } from 'node:crypto';
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { DIAGRAM_SVG, expectEveryForgeCallHandled, imagePullHistory, publishPullRequest, solidPNG } from './forge-helpers.js';

interface Commit { sha: string; subject: string }
interface Opened { id: string; chunk: { data: string; nextOffset: number; eof: boolean }; filesHandle?: string }
interface ImageSide { exists: boolean; size: number; url?: string; mimeType?: string; overCap?: boolean; notImage?: boolean }
interface Sizes { path: string; old: { exists: boolean; size: number }; new: { exists: boolean; size: number }; error?: string; code?: string }

async function readPatch(harness: HarnessApp, opened: Opened): Promise<string> {
  let patch = opened.chunk.data;
  let chunk = opened.chunk;
  while (!chunk.eof) {
    chunk = await harness.rpc('ReadReviewDiff', opened.id, chunk.nextOffset, 1 << 20);
    patch += chunk.data;
  }
  if (opened.id) await harness.rpc('ReleaseReviewDiff', opened.id);
  return patch;
}

/** The handle an open kept its pinned sides under. */
function pinned(opened: Opened): string {
  expect(opened.filesHandle).toBeTruthy();
  return opened.filesHandle!;
}

for (const forge of ['github', 'gitlab'] as const) {
  test(`a ${forge} PR's commit runs and image sides read at the diff's revisions`, async ({ harness }) => {
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `images-${randomBytes(4).toString('hex')}`, repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] } }],
    });
    const { projectId, path: workspacePath } = seed.projects[0];
    const repo = await publishPullRequest(harness, workspacePath, {
      forge,
      project: `ao-e2e/images-${randomBytes(4).toString('hex')}`,
      pulls: [{ number: 7, title: 'Images', body: '' }],
    }, imagePullHistory());
    const pull = repo.pulls![0];
    const ws = { projectId, workspacePath };
    const pr = { Forge: forge, Host: forge === 'github' ? 'github.com' : 'gitlab.com', Namespace: 'ao-e2e', Repo: repo.project.split('/')[1], Number: 7 };
    const revision = { baseRef: 'main', baseSHA: pull.baseSha!, headSHA: pull.headSha! };

    const listed = await harness.rpc<{ commits: Commit[]; total: number | null }>('ListPRCommits', ws, pr, revision, true);
    const { commits } = listed;
    expect(listed.total).toBe(commits.length);
    expect(commits.map((c) => c.subject)).toEqual(['add blob', 'rename moved', 'merge add diagram', 'add diagram', 'restyle logo', 'add icon']);
    const [blob, , merge, , restyle, icon] = commits;

    // restyle logo..merge: the logo change, old.png's delete and the merged
    // diagram; nothing the first or later commits did.
    const runOpened = await harness.rpc<Opened>('OpenPRCommitDiff', ws, pr, revision, restyle.sha, merge.sha, false, true);
    const runHandle = pinned(runOpened);
    const run = await readPatch(harness, runOpened);
    expect(run).toContain('b/logo.png');
    expect(run).toContain('a/old.png');
    expect(run).toContain('b/diagram.svg');
    expect(run).not.toContain('icon.png');
    expect(run).not.toContain('moved.png');
    await expect(harness.rpc('OpenPRCommitDiff', ws, pr, revision, icon.sha, blob.sha, false, true)).rejects.toThrow(/covers every commit/);
    await expect(harness.rpc('OpenPRCommitDiff', ws, pr, revision, blob.sha, icon.sha, false, true)).rejects.toThrow(/out of order/);

    const logo = await harness.rpc<{ old: ImageSide; new: ImageSide }>('GetReviewDiffImage', ws, runHandle, { path: 'logo.png' });
    for (const [side, want] of [[logo.old, solidPNG(16, 16, [200, 40, 40])], [logo.new, solidPNG(32, 32, [40, 200, 200])]] as const) {
      expect(side).toMatchObject({ exists: true, size: want.length, mimeType: 'image/png' });
      const response = await fetch(new URL(side.url!, harness.url));
      expect(response.status).toBe(200);
      expect(new Uint8Array(await response.arrayBuffer())).toEqual(want);
    }
    const diagram = await harness.rpc<{ old: ImageSide; new: ImageSide }>('GetReviewDiffImage', ws, runHandle, { path: 'diagram.svg' });
    expect(diagram.old).toEqual({ exists: false, size: 0 });
    const svg = await fetch(new URL(diagram.new.url!, harness.url));
    expect(await svg.text()).toBe(DIAGRAM_SVG);
    expect(svg.headers.get('content-security-policy')).toBe("default-src 'none'; style-src 'unsafe-inline'; sandbox");
    expect(svg.headers.get('content-disposition')).toBe('attachment');

    // The whole PR: sizes for every binary file, a pure rename's sides
    // equal, a deleted file's new side absent, a non-image's bytes refused.
    const wholeOpened = await harness.rpc<Opened>('OpenPRDiff', ws, pr, revision, false, true);
    const wholeHandle = pinned(wholeOpened);
    const whole = await readPatch(harness, wholeOpened);
    expect(whole).toContain('b/img/moved.png');
    const sizes = await harness.rpc<Sizes[]>('GetReviewDiffFileSizes', ws, wholeHandle, [
      { path: 'img/moved.png', oldPath: 'moved.png' },
      { path: 'old.png' },
      { path: 'data.bin' },
      { path: 'img' },
    ]);
    const moved = solidPNG(10, 10, [40, 160, 40]).length;
    expect(sizes).toEqual([
      { path: 'img/moved.png', oldPath: 'moved.png', old: { exists: true, size: moved }, new: { exists: true, size: moved } },
      { path: 'old.png', old: { exists: true, size: solidPNG(8, 8, [40, 40, 200]).length }, new: { exists: false, size: 0 } },
      { path: 'data.bin', old: { exists: false, size: 0 }, new: { exists: true, size: 8 } },
      { path: 'img', old: { exists: false, size: 0 }, new: { exists: false, size: 0 }, error: 'img is not a file', code: 'review_diff_file_refused' },
    ]);
    await expect(harness.rpc('GetReviewDiffImage', ws, wholeHandle, { path: 'img' }))
      .rejects.toThrow('review_diff_file_refused: img is not a file');
    const data = await harness.rpc<{ new: ImageSide }>('GetReviewDiffImage', ws, wholeHandle, { path: 'data.bin' });
    expect(data.new).toEqual({ exists: true, size: 8, notImage: true });
    await expect(harness.rpc('GetReviewDiffImage', ws, 'no-such-handle', { path: 'data.bin' }))
      .rejects.toThrow("review_diff_files_gone: This diff's files are no longer held; reload the diff.");

    // A working-tree file rewritten after its diff was opened is refused
    // with its own code, which the client offers a reload on.
    writeFileSync(join(workspacePath, 'fresh.png'), solidPNG(6, 6, [10, 20, 30]));
    const worktree = await harness.rpc<Opened>('OpenWorkspaceDiff', ws, false);
    const worktreeHandle = pinned(worktree);
    await readPatch(harness, worktree);
    writeFileSync(join(workspacePath, 'fresh.png'), solidPNG(7, 7, [30, 20, 10]));
    await expect(harness.rpc('GetReviewDiffImage', ws, worktreeHandle, { path: 'fresh.png' }))
      .rejects.toThrow("review_diff_files_changed: This file changed since the diff was read; reload the diff.");
    await expectEveryForgeCallHandled(harness);
  });
}
