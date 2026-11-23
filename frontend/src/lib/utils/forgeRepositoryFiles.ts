// Files of the PR's own repository that a PR/MR body shows, read from the
// local git object store by the facts store (stores/forgeFacts.svelte.ts):
//
//   - Repository images. An image whose destination names a repository file
//     (repositoryFiles.ts) reads its `image:` fact in the destination hook,
//     so every way of writing an image (Markdown, by reference, an HTML
//     `<img>`, `<video>` or `<picture>` candidate, in a paragraph or an HTML
//     block)
//     gets the same claim: found, an app href with the blob id and pixel
//     size (repositoryImageHref.ts) that RepositoryImageHost.svelte paints
//     at its final size; a Git LFS pointer, absent or still asked, the same
//     href with that state. Unread or unanswerable, the image stays as
//     written, as it renders without facts.
//   - GitHub permalink snippets. A bare URL of a file of this repository at
//     a full commit id, with no query but `?plain=1`, and a `#L<a>` or
//     `#L<a>-L<b>` fragment reads its `lines:` fact; found, it stands as a
//     block (`liftToBlock`): a header
//     linking the file and the commit over the lines, numbered from the
//     first, highlighted by the file's extension. Asked, the same frame with
//     that many empty lines, so the answer changes content and not height.
//     Anything else stays the ordinary link.
//
// The keys a body can read are found from its raw text, ahead of its
// first paint, by forgeRepositoryScan.ts.

import type { ColorScheme, ColorSchemeSources, CodeSnippetToken, DestinationContext, DestinationHook, Extension, Token, Tokens } from '../markdown';
import { colorSchemeOnly, liftToBlock, mediaOfHref, readFact } from '../markdown';
import type { ForgeAttachmentSource } from './forgeAttachments';
import { factRevision, imageFactKey, linesFactKey, REVISION_FACT_KEY, type FactRevision, type ForgeFact, type ForgeFactState } from './forgeFacts';
import { forgeShapes, type ForgeShapes } from './forgeReferenceExtension';
import { buildRepositoryImageHref, type RepositoryImageState } from './repositoryImageHref';
import { cleanRepositoryPath, decodeOnce, repositoryFileOf, type RepositoryFile, type RepositorySite } from './repositoryFiles';

/** The lines a snippet shows at most; a longer range is not asked for and stays a link. */
export const MAX_SNIPPET_LINES = 200;

/** Observability for tests: how many permalink candidates were read. */
export const permalinkCounters = { reads: 0 };

/**
 * The repository file an image destination names, or null. GitLab plays a
 * video or audio path as a player (markedMediaImages), never an image.
 */
export function repositoryImageOf(href: string, site: RepositorySite, revision: () => FactRevision): RepositoryFile | null {
  const file = repositoryFileOf(href, site, revision);
  return file && (site.github || mediaOfHref(file.path) === null) ? file : null;
}

/** A GitHub permalink to lines of a file of this repository. */
export interface Permalink {
  readonly sha: string;
  readonly path: string;
  readonly start: number;
  readonly end: number;
  /** The repository name as the URL writes it, for the header. */
  readonly repo: string;
  readonly owner: string;
}

const PERMALINK_PATH = /^\/([^/]+)\/([^/]+)\/blob\/([0-9a-f]{40})\/(.+)$/;
const LINE_RANGE = /^#L([1-9]\d{0,8})(?:-L([1-9]\d{0,8}))?$/;

/**
 * The permalink `href` is on `shapes`' forge, or null (GitHub only). Its
 * one query may be `plain=1`, GitHub's way to link lines of a file it
 * would otherwise render (a Markdown file); any other query is no
 * permalink.
 */
export function permalinkOf(href: string, shapes: Pick<ForgeShapes, 'github' | 'host' | 'project'>): Permalink | null {
  permalinkCounters.reads++;
  if (!shapes.github || !href.startsWith('https://')) return null;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return null;
  }
  if (url.host.toLowerCase() !== shapes.host || (url.search !== '' && url.search !== '?plain=1') || url.username !== '' || url.password !== '') return null;
  const range = LINE_RANGE.exec(url.hash);
  const parts = range && PERMALINK_PATH.exec(url.pathname);
  if (!range || !parts || `${parts[1]}/${parts[2]}`.toLowerCase() !== shapes.project.toLowerCase()) return null;
  const decoded = decodeOnce(parts[4]);
  const path = decoded === null ? null : cleanRepositoryPath(decoded);
  if (path === null) return null;
  const first = Number(range[1]);
  const last = range[2] === undefined ? first : Number(range[2]);
  const start = Math.min(first, last);
  const end = Math.max(first, last);
  if (end - start + 1 > MAX_SNIPPET_LINES) return null;
  return { sha: parts[3], path, start, end, repo: parts[2], owner: parts[1] };
}

/** The fence language a file's name highlights as, '' for plain text. */
export function snippetLanguage(path: string): string {
  const name = path.slice(path.lastIndexOf('/') + 1).toLowerCase();
  if (name === 'makefile' || name === 'dockerfile') return name;
  const dot = name.lastIndexOf('.');
  return dot > 0 ? name.slice(dot + 1) : '';
}

const textToken = (text: string): Tokens.Text => ({ type: 'text', raw: text, text });

const linkToken = (href: string, children: Token[]): Tokens.Link => ({
  type: 'link',
  raw: '',
  href,
  title: null,
  text: children.map((child) => ('text' in child ? child.text : '')).join(''),
  tokens: children,
});

