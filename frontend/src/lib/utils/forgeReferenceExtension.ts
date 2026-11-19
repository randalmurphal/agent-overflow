// Inline extension that links what a forge links in a PR/MR body or comment:
// issue, PR and MR references, mentions, commits and ranges, a pasted forge
// URL in its short form, bare email addresses and GitLab wiki links. Every
// claim is an ordinary `link` token with an absolute forge URL, so both
// render paths, the click path and find need nothing new. Its destination
// hook resolves a relative link where the forge resolves it, however the
// link was written (`DestinationHook`).
//
// A forge links a reference only when its target exists, which this page
// cannot know. So a shape is linked only when it is unambiguous: `#12`,
// `!12`, `owner/repo#12`, `@user`, a 40-hex SHA, and a short SHA that mixes
// digits and letters (an ordinary word or number never does). A shape that
// is also ordinary prose (`$5`, `%1`, `&1`, `~bug`, `project#1` for another
// project) stays text. A path segment that is `.` or `..` names no
// project, so a path holding one is not a reference.
//
// Nothing links inside link text or a raw `<a>` (`inLinkText`, `inLink`),
// and the engine consumes code spans, code blocks and HTML tags before any
// position inside them reaches this tokenizer.

import type { DestinationHook, Extension, Token, Tokens, TokensList } from '../markdown';
import { isOpenableScheme } from '../markdown';
import type { ForgeAttachmentSource } from './forgeAttachments';

