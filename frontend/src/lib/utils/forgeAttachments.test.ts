import { describe, expect, it } from 'vitest';
import {
  FORGE_ATTACHMENT_HREF_PREFIX,
  browserUrlForForgeAttachment,
  buildForgeAttachmentHref,
  forgeAttachmentName,
  isForgeAttachmentHref,
  parseForgeAttachmentHref,
} from './forgeAttachments';
import { PATH_LINK_HREF_PREFIX } from './pathLinkExtension';
import type { PRRef } from './prReference';

const HEX = '0123456789abcdef0123456789abcdef';

const GITHUB_PR: PRRef = { forge: 'github', namespace: 'acme', repo: 'widget', number: 7 };
const GITLAB_MR: PRRef = { forge: 'gitlab', namespace: 'group/sub', repo: 'widget', number: 3 };

describe('github attachment shapes', () => {
  it.each([
    'https://github.com/user-attachments/assets/2b6d0f0e-1111-2222-3333-444455556666',
    'https://github.com/user-attachments/files/19283746/report.pdf',
    'https://github.com/acme/widget/assets/12345/2b6d0f0e-aaaa',
    'https://github.com/acme/widget/files/19283746/notes.txt',
    'https://private-user-images.githubusercontent.com/1/2.png?jwt=abc',
    'HTTPS://GitHub.com/user-attachments/assets/ID',
    'http://github.com/user-attachments/assets/ID',
  ])('claims %s', (href) => {
    expect(isForgeAttachmentHref('github', href)).toBe(true);
  });

  it.each([
    // Public, and the page can fetch it itself: claiming it would spend an
    // RPC and a ticket on bytes an <img> already renders.
    'https://user-images.githubusercontent.com/1/2.png',
    'https://github.com/acme/widget/pull/7',
    'https://github.com/acme/widget/blob/main/README.md',
    'https://evil.test/user-attachments/assets/id',
    'https://github.com.evil.test/user-attachments/assets/id',
    '/user-attachments/assets/id',
    'ftp://github.com/user-attachments/assets/id',
    '',
  ])('leaves %s alone', (href) => {
    expect(isForgeAttachmentHref('github', href)).toBe(false);
  });

  it('never claims a github URL for a gitlab merge request', () => {
    expect(isForgeAttachmentHref('gitlab', 'https://github.com/user-attachments/assets/x')).toBe(false);
  });

  it('names the file segment when there is one, else the id', () => {
    expect(forgeAttachmentName('github', 'https://github.com/user-attachments/files/1/a%20b.pdf'))
      .toBe('a b.pdf');
    expect(forgeAttachmentName('github', 'https://github.com/user-attachments/assets/abc-123'))
      .toBe('abc-123');
    expect(forgeAttachmentName('github', 'https://private-user-images.githubusercontent.com/1/shot.png?jwt=x'))
      .toBe('shot.png');
  });
});

describe('gitlab upload shapes', () => {
  it.each([
    `/uploads/${HEX}/shot.png`,
    `/-/project/4711/uploads/${HEX}/shot.png`,
    `https://gitlab.com/group/sub/widget/uploads/${HEX}/shot.png`,
    `https://gitlab.example.test/group/widget/uploads/${HEX}/report.pdf`,
    `https://gitlab.example.test/-/project/4711/uploads/${HEX}/report.pdf`,
    `/uploads/${HEX}/shot.png?inline=false`,
  ])('claims %s', (href) => {
    expect(isForgeAttachmentHref('gitlab', href)).toBe(true);
  });

  it.each([
    // The secret must be a 32-char lowercase hex run; everything else is an
    // ordinary repository path that happens to contain the word uploads.
    '/uploads/short/shot.png',
    `/uploads/${HEX.toUpperCase()}/shot.png`,
    `/uploads/${HEX}/nested/shot.png`,
    `/uploads/${HEX}/`,
    `docs/uploads/${HEX}/shot.png`,
    `//gitlab.com/uploads/${HEX}/shot.png`,
    // Absolute needs a project in front of the upload, which is how GitLab
    // addresses one.
    `https://gitlab.com/uploads/${HEX}/shot.png`,
    // Relative admits only the two project spellings, nothing deeper.
    `/group/widget/uploads/${HEX}/shot.png`,
  ])('leaves %s alone', (href) => {
    expect(isForgeAttachmentHref('gitlab', href)).toBe(false);
  });

  it('percent-decodes the upload name', () => {
    expect(forgeAttachmentName('gitlab', `/uploads/${HEX}/a%20b.png`)).toBe('a b.png');
  });
});