/** The snippet a permalink stands as, its code `text` (empty lines while asked). */
function snippetToken(raw: string, href: string, origin: string, link: Permalink, text: string, lang: string): CodeSnippetToken {
  const lines = link.start === link.end ? `Line ${link.start} in ` : `Lines ${link.start} to ${link.end} in `;
  const header: Tokens.Paragraph = {
    type: 'paragraph',
    raw: '',
    text: '',
    tokens: [
      linkToken(href, [textToken(`${link.repo}/${link.path}`)]),
      { type: 'br', raw: '' } as Token,
      textToken(lines),
      linkToken(`${origin}/${link.owner}/${link.repo}/commit/${link.sha}`, [{ type: 'codespan', raw: '', text: link.sha.slice(0, 7) }]),
    ],
  };
  return { type: 'codeSnippet', raw, tokens: [header, { type: 'code', raw: '', lang, text, firstLine: link.start }] };
}

type ClaimedImage = Tokens.Image & { sources?: ColorSchemeSources; shownIn?: ColorScheme; inLink?: true };

/**
 * The repository files of one PR's surface, as an inline extension: the
 * image claims as its destination hook and, on GitHub, the permalink
 * snippets as its tokenizer. Null without a forge address. Its identity is
 * the caller's to hold per PR (chatMarkdownOptions.ts); the revision and
 * the answers it reads are facts, so a new head rebuilds only the blocks
 * that read it.
 */
export function buildForgeRepositoryFiles(source: ForgeAttachmentSource): Extension | null {
  const shapes = forgeShapes(source);
  if (shapes === null) return null;
  const { pr, backend } = source;
  const fileUrl = (commit: string, path: string) =>
    `${shapes.origin}/${shapes.project}${shapes.github ? '' : '/-'}/blob/${commit}/${path.split('/').map(encodeURIComponent).join('/')}`;

  // The claim `href` gets, or null when it is no repository image or its
  // fact gives nothing to show (unread, unanswerable): it stays as written.
  const claimOf = (href: string, context: DestinationContext): { href: string; browserUrl: string } | null => {
    const file = repositoryImageOf(href, shapes, () => factRevision(context.fact(REVISION_FACT_KEY) as ForgeFactState));
    if (!file) return null;
    const fact = context.fact(imageFactKey(file.commit, file.path)) as ForgeFactState;
    let state: RepositoryImageState;
    if (fact === 'pending') state = 'pending';
    else if (typeof fact !== 'object' || fact.status === 'unavailable') return null;
    else if (fact.status === 'absent') state = 'absent';
    else if (fact.lfs) state = 'lfs';
    else if (typeof fact.oid === 'string' && /^[0-9a-f]{40}(?:[0-9a-f]{24})?$/.test(fact.oid)) state = 'found';
    else return null;
    const found = state === 'found' ? (fact as ForgeFact) : null;
    const browserUrl = fileUrl(file.commit, file.path);
    return {
      browserUrl,
      href: buildRepositoryImageHref({
        state,
        backend,
        pr,
        commit: file.commit,
        path: file.path,
        oid: found?.oid ?? '',
        width: found?.width ?? 0,
        height: found?.height ?? 0,
        fluid: found?.fluid === true,
        browserUrl,
      }),
    };
  };

  const destination: DestinationHook = (token, context) => {
    if (token.type !== 'image') return undefined;
    const image = token as ClaimedImage;
    const claimed = claimOf(image.href, context);
    // A `<picture>`'s scheme sources are claimed the same way.
    let sources: ColorSchemeSources | undefined;
    let claimsSource = false;
    if (image.sources) {
      sources = {};
      for (const scheme of ['dark', 'light'] as const) {
        const src = image.sources[scheme];
        if (src === undefined) continue;
        const each = claimOf(src, context);
        claimsSource ||= each !== null;
        sources[scheme] = each?.href ?? src;
      }
    }
    if (!claimed && !claimsSource) return undefined;
    const shownIn = claimed ? colorSchemeOnly(image.href) : null;
    return {
      ...image,
      ...(claimed ? { href: claimed.href, title: image.title || claimed.browserUrl } : {}),
      ...(shownIn ? { shownIn } : {}),
      ...(context.inLink ? { inLink: true as const } : {}),
      ...(sources ? { sources } : {}),
    };
  };

  return {
    name: 'forgeRepositoryFiles',
    level: 'inline',
    destination,
    tokenizer(src) {
      // Opening characters before anything else: this runs at every position.
      if (!shapes.github || src.charCodeAt(0) !== 104 /* h */ || !src.startsWith('https://')) return undefined;
      const state = this.lexer.state;
      if (state.inLink || state.inLinkText || state.inHtmlText) return undefined;
      const url = this.lexer.tokenizer.url(src);
      if (!url) return undefined;
      const link = permalinkOf(url.href, shapes);
      if (!link) return undefined;
      const fact = readFact<ForgeFact>(this.lexer, linesFactKey(link.sha, link.path, link.start, link.end));
      let text: string;
      let lang = '';
      if (fact === 'pending') text = '\n'.repeat(link.end - link.start);
      else if (typeof fact === 'object' && fact.status === 'found' && typeof fact.text === 'string') {
        text = fact.text;
        lang = snippetLanguage(link.path);
      } else return undefined;
      return liftToBlock(this.lexer, url, snippetToken(url.raw, url.href, shapes.origin, link, text, lang));
    },
  };
}
