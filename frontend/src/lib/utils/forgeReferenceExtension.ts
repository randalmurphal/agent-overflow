// Inline extension that links what a forge links in a PR/MR body or comment:
// issue, PR and MR references, mentions, commits and ranges, a pasted forge
// URL in its short form, bare email addresses and GitLab wiki links. Every
// claim is an ordinary `link` token with an absolute forge URL, so both
// render paths, the click path and find need nothing new. Its destination
// hook resolves a relative link where the forge resolves it, however the
// link was written (`DestinationHook`).
//
// A forge links a reference only when its target exists. The pane asks the
// forge (a fact, utils/forgeFacts.ts) for every reference a body can hold
// (`forgeFactKeys`), and the tokenizers read the answer from their block's
// scope (`readFact`): found links where the forge links (a GitHub `#12`
// that is a pull request goes to `/pull/12`) with its title as hover
// text, absent is text as written. Without an answer (asked, unanswered,
// or a surface with no facts) a shape is linked only when it is
// unambiguous: `#12`, `!12`, `owner/repo#12`, `@user`, a 40-hex SHA, and a
// short SHA that mixes digits and letters (an ordinary word or number
// never does). GitLab's label, milestone, snippet and epic references
// (`~bug`, `%v1`, `$5`, `&1`) are also prose, so they show as the forge
// shows them only once it is asked: a label is a chip, the others links.
// `project#1` for another project stays text. A path segment that is `.`
// or `..` names no project, so a path holding one is not a reference.
//
// Each shape's pattern is written once, as a source both the tokenizer
// (anchored at its position) and the key scan (sticky, at each candidate)
// are built from. The scan reads raw text, so it finds every key a lex can
// read and some it never does (inside code): a test holds it to every
// lex.
//
// Nothing links inside link text or a raw `<a>` (`inLinkText`, `inLink`),
// and the engine consumes code spans, code blocks and HTML tags before any
// position inside them reaches this tokenizer.

import type { DestinationHook, Extension, LabelChipToken, Lexer, Token, Tokens, TokensList } from '../markdown';
import { isOpenableScheme, markedStrikethrough, readFact } from '../markdown';
import type { ForgeAttachmentSource } from './forgeAttachments';
import {
  FACT_KEYS_PER_BODY,
  commitFactKey,
  epicFactKey,
  labelFactKey,
  milestoneFactKey,
  mrFactKey,
  refFactKey,
  snippetFactKey,
  userFactKey,
  NO_REVISION,
  type FactRevision,
  type ForgeFact,
} from './forgeFacts';
import { scanPermalinkLines, scanRepositoryImages } from './forgeRepositoryFiles';

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

// Each shape's one source, and the two forms built from it: anchored for
// the tokenizer at its position, sticky for the scan at an offset.
const anchored = (source: string): RegExp => new RegExp(`^${source}`);
const sticky = (source: string): RegExp => new RegExp(source, 'y');

const ISSUE_SOURCE = String.raw`#([1-9]\d*)`;
const MERGE_REQUEST_SOURCE = String.raw`!([1-9]\d*)`;
const GH_ISSUE_SOURCE = String.raw`GH-([1-9]\d*)`;
const GITHUB_MENTION_SOURCE = String.raw`@([A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38})(?:\/([A-Za-z0-9][A-Za-z0-9_-]*))?`;
const GITLAB_MENTION_SOURCE = String.raw`@([A-Za-z0-9_][A-Za-z0-9_.-]*(?:\/[A-Za-z0-9_][A-Za-z0-9_.-]*)*)`;
// GitLab's label: a word of these characters, `::` joining a scoped
// label's parts, or a quoted title.
const LABEL_SOURCE = String.raw`~(?:"([^"\n]+)"|([A-Za-z0-9_?.&-]+(?:::[A-Za-z0-9_?.&-]+)*))`;
// A milestone's title, quoted or one word; an id alone is a milestone by id.
const MILESTONE_SOURCE = String.raw`%(?:"([^"\n]+)"|([A-Za-z0-9_.-]+))`;
const SNIPPET_SOURCE = String.raw`\$([1-9]\d*)`;
const EPIC_SOURCE = String.raw`&([1-9]\d*)`;
/** The characters of a reference's project path, `/` between its segments. */
const PATH_CHARS = 'A-Za-z0-9_.-';