describe('the URL a browser would open', () => {
  const WEB = 'https://gitlab.example.test/group/sub/widget/-/merge_requests/3';

  it('resolves a relative gitlab upload against the MR project', () => {
    expect(browserUrlForForgeAttachment('gitlab', `/uploads/${HEX}/x.png`, WEB, GITLAB_MR))
      .toBe(`https://gitlab.example.test/group/sub/widget/uploads/${HEX}/x.png`);
  });

  it('keeps a project-id upload addressed as written', () => {
    expect(browserUrlForForgeAttachment('gitlab', `/-/project/4711/uploads/${HEX}/x.png`, WEB, GITLAB_MR))
      .toBe(`https://gitlab.example.test/-/project/4711/uploads/${HEX}/x.png`);
  });

  it('has no honest answer for a relative upload with no MR web URL', () => {
    expect(browserUrlForForgeAttachment('gitlab', `/uploads/${HEX}/x.png`, '', GITLAB_MR)).toBeNull();
  });

  it('normalizes an absolute github reference to https', () => {
    expect(browserUrlForForgeAttachment('github', 'http://github.com/user-attachments/assets/x', '', GITHUB_PR))
      .toBe('https://github.com/user-attachments/assets/x');
  });

  it('is null for an href this forge does not serve', () => {
    expect(browserUrlForForgeAttachment('github', 'https://example.test/x.png', '', GITHUB_PR)).toBeNull();
  });
});

describe('the nonce-gated href', () => {
  it('round-trips every field the click and render paths need', () => {
    const href = buildForgeAttachmentHref({
      href: `/uploads/${HEX}/x.png`,
      pr: GITLAB_MR,
      backend: 'gpu',
      webBase: 'https://gitlab.example.test/group/sub/widget/-/merge_requests/3',
    });
    expect(href.startsWith(FORGE_ATTACHMENT_HREF_PREFIX)).toBe(true);
    expect(parseForgeAttachmentHref(href)).toEqual({
      href: `/uploads/${HEX}/x.png`,
      pr: GITLAB_MR,
      backend: 'gpu',
      webBase: 'https://gitlab.example.test/group/sub/widget/-/merge_requests/3',
    });
  });

  it('keeps the home backend and an unknown web URL as empty strings', () => {
    const href = buildForgeAttachmentHref({
      href: 'https://github.com/user-attachments/assets/x',
      pr: GITHUB_PR,
      backend: '',
      webBase: '',
    });
    expect(parseForgeAttachmentHref(href)).toEqual({
      href: 'https://github.com/user-attachments/assets/x',
      pr: GITHUB_PR,
      backend: '',
      webBase: '',
    });
  });

  it.each([
    // The nonce is the whole defence: PR bodies are third-party text, and
    // without it any of them could name another computer and repository.
    'agent-overflow:forge?nonce=deadbeef&href=%2Fetc%2Fpasswd&forge=github&repo=x&n=1',
    'agent-overflow:forge?href=x&forge=github&repo=x&n=1',
    `${PATH_LINK_HREF_PREFIX}path=%2Fetc%2Fpasswd`,
    'https://example.test/x',
    '',
    null,
  ])('refuses a forged or foreign href: %s', (href) => {
    expect(parseForgeAttachmentHref(href)).toBeNull();
  });

  it.each([
    'forge=svn&repo=x&n=1&href=y',
    'forge=github&repo=x&n=0&href=y',
    'forge=github&repo=x&n=abc&href=y',
    'forge=github&repo=&n=1&href=y',
    'forge=github&repo=x&n=1',
  ])('refuses our own prefix with an unusable payload: %s', (query) => {
    expect(parseForgeAttachmentHref(`${FORGE_ATTACHMENT_HREF_PREFIX}${query}`)).toBeNull();
  });
});