/** Characters that continue a word, a path or an email local part. */
const WORD = /[A-Za-z0-9_.+/-]/;
/** Characters that may not precede a reference that opens with a word. */
const NOT_BEFORE_WORD = /[A-Za-z0-9_.+/@:&#~-]/;
/** Characters that may not precede an email address: its own local part's,
 * which the forges scan back over for its start. */
const NOT_BEFORE_EMAIL = /[A-Za-z0-9_.+/@&#~-]/;
/** Characters that may not precede `#12`, `!12` or `@user`. */
const NOT_BEFORE_SIGIL = /[A-Za-z0-9_/@.&`-]/;
const WORD_CHAR = /[A-Za-z0-9_]/;

// Where the next reference can open, for the text rule to stop at. A
// word-shaped reference opens after a boundary; `^` is a word start only
// when the character before the window is not a word character (start()).
// `!` and `[[` are stops of the engine's text rule too; a text run of HTML
// (`Lexer.htmlTextTokens`) stops only here.
const START = /[#@!]|\[\[|(?:^|[^A-Za-z0-9_.+/@:&#~-])(?:[A-Za-z0-9_][A-Za-z0-9_.+/-]*[#!@>]|[0-9a-f]{7}|GH-\d|mailto:|xmpp:|[A-Za-z][A-Za-z0-9+.-]*:\/\/)|:[A-Za-z0-9._+-]+@/g;

const ISSUE = /^#([1-9]\d*)/;
const MERGE_REQUEST = /^!([1-9]\d*)/;
const GH_ISSUE = /^GH-([1-9]\d*)/;
const GITLAB_SUFFIX = /^\+s?(?![A-Za-z0-9_])/;
const GITHUB_MENTION = /^@([A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38})(?:\/([A-Za-z0-9][A-Za-z0-9_-]*))?/;
const GITLAB_MENTION = /^@([A-Za-z0-9_][A-Za-z0-9_.-]*(?:\/[A-Za-z0-9_][A-Za-z0-9_.-]*)*)/;
// GFM extended email autolinks; the domain never ends with `.`.
const EMAIL = /^[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+/;
const SCHEME_EMAIL = /^(?:mailto:[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+|xmpp:[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+(?:\/[A-Za-z0-9@.]+)?)/;
// CommonMark's email autolink, which the engine leaves off for agent chat.
const ANGLE_EMAIL = /^<([A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*)>/;
// `owner/repo#12`, `group/sub/project!12`, `user@sha`, `group/project>`.
const PATH_REFERENCE = /^((?:[A-Za-z0-9_.-]+\/)*[A-Za-z0-9_.-]+)([#!@>])/;
const COMMITS = /^([0-9a-f]{7,40})(?:\.\.\.([0-9a-f]{7,40}))?/;
const WIKI = /^\[\[([^[\]|\n]+)(?:\|([^[\]|\n]+))?\]\]/;
const SCHEME = /^[A-Za-z][A-Za-z0-9+.-]*:/;
// GitLab links a URL of any scheme; the engine links http, https and ftp.
const SCHEME_URL = /^([A-Za-z][A-Za-z0-9+.-]{1,31}):\/\/[^\s<]+/;
const ENGINE_SCHEMES = /^(?:https?|ftp)$/i;
/** A path segment that names no project: the browser folds it away. */
const DOT_SEGMENT = /(?:^|\/)\.\.?(?:\/|$)/;

/** Whether a reference may end at `index`: no word character follows. */
function endsAt(src: string, index: number): boolean {
  return index >= src.length || !WORD_CHAR.test(src[index]);
}

/** A commit SHA, as far as the page can tell: 40 hex, or a short run that
 * holds a digit and a letter, so it is neither a word nor a number. */
function commitShaped(hex: string): boolean {
  return hex.length === 40 || (/[0-9]/.test(hex) && /[a-f]/.test(hex));
}

/** Whether a SHA ends at `index`: a word, path or `.word` continuing it
 * makes it part of something else. */
function shaEndsAt(src: string, index: number): boolean {
  if (index >= src.length) return true;
  const next = src[index];
  if (/[A-Za-z0-9_@/-]/.test(next)) return false;
  return !(next === '.' && /[A-Za-z0-9]/.test(src[index + 1] ?? ''));
}

function originOf(url: string): string | null {
  if (url === '') return null;
  try {
    return new URL(url).origin;
  } catch {
    return null;
  }
}

function textToken(text: string): Tokens.Text {
  return { type: 'text', raw: text, text };
}

/** GitHub sets a SHA in monospace. */
function codeToken(text: string): Tokens.Codespan {
  return { type: 'codespan', raw: text, text };
}

function link(raw: string, href: string, children: Token[]): Tokens.Link {
  const text = children.map((child) => ('text' in child ? child.text : '')).join('');
  return { type: 'link', raw, href, title: null, text, tokens: children };
}

/**
 * The references of one PR's surface, as an inline extension. In a text run
 * of HTML (`state.inHtmlText`) it links what the forge links in HTML text:
 * references and mentions, and on GitLab a URL of any scheme and a wiki
 * link. Its identity is the caller's to hold per PR (chatMarkdownOptions.ts).
 */
export function buildForgeReferences(source: ForgeAttachmentSource): Extension {
  const { pr, webBase } = source;
  const github = pr.forge === 'github';
  const origin = originOf(webBase) ?? originOf(`https://${pr.host}`);
  // With no forge address there is nothing to link to.
  if (origin === null) return { name: 'forgeReference', level: 'inline', tokenizer: () => undefined };
  const host = new URL(origin).host.toLowerCase();
  const project = `${pr.namespace}/${pr.repo}`;
  const projectKey = project.toLowerCase();
  const projectUrl = `${origin}/${project}`;
  // The page a forge comment lives on, which a page-relative href resolves
  // against in the forge's browser.
  const pageUrl = webBase !== '' ? webBase : github ? `${projectUrl}/pull/${pr.number}` : `${projectUrl}/-/merge_requests/${pr.number}`;
  // GitLab resolves a repository-relative link on the default branch, which
  // `HEAD` names in its blob URLs.
  const blobBase = `${projectUrl}/-/blob/HEAD/`;
  const shortSha = (sha: string) => sha.slice(0, github ? 7 : 8);
  const objectUrl = (path: string, kind: string) => `${origin}/${path}${github ? '' : '/-'}/${kind}`;

  // The path a reference names, or null when the written prefix is not a
  // project this rule may link.
  const referencedPath = (written: string): string | null => {
    if (DOT_SEGMENT.test(written)) return null;
    const slash = written.includes('/');
    if (github) return slash ? (written.split('/').length === 2 ? written : null) : null;
    if (slash) return written;
    // A bare `project#1` is linked for this project only: another name
    // there is as likely a word (`issue#1`, `pkg!1`).
    return written.toLowerCase() === pr.repo.toLowerCase() ? project : null;
  };
  // How a reference to `path` reads from this project.
  const relativePath = (path: string): string => {
    if (path.toLowerCase() === projectKey) return '';
    if (!github) {
      const cut = path.lastIndexOf('/');
      if (cut > 0 && path.slice(0, cut).toLowerCase() === pr.namespace.toLowerCase()) return path.slice(cut + 1);
    }
    return path;
  };

  const issueLink = (raw: string, path: string, kind: '#' | '!', number: string, shown?: string): Tokens.Link => {
    const target = kind === '!' ? objectUrl(path, `merge_requests/${number}`) : objectUrl(path, `issues/${number}`);
    return link(raw, target, [textToken(shown ?? `${relativePath(path)}${kind}${number}`)]);
  };

  const commitLink = (raw: string, path: string, from: string, to: string | undefined): Tokens.Link => {
    const href = to === undefined ? objectUrl(path, `commit/${from}`) : objectUrl(path, `compare/${from}...${to}`);
    const shown = to === undefined ? shortSha(from) : `${shortSha(from)}...${shortSha(to)}`;
    const prefix = relativePath(path);
    const children: Token[] = [];
    if (prefix !== '') children.push(textToken(`${prefix}@`));
    children.push(github ? codeToken(shown) : textToken(shown));
    return link(raw, href, children);
  };

  // A pasted URL of an issue, PR, MR or commit on this forge, in the short
  // form the forge shows; null for every other URL.
  const shortenedUrl = (raw: string, href: string): Tokens.Link | null => {
    let url: URL;
    try {
      url = new URL(href);
    } catch {
      return null;
    }
    if ((url.protocol !== 'https:' && url.protocol !== 'http:') || url.host.toLowerCase() !== host || url.search !== '') return null;
    const path = url.pathname;
    if (github) {
      const issue = /^\/([^/]+\/[^/]+)\/(?:issues|pull)\/([1-9]\d*)$/.exec(path);
      if (issue) {
        if (url.hash !== '' && !/^#issuecomment-\d+$/.test(url.hash)) return null;
        const shown = `${relativePath(issue[1])}#${issue[2]}${url.hash === '' ? '' : ' (comment)'}`;
        return link(raw, href, [textToken(shown)]);
      }
      if (url.hash !== '') return null;
      const commit = /^\/([^/]+\/[^/]+)\/commit\/([0-9a-f]{7,40})$/.exec(path);
      if (commit) return { ...commitLink(raw, commit[1], commit[2], undefined), href };
      const compare = /^\/([^/]+\/[^/]+)\/compare\/([^/]+?)\.\.\.([^/]+)$/.exec(path);
      if (compare) {
        const prefix = relativePath(compare[1]);
        const children: Token[] = [];
        if (prefix !== '') children.push(textToken(`${prefix}@`));
        children.push(codeToken(`${compare[2]}...${compare[3]}`));
        return link(raw, href, children);
      }
      return null;
    }
    if (url.hash !== '') return null;
    const item = /^\/(.+?)\/-\/(issues|merge_requests)\/([1-9]\d*)(\/diffs)?(\+s?)?$/.exec(path);
    if (item) {
      const kind = item[2] === 'issues' ? '#' : '!';
      const target = item[5] ? href.slice(0, href.length - item[5].length) : href;
      return link(raw, target, [textToken(`${relativePath(item[1])}${kind}${item[3]}${item[4] ? ' (diffs)' : ''}`)]);
    }
    const commit = /^\/(.+?)\/-\/commit\/([0-9a-f]{7,40})$/.exec(path);
    if (commit) return { ...commitLink(raw, commit[1], commit[2], undefined), href };
    return null;
  };

  // A relative href where the forge sends it: GitHub leaves it for the
  // browser to resolve against the page; GitLab resolves a path in the
  // repository and leaves a query to the page. Null for a fragment or an
  // absolute href (a scheme-relative one is already https).
  const resolveRelative = (href: string): string | null => {
    const trimmed = href.trim();
    if (trimmed === '' || trimmed.startsWith('#') || SCHEME.test(trimmed)) return null;
    try {
      if (github || trimmed.startsWith('?')) return new URL(trimmed, pageUrl).href;
      return new URL(trimmed.startsWith('/') ? trimmed.slice(1) : trimmed, blobBase).href;
    } catch {
      return null;
    }
  };

  // A relative link resolved, however it was written: inline, by reference,
  // as an HTML `<a>`. A relative image stays as written: loading repository
  // files is not this page's to do.
  const destination: DestinationHook = (token) => {
    if (token.type !== 'link') return undefined;
    const href = resolveRelative(token.href);
    return href === null ? undefined : { ...token, href };
  };

  // What the tokenizer last saw, so start() can tell whether a window that
  // opens with a word character opens mid-word. The tokenizer runs at every
  // position before the text rule asks start() about the rest.
  let seenLength = -1;
  let seenWordChar = false;

  const prevChar = (tokens: Token[] | TokensList): string => {
    const last = tokens[tokens.length - 1];
    return last ? last.raw.slice(-1) : '';
  };

  const sigilReference = (src: string, tokens: Token[] | TokensList): Tokens.Link | undefined => {
    if (NOT_BEFORE_SIGIL.test(prevChar(tokens))) return undefined;
    const first = src.charCodeAt(0);
    if (first === 35 /* # */ || first === 33 /* ! */) {
      if (first === 33 && github) return undefined;
      const match = (first === 35 ? ISSUE : MERGE_REQUEST).exec(src);
      if (!match || !endsAt(src, match[0].length)) return undefined;
      const suffix = github ? null : GITLAB_SUFFIX.exec(src.slice(match[0].length));
      const raw = match[0] + (suffix?.[0] ?? '');
      return issueLink(raw, project, first === 35 ? '#' : '!', match[1]);
    }
    return mention(src);
  };

  const mention = (src: string): Tokens.Link | undefined => {
    if (github) {
      const match = GITHUB_MENTION.exec(src);
      if (!match) return undefined;
      const end = match[0].length;
      if (end < src.length && /[A-Za-z0-9_/-]/.test(src[end])) return undefined;
      if (match[2] === undefined) return link(match[0], `${origin}/${match[1]}`, [textToken(match[0])]);
      // A team is linked for the repository's own organization only.
      if (match[1].toLowerCase() !== pr.namespace.toLowerCase()) return undefined;
      return link(match[0], `${origin}/orgs/${match[1]}/teams/${match[2]}`, [textToken(match[0])]);
    }
    const match = GITLAB_MENTION.exec(src);
    if (!match) return undefined;
    // A sentence's full stop or dash is not part of the name.
    const name = match[1].replace(/[.-]+$/, '');
    if (name === '') return undefined;
    const raw = `@${name}`;
    return link(raw, `${origin}/${name}`, [textToken(raw)]);
  };

  // `html`: a text run of HTML, where an email address is text and GitLab
  // links a URL of every scheme (the engine's own are not there to).
  const wordReference = (src: string, tokens: Token[] | TokensList, urlEnd: (run: string) => string, html: boolean): Tokens.Link | undefined => {
    const before = prevChar(tokens);
    // An email address opens after a scheme-less `:` too (`contact:a@b.c`).
    const email = html || NOT_BEFORE_EMAIL.test(before) ? null : EMAIL.exec(src);
    if (email && before === ':') return emailLink(email[0]);
    if (NOT_BEFORE_WORD.test(before)) return undefined;
    const first = src.charCodeAt(0);
    if (!html && (first === 109 /* m */ || first === 120 /* x */)) {
      const scheme = SCHEME_EMAIL.exec(src);
      if (scheme && !/[-_]$/.test(scheme[0])) return link(scheme[0], scheme[0], [textToken(scheme[0])]);
    }
    if (github && first === 71 /* G */) {
      const match = GH_ISSUE.exec(src);
      if (match && endsAt(src, match[0].length)) return issueLink(match[0], project, '#', match[1], match[0]);
    }
    if (!github) {
      const url = SCHEME_URL.exec(src);
      if (url) {
        const engine = ENGINE_SCHEMES.test(url[1]);
        if ((engine && !html) || !isOpenableScheme(url[1].toLowerCase())) return undefined;
        // GitLab's filter for HTML text ends a URL at `>`.
        const raw = urlEnd(html ? url[0].split('>', 1)[0] : url[0]);
        return (engine ? shortenedUrl(raw, raw) : null) ?? link(raw, raw, [textToken(raw)]);
      }
    }
    if (email) return emailLink(email[0]);
    const path = PATH_REFERENCE.exec(src);
    if (path) return pathReference(src, path[1], path[2], path[0].length);
    const commits = COMMITS.exec(src);
    if (!commits || !shaEndsAt(src, commits[0].length)) return undefined;
    if (!commitShaped(commits[1]) || (commits[2] !== undefined && !commitShaped(commits[2]))) return undefined;
    return commitLink(commits[0], project, commits[1], commits[2]);
  };

  // GFM extended email autolinks; one ending in `-` or `_` is text.
  const emailLink = (address: string): Tokens.Link | undefined =>
    /[-_]$/.test(address) ? undefined : link(address, `mailto:${address}`, [textToken(address)]);

  const pathReference = (src: string, written: string, sigil: string, end: number): Tokens.Link | undefined => {
    if (DOT_SEGMENT.test(written)) return undefined;
    if (sigil === '>') {
      if (github || !written.includes('/') || !endsAt(src, end)) return undefined;
      return link(src.slice(0, end), `${origin}/${written}`, [textToken(src.slice(0, end))]);
    }
    if (sigil === '@') {
      // GitHub's `user@sha` names the user's fork of this repository.
      const path = github && !written.includes('/') ? `${written}/${pr.repo}` : referencedPath(written);
      const commits = path === null ? null : COMMITS.exec(src.slice(end));
      if (path === null || !commits || !shaEndsAt(src, end + commits[0].length)) return undefined;
      return commitLink(src.slice(0, end + commits[0].length), path, commits[1], commits[2]);
    }
    if (sigil === '!' && github) return undefined;
    const path = referencedPath(written);
    const number = (sigil === '#' ? ISSUE : MERGE_REQUEST).exec(src.slice(end - 1));
    if (path === null || !number || !endsAt(src, end - 1 + number[0].length)) return undefined;
    const after = end - 1 + number[0].length;
    const suffix = github ? null : GITLAB_SUFFIX.exec(src.slice(after));
    return issueLink(src.slice(0, after + (suffix?.[0].length ?? 0)), path, sigil as '#' | '!', number[1]);
  };

  const extension: Extension = {
    name: 'forgeReference',
    level: 'inline',
    start(src) {
      START.lastIndex = 0;
      let match = START.exec(src);
      if (match && match.index === 0 && match[0].length > 1 && WORD.test(src[0])
        && seenLength === src.length + 1 && seenWordChar) {
        // The window opens mid-word: the word is not a reference start.
        START.lastIndex = 1;
        match = START.exec(src);
      }
      if (!match) return undefined;
      if (match[0].length === 1 || match[0] === '[[' || WORD.test(match[0][0])) return match.index;
      return match.index + 1;
    },
    tokenizer(src, tokens) {
      seenLength = src.length;
      seenWordChar = WORD.test(src[0] ?? '');
      const first = src.charCodeAt(0);
      const sigil = first === 35 /* # */ || first === 64 /* @ */ || first === 33 /* ! */;
      const word = first < 128 && WORD_CHAR.test(src[0]);
      if (!sigil && !word && first !== 91 /* [ */ && first !== 60 /* < */) return undefined;
      const state = this.lexer.state;
      if (state.inLink || state.inLinkText) return undefined;
      if (sigil) return sigilReference(src, tokens);
      // HTML text holds no Markdown link or autolink, and on GitHub no bare
      // URL is linked there.
      const html = state.inHtmlText;
      if (html && first === 60) return undefined;
      if (first === 91) {
        if (!github) {
          const wiki = WIKI.exec(src);
          if (wiki) {
            const target = (wiki[2] ?? wiki[1]).trim();
            // A marker alone in its paragraph is the table of contents
            // (documentScope.ts); in a sentence GitLab links it as a
            // relative path, not a wiki page.
            const href = /^_toc_$/i.test(target) ? resolveRelative(target) : `${projectUrl}/-/wikis/${encodeURI(target.replace(/ /g, '-'))}`;
            return href === null ? undefined : link(wiki[0], href, [textToken(wiki[1])]);
          }
        }
        return undefined;
      }
      if (first === 60) {
        const email = ANGLE_EMAIL.exec(src);
        if (email) return link(email[0], `mailto:${email[1]}`, [textToken(email[1])]);
        const auto = this.lexer.tokenizer.autolink(src);
        return auto ? (shortenedUrl(auto.raw, auto.href) ?? undefined) : undefined;
      }
      if (!html && (first === 104 /* h */ || first === 72 /* H */)) {
        const url = this.lexer.tokenizer.url(src);
        if (url) return shortenedUrl(url.raw, url.href) ?? undefined;
      }
      return wordReference(src, tokens, (run) => this.lexer.tokenizer.urlEnd(run), html);
    },
    destination,
  };
  return extension;
}