const ISSUE = anchored(ISSUE_SOURCE);
const MERGE_REQUEST = anchored(MERGE_REQUEST_SOURCE);
const GH_ISSUE = anchored(GH_ISSUE_SOURCE);
const GITLAB_SUFFIX = /^\+s?(?![A-Za-z0-9_])/;
const GITHUB_MENTION = anchored(GITHUB_MENTION_SOURCE);
const GITLAB_MENTION = anchored(GITLAB_MENTION_SOURCE);
const LABEL = anchored(LABEL_SOURCE);
const MILESTONE = anchored(MILESTONE_SOURCE);
const SNIPPET = anchored(SNIPPET_SOURCE);
const EPIC = anchored(EPIC_SOURCE);
// GFM extended email autolinks; the domain never ends with `.`.
const EMAIL = /^[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+/;
const SCHEME_EMAIL = /^(?:mailto:[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+|xmpp:[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+(?:\/[A-Za-z0-9@.]+)?)/;
// CommonMark's email autolink, which the engine leaves off for agent chat.
const ANGLE_EMAIL = /^<([A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*)>/;
// `owner/repo#12`, `group/sub/project!12`, `user@sha`, `group/project>`.
const PATH_REFERENCE = new RegExp(`^((?:[${PATH_CHARS}]+\\/)*[${PATH_CHARS}]+)([#!@>])`);
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

/** What one PR's surface needs to read a reference: its forge address and
 * project, and which written project paths a reference may name. */
export interface ForgeShapes {
  readonly github: boolean;
  readonly origin: string;
  readonly host: string;
  /** `namespace/repo`, as the PR names it: the project of every key for it. */
  readonly project: string;
  readonly namespace: string;
  /** The path a reference written with `written` names, or null when the
   * written prefix is not a project this rule may link. */
  referencedPath(written: string): string | null;
}

/** Null when the PR has no forge address: nothing is a reference. */
export function forgeShapes(source: ForgeAttachmentSource): ForgeShapes | null {
  const { pr, webBase } = source;
  const github = pr.forge === 'github';
  const origin = originOf(webBase) ?? originOf(`https://${pr.host}`);
  if (origin === null) return null;
  const project = `${pr.namespace}/${pr.repo}`;
  const repoKey = pr.repo.toLowerCase();
  return {
    github,
    origin,
    host: new URL(origin).host.toLowerCase(),
    project,
    namespace: pr.namespace,
    referencedPath(written) {
      if (DOT_SEGMENT.test(written)) return null;
      const slash = written.includes('/');
      if (github) return slash ? (written.split('/').length === 2 ? written : null) : null;
      if (slash) return written;
      // A bare `project#1` is linked for this project only: another name
      // there is as likely a word (`issue#1`, `pkg!1`).
      return written.toLowerCase() === repoKey ? project : null;
    },
  };
}

/** A label's title as GitLab reads it: an unquoted one never ends in `.` or `?`. */
function labelTitle(match: RegExpExecArray): string {
  return match[1] ?? match[2].replace(/[.?]+$/, '');
}

/** A milestone's title, or null for a milestone by id (`%12`). */
function milestoneTitle(match: RegExpExecArray): string | null {
  if (match[1] !== undefined) return match[1];
  const title = match[2].replace(/[.-]+$/, '');
  return title === '' || /^\d+$/.test(title) ? null : title;
}

/**
 * The references of one PR's surface, as an inline extension. In a text run
 * of HTML (`state.inHtmlText`) it links what the forge links in HTML text:
 * references and mentions, and on GitLab a URL of any scheme and a wiki
 * link. Its identity is the caller's to hold per PR (chatMarkdownOptions.ts).
 */
export function buildForgeReferences(source: ForgeAttachmentSource): Extension {
  const { pr, webBase } = source;
  const shapes = forgeShapes(source);
  // With no forge address there is nothing to link to.
  if (shapes === null) return { name: 'forgeReference', level: 'inline', tokenizer: () => undefined };
  const { github, origin, host, project } = shapes;
  const referencedPath = shapes.referencedPath;
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
  const factLink = factLinkOn(origin);

  // How a reference to `path` reads from this project.
  const relativePath = (path: string): string => {
    if (path.toLowerCase() === projectKey) return '';
    if (!github) {
      const cut = path.lastIndexOf('/');
      if (cut > 0 && path.slice(0, cut).toLowerCase() === pr.namespace.toLowerCase()) return path.slice(cut + 1);
    }
    return path;
  };

  const issueLink = (lexer: Lexer, raw: string, path: string, kind: '#' | '!', number: string, shown?: string): Tokens.Link | Tokens.Text => {
    const target = kind === '!' ? objectUrl(path, `merge_requests/${number}`) : objectUrl(path, `issues/${number}`);
    const key = kind === '!' ? mrFactKey(path, number) : refFactKey(path, number);
    return factLink(lexer, key, raw, target, [textToken(shown ?? `${relativePath(path)}${kind}${number}`)]);
  };

  // A commit of this project hovers its subject when the repository has
  // it. A commit is never absent: the forge may have what the clone lacks.
  const commitLink = (lexer: Lexer | null, raw: string, path: string, from: string, to: string | undefined): Tokens.Link => {
    const href = to === undefined ? objectUrl(path, `commit/${from}`) : objectUrl(path, `compare/${from}...${to}`);
    const shown = to === undefined ? shortSha(from) : `${shortSha(from)}...${shortSha(to)}`;
    const prefix = relativePath(path);
    const children: Token[] = [];
    if (prefix !== '') children.push(textToken(`${prefix}@`));
    children.push(github ? codeToken(shown) : textToken(shown));
    const fact = lexer && to === undefined && prefix === '' ? readFact<ForgeFact>(lexer, commitFactKey(from)) : undefined;
    const title = typeof fact === 'object' && fact.status === 'found' && fact.title ? fact.title : null;
    return { ...link(raw, href, children), title };
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
      if (commit) return { ...commitLink(null, raw, commit[1], commit[2], undefined), href };
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
    if (commit) return { ...commitLink(null, raw, commit[1], commit[2], undefined), href };
    return null;
  };

  // A relative href where the forge sends it: GitHub leaves it for the
  // browser to resolve against the page; GitLab resolves a path in the
  // repository and leaves a query to the page. Null for a fragment, an
  // absolute href (a scheme-relative one is already https), and one the URL
  // parser reads as naming another host (`/\host/x`): relative means on
  // the forge.
  const resolveRelative = (href: string): string | null => {
    const trimmed = href.trim();
    if (trimmed === '' || trimmed.startsWith('#') || SCHEME.test(trimmed)) return null;
    let resolved: URL;
    try {
      resolved = github || trimmed.startsWith('?') ? new URL(trimmed, pageUrl) : new URL(trimmed.startsWith('/') ? trimmed.slice(1) : trimmed, blobBase);
    } catch {
      return null;
    }
    return resolved.origin === origin ? resolved.href : null;
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

  const sigilReference = (lexer: Lexer, src: string, tokens: Token[] | TokensList): Tokens.Link | Tokens.Text | undefined => {
    if (NOT_BEFORE_SIGIL.test(prevChar(tokens))) return undefined;
    const first = src.charCodeAt(0);
    if (first === 35 /* # */ || first === 33 /* ! */) {
      if (first === 33 && github) return undefined;
      const match = (first === 35 ? ISSUE : MERGE_REQUEST).exec(src);
      if (!match || !endsAt(src, match[0].length)) return undefined;
      const suffix = github ? null : GITLAB_SUFFIX.exec(src.slice(match[0].length));
      const raw = match[0] + (suffix?.[0] ?? '');
      return issueLink(lexer, raw, project, first === 35 ? '#' : '!', match[1]);
    }
    return mention(lexer, src);
  };

  const mention = (lexer: Lexer, src: string): Tokens.Link | Tokens.Text | undefined => {
    if (github) {
      const match = GITHUB_MENTION.exec(src);
      if (!match) return undefined;
      const end = match[0].length;
      if (end < src.length && /[A-Za-z0-9_/-]/.test(src[end])) return undefined;
      if (match[2] === undefined) return factLink(lexer, userFactKey(match[1]), match[0], `${origin}/${match[1]}`, [textToken(match[0])]);
      // A team is linked for the repository's own organization only.
      if (match[1].toLowerCase() !== pr.namespace.toLowerCase()) return undefined;
      return link(match[0], `${origin}/orgs/${match[1]}/teams/${match[2]}`, [textToken(match[0])]);
    }
    const match = GITLAB_MENTION.exec(src);
    if (!match) return undefined;
    const name = gitlabMentionName(match);
    if (name === '') return undefined;
    const raw = `@${name}`;
    return factLink(lexer, userFactKey(name), raw, `${origin}/${name}`, [textToken(raw)]);
  };

  // `html`: a text run of HTML, where an email address is text and GitLab
  // links a URL of every scheme (the engine's own are not there to).
  const wordReference = (lexer: Lexer, src: string, tokens: Token[] | TokensList, urlEnd: (run: string) => string, html: boolean): Tokens.Link | Tokens.Text | undefined => {
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
      if (match && endsAt(src, match[0].length)) return issueLink(lexer, match[0], project, '#', match[1], match[0]);
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
    if (path) return pathReference(lexer, src, path[1], path[2], path[0].length);
    const commits = COMMITS.exec(src);
    if (!commits || !shaEndsAt(src, commits[0].length)) return undefined;
    if (!commitShaped(commits[1]) || (commits[2] !== undefined && !commitShaped(commits[2]))) return undefined;
    return commitLink(lexer, commits[0], project, commits[1], commits[2]);
  };

  // GFM extended email autolinks; one ending in `-` or `_` is text.
  const emailLink = (address: string): Tokens.Link | undefined =>
    /[-_]$/.test(address) ? undefined : link(address, `mailto:${address}`, [textToken(address)]);

  const pathReference = (lexer: Lexer, src: string, written: string, sigil: string, end: number): Tokens.Link | Tokens.Text | undefined => {
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
      return commitLink(lexer, src.slice(0, end + commits[0].length), path, commits[1], commits[2]);
    }
    if (sigil === '!' && github) return undefined;
    const path = referencedPath(written);
    const number = (sigil === '#' ? ISSUE : MERGE_REQUEST).exec(src.slice(end - 1));
    if (path === null || !number || !endsAt(src, end - 1 + number[0].length)) return undefined;
    const after = end - 1 + number[0].length;
    const suffix = github ? null : GITLAB_SUFFIX.exec(src.slice(after));
    return issueLink(lexer, src.slice(0, after + (suffix?.[0].length ?? 0)), path, sigil as '#' | '!', number[1]);
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
      if (sigil) return sigilReference(this.lexer, src, tokens);
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
      return wordReference(this.lexer, src, tokens, (run) => this.lexer.tokenizer.urlEnd(run), html);
    },
    destination,
  };
  return extension;
}

/** A GitLab mention's name: a sentence's full stop or dash is not part of it. */
function gitlabMentionName(match: RegExpExecArray): string {
  return match[1].replace(/[.-]+$/, '');
}

const prevChar = (tokens: Token[] | TokensList): string => {
  const last = tokens[tokens.length - 1];
  return last ? last.raw.slice(-1) : '';
};

/** Reads `key`'s fact as a link to `href` or as text: found links where the
 * forge does, with its title as hover text; absent is text as written;
 * anything else (asked, unanswered, no facts) is the shape's link. */
function factLinkOn(origin: string) {
  return (lexer: Lexer | null, key: string, raw: string, href: string, children: Token[]): Tokens.Link | Tokens.Text => {
    const fact = lexer ? readFact<ForgeFact>(lexer, key) : undefined;
    if (typeof fact === 'object') {
      if (fact.status === 'absent') return textToken(raw);
      if (fact.status === 'found') return { ...link(raw, factUrl(fact, origin) ?? href, children), title: fact.title || null };
    }
    return link(raw, href, children);
  };
}

/** A found fact's URL when it is one on the PR's forge. */
function factUrl(fact: ForgeFact, origin: string): string | null {
  return typeof fact.url === 'string' && originOf(fact.url) === origin ? fact.url : null;
}

const GITLAB_SIGIL = /[~%$&]/;

/**
 * GitLab's references that read as prose without the forge's answer:
 * `~label`, `~"multi word"` and scoped `~type::bug` labels, `%milestone`,
 * `$snippet` and `&epic`. A label the forge has, or is being asked about,
 * is a chip (`labelChip`, the same box either way); a milestone, snippet
 * or epic is a link. Anything else is text, as without facts.
 *
 * It runs ahead of the default rules so that it sees `~` before the forge
 * strikethrough, which claims every tilde: a tilde that strikes stays the
 * strikethrough's (GitLab reads Markdown before references), and one that
 * does not is offered to the label. Null on GitHub and without an address.
 */
export function buildForgeGitlabReferences(source: ForgeAttachmentSource): Extension | null {
  if (source.pr.forge !== 'gitlab') return null;
  const shapes = forgeShapes(source);
  if (shapes === null) return null;
  const { origin, project, namespace } = shapes;
  const projectUrl = `${origin}/${project}`;
  const factLink = factLinkOn(origin);
  const strike = markedStrikethrough.tokenizer;

  const label = (lexer: Lexer, src: string): LabelChipToken | undefined => {
    const match = LABEL.exec(src);
    if (!match) return undefined;
    const title = labelTitle(match);
    if (title === '') return undefined;
    const raw = match[1] !== undefined ? match[0] : `~${title}`;
    if (match[1] === undefined && !endsAt(src, raw.length)) return undefined;
    const fact = readFact<ForgeFact>(lexer, labelFactKey(project, title));
    const found = typeof fact === 'object' && fact.status === 'found' ? fact : null;
    if (!found && fact !== 'pending') return undefined;
    const cut = title.lastIndexOf('::');
    const scoped = cut > 0 && cut + 2 < title.length;
    const shape = `${projectUrl}/-/issues?label_name=${encodeURIComponent(title)}`;
    return {
      type: 'labelChip',
      raw,
      text: scoped ? `${title.slice(0, cut)}${title.slice(cut + 2)}` : title,
      scope: scoped ? title.slice(0, cut) : null,
      name: scoped ? title.slice(cut + 2) : title,
      href: (found && factUrl(found, origin)) ?? shape,
      title: null,
      color: found?.color ?? null,
      textColor: found?.textColor ?? null,
    };
  };

  // A milestone, snippet or epic: a link once found or while asked, text
  // otherwise. A pending one links where its shape goes.
  const named = (lexer: Lexer, raw: string, key: string, shape: string): Tokens.Link | Tokens.Text | undefined => {
    const fact = readFact<ForgeFact>(lexer, key);
    if (fact !== 'pending' && (typeof fact !== 'object' || fact.status !== 'found')) return undefined;
    return factLink(lexer, key, raw, shape, [textToken(raw)]);
  };

  return {
    name: 'forgeGitlabReference',
    level: 'inline',
    precedesDefaults: true,
    start(src) {
      const index = src.search(GITLAB_SIGIL);
      return index === -1 ? undefined : index;
    },
    tokenizer(src, tokens) {
      const first = src.charCodeAt(0);
      if (first !== 126 /* ~ */ && first !== 37 /* % */ && first !== 36 /* $ */ && first !== 38 /* & */) return undefined;
      const state = this.lexer.state;
      const struck = first === 126 && strike && !state.inHtmlText ? strike.call(this, src, tokens) : undefined;
      if (struck && struck.type !== 'text') return struck;
      if (state.inLink || state.inLinkText || NOT_BEFORE_SIGIL.test(prevChar(tokens))) return struck;
      const lexer = this.lexer;
      if (first === 126) return label(lexer, src) ?? struck;
      if (first === 37) {
        const match = MILESTONE.exec(src);
        const title = match && milestoneTitle(match);
        if (!match || !title) return undefined;
        const raw = match[1] !== undefined ? match[0] : `%${title}`;
        return named(lexer, raw, milestoneFactKey(project, title), `${projectUrl}/-/milestones?search_title=${encodeURIComponent(title)}`);
      }
      const match = (first === 36 ? SNIPPET : EPIC).exec(src);
      if (!match || !endsAt(src, match[0].length)) return undefined;
      return first === 36
        ? named(lexer, match[0], snippetFactKey(project, match[1]), `${projectUrl}/-/snippets/${match[1]}`)
        : named(lexer, match[0], epicFactKey(namespace, match[1]), `${origin}/groups/${namespace}/-/epics/${match[1]}`);
    },
  };
}

// ---------------------------------------------------------------------------
// The keys a body can read
// ---------------------------------------------------------------------------

const ISSUE_AT = sticky(ISSUE_SOURCE);
const MERGE_REQUEST_AT = sticky(MERGE_REQUEST_SOURCE);
const GH_ISSUE_AT = sticky(GH_ISSUE_SOURCE);
const GITHUB_MENTION_AT = sticky(GITHUB_MENTION_SOURCE);
const GITLAB_MENTION_AT = sticky(GITLAB_MENTION_SOURCE);
const LABEL_AT = sticky(LABEL_SOURCE);
const MILESTONE_AT = sticky(MILESTONE_SOURCE);
const SNIPPET_AT = sticky(SNIPPET_SOURCE);
const EPIC_AT = sticky(EPIC_SOURCE);

const at = (pattern: RegExp, body: string, index: number): RegExpExecArray | null => {
  pattern.lastIndex = index;
  return pattern.exec(body);
};

const isAlnum = (code: number): boolean => (code >= 48 && code <= 57) || (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
const isHex = (code: number): boolean => (code >= 48 && code <= 57) || (code >= 97 && code <= 102);
const PATH_CHAR = new RegExp(`[/${PATH_CHARS}]`);

/** How many of a path's leading `_` and `-` the scan reads past: the
 * openers a lex can start inside (emphasis, an inline diff) are shorter,
 * and a bound keeps a long run of them linear. */
const LEADING_VARIANTS = 8;

/**
 * Adds the key of the word-shaped reference at `index` (a mention, a
 * label, a milestone), and the key of each shorter reading a lex can
 * make: a container's closer (`_@ana_`, `{-~bug-}`) ends the lexed text
 * before the word's trailing `_` or `-`. False once the cap is reached.
 */
function addWord(
  body: string,
  index: number,
  pattern: RegExp,
  anchoredPattern: RegExp,
  keyOf: (match: RegExpExecArray) => string | null,
  add: FactKeySink,
): boolean {
  const match = at(pattern, body, index);
  if (!match) return true;
  const key = keyOf(match);
  if (key !== null && !add(key)) return false;
  const end = index + match[0].length;
  for (let cut = 1; cut <= LEADING_VARIANTS; cut++) {
    const char = body.charCodeAt(end - cut);
    if (char !== 95 /* _ */ && char !== 45 /* - */) break;
    const shorter = anchoredPattern.exec(body.slice(index, end - cut));
    const each = shorter && keyOf(shorter);
    if (each && !add(each)) return false;
  }
  return true;
}

/** Adds a key; false once the body's cap is reached, which ends the scan. */
export type FactKeySink = (key: string) => boolean;

/**
 * One kind's scan of a body's raw text, given the PR's shapes and the
 * frame's revision (`NO_REVISION` without one).
 */
export interface FactScan {
  /** Its keys embed the revision's commits (`image:<head>:<path>`): a body
   * it found a key in is scanned again when the revision moves. A body no
   * such scan found a key in is not. */
  readonly readsHead: boolean;
  readonly scan: (body: string, shapes: ForgeShapes, revision: FactRevision, add: FactKeySink) => void;
}

/** A body's keys, and whether a scan that reads the head found one. */
export interface ScannedKeys {
  readonly keys: readonly string[];
  readonly readsHead: boolean;
}

/**
 * The references' keys: issues, PRs and MRs (bare, `GH-`, by path),
 * mentions, commits, and GitLab's labels, milestones, snippets and epics.
 * One pass: at each sigil the shape's pattern from its position, and the
 * project path written before it, as the tokenizer reads both. A lex can
 * start at a sigil or word only after a character that is no letter or
 * digit (a token's last character, or a container's opener), so one after
 * a letter or digit is skipped; a path that opens with `_` or `-` may be
 * read from after them (emphasis, an inline diff), so each is tried.
 */
const scanReferences = (body: string, shapes: ForgeShapes, _revision: FactRevision, add: FactKeySink): void => {
  const { github, project, namespace } = shapes;
  let pathStart = 0;
  let hexStart = -1;
  const length = body.length;
  for (let index = 0; index <= length; index++) {
    const code = index < length ? body.charCodeAt(index) : -1;
    if (hexStart !== -1 && !isHex(code)) {
      const hex = body.slice(hexStart, index);
      if (hex.length >= 7 && hex.length <= 40 && commitShaped(hex) && !add(commitFactKey(hex))) return;
      hexStart = -1;
    }
    if (code === -1) return;
    if (isHex(code) && hexStart === -1 && (index === 0 || !isAlnum(body.charCodeAt(index - 1)))) hexStart = index;
    const after = index === 0 || !isAlnum(body.charCodeAt(index - 1));
    if (code === 35 /* # */ || (code === 33 /* ! */ && !github)) {
      const match = at(code === 35 ? ISSUE_AT : MERGE_REQUEST_AT, body, index);
      if (match) {
        const keyOf = code === 35 ? refFactKey : mrFactKey;
        if (after && !add(keyOf(project, match[1]))) return;
        // The project written before the sigil, from its start or after
        // each of its first leading `_` or `-` (a container's opener).
        for (let start = pathStart; start < index && start - pathStart <= LEADING_VARIANTS; start++) {
          const char = body.charCodeAt(start);
          if (WORD_CHAR.test(body[start]) && body.charCodeAt(index - 1) !== 47 /* / */) {
            const path = shapes.referencedPath(body.slice(start, index));
            if (path !== null && !add(keyOf(path, match[1]))) return;
          }
          if (char !== 95 /* _ */ && char !== 45 /* - */) break;
        }
      }
    }
    else if (code === 64 /* @ */) {
      if (after) {
        const name = github
          ? (match: RegExpExecArray) => (match[2] === undefined ? userFactKey(match[1]) : null)
          : (match: RegExpExecArray) => (gitlabMentionName(match) !== '' ? userFactKey(gitlabMentionName(match)) : null);
        if (!addWord(body, index, github ? GITHUB_MENTION_AT : GITLAB_MENTION_AT, github ? GITHUB_MENTION : GITLAB_MENTION, name, add)) return;
      }
    }
    else if (code === 71 /* G */ && github && after) {
      const match = at(GH_ISSUE_AT, body, index);
      if (match && !add(refFactKey(project, match[1]))) return;
    }
    else if (!github && after && code === 126 /* ~ */) {
      const label = (match: RegExpExecArray) => {
        const title = labelTitle(match);
        return title ? labelFactKey(project, title) : null;
      };
      if (!addWord(body, index, LABEL_AT, LABEL, label, add)) return;
    }
    else if (!github && after && code === 37 /* % */) {
      const milestone = (match: RegExpExecArray) => {
        const title = milestoneTitle(match);
        return title ? milestoneFactKey(project, title) : null;
      };
      if (!addWord(body, index, MILESTONE_AT, MILESTONE, milestone, add)) return;
    }
    else if (!github && after && (code === 36 /* $ */ || code === 38 /* & */)) {
      const match = at(code === 36 ? SNIPPET_AT : EPIC_AT, body, index);
      if (match && !add(code === 36 ? snippetFactKey(project, match[1]) : epicFactKey(namespace, match[1]))) return;
    }
    if (!PATH_CHAR.test(body[index])) pathStart = index + 1;
  }
};

/** Every kind's scan, in order: references, repository images (keyed by
 * the frame's commits) and permalink lines (forgeRepositoryFiles.ts). */
const FACT_SCANS: FactScan[] = [
  { readsHead: false, scan: scanReferences },
  { readsHead: true, scan: scanRepositoryImages },
  { readsHead: false, scan: scanPermalinkLines },
];

/** Test seam: runs `scan` after the others until the returned call. */
export function __addFactScanForTest(scan: FactScan): () => void {
  FACT_SCANS.push(scan);
  return () => {
    const at = FACT_SCANS.indexOf(scan);
    if (at !== -1) FACT_SCANS.splice(at, 1);
  };
}

/**
 * The fact keys a body's references can read on `source`'s surface at
 * `head`, distinct, at most `FACT_KEYS_PER_BODY`. Read from the raw text,
 * without lexing: it may name keys no lex reads (text in code), never
 * fewer. `forgeFactScanner` holds the PR's shapes for repeated scans.
 */
export function forgeFactKeys(body: string, source: ForgeAttachmentSource, revision: FactRevision = NO_REVISION): readonly string[] {
  return forgeFactScanner(source)(body, revision).keys;
}

/** `forgeFactKeys` for one PR's bodies, with whether they depend on the revision. */
export function forgeFactScanner(source: ForgeAttachmentSource): (body: string, revision: FactRevision) => ScannedKeys {
  const shapes = forgeShapes(source);
  return (body, revision) => {
    if (shapes === null || body === '') return NO_SCAN;
    let keys: Set<string> | null = null;
    let readsHead = false;
    const add: FactKeySink = (key) => {
      (keys ??= new Set()).add(key);
      return keys.size < FACT_KEYS_PER_BODY;
    };
    for (const each of FACT_SCANS) {
      const before = keys === null ? 0 : (keys as Set<string>).size;
      each.scan(body, shapes, revision, add);
      const after = keys === null ? 0 : (keys as Set<string>).size;
      if (each.readsHead && after > before) readsHead = true;
      if (after >= FACT_KEYS_PER_BODY) break;
    }
    return keys === null ? NO_SCAN : { keys: [...(keys as Set<string>)], readsHead };
  };
}

const NO_SCAN: ScannedKeys = Object.freeze({ keys: Object.freeze([]) as readonly string[], readsHead: false });
