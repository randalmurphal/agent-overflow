// Suggested-change fences in a comment body, for the review pane's read
// and its editors' preview: where each one is, so its block renders as
// the diff it proposes with the markdown around it, and the text the
// `Suggest change` control inserts. The grammar is the backend's
// (`internal/mdsuggest/parse.go`, which decides what the forge's comment
// holds): a fence of three or more backticks or tildes indented at most
// three columns with the info string `suggestion` or GitLab's
// `suggestion:-N+M`, closed by a line of the same character at least as
// long carrying nothing else, at the top level of the body (one inside a
// blockquote or a list item is quoted or nested text), and never an
// unclosed one. CRLF bodies read as LF. Keep the two in step: the tests
// beside each hold the same cases.

/** One piece of a comment body: markdown, or a suggestion block in body
 * order (`index`, as `CommentSuggestion.index` counts them). */
export type SuggestionSegment =
  | { readonly kind: 'markdown'; readonly text: string }
  | { readonly kind: 'suggestion'; readonly index: number; readonly lines: readonly string[]; readonly above: number; readonly below: number };

const HEADER_MAX = 100;
const FENCE_OPEN = /^(`{3,}|~{3,})(.*)$/;
const FENCE_CLOSE = /^(`{3,}|~{3,})[ \t]*$/;
const LIST_MARKER = /^([-+*]|\d{1,9}[.)])( +|$)/;
const WIDENED = /^suggestion:-(\d+)\+(\d+)$/;

interface OpenFence {
  char: string;
  length: number;
  indent: number;
  suggestion: boolean;
  above: number;
  below: number;
  /** The opener's line. */
  start: number;
  lines: string[];
}

function leadingColumns(line: string): [number, string] {
  let col = 0;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (ch === ' ') col += 1;
    else if (ch === '\t') col += 4 - (col % 4);
    else return [col, line.slice(i)];
  }
  return [col, ''];
}

function stripColumns(line: string, n: number): string {
  let i = 0;
  while (i < line.length && i < n && line[i] === ' ') i += 1;
  return line.slice(i);
}

function openFence(rest: string, indent: number, topLevel: boolean, start: number): OpenFence | null {
  const match = FENCE_OPEN.exec(rest);
  if (!match) return null;
  const marker = match[1];
  const info = match[2].trim();
  if (marker[0] === '`' && info.includes('`')) return null;
  const fence: OpenFence = { char: marker[0], length: marker.length, indent, suggestion: false, above: 0, below: 0, start, lines: [] };
  if (!topLevel) return fence;
  const word = info.split(' ')[0];
  if (word === 'suggestion') {
    fence.suggestion = true;
  } else {
    const widened = WIDENED.exec(word);
    if (widened) {
      const above = Number(widened[1]);
      const below = Number(widened[2]);
      if (above <= HEADER_MAX && below <= HEADER_MAX) {
        fence.suggestion = true;
        fence.above = above;
        fence.below = below;
      }
    }
  }
  return fence;
}

function fenceCloses(rest: string, fence: OpenFence): boolean {
  const match = FENCE_CLOSE.exec(rest);
  return match !== null && match[1][0] === fence.char && match[1].length >= fence.length;
}

/** The body split at its suggestion blocks: markdown between them (none
 * empty) and each block's replacement lines. A body with none is one
 * markdown segment. */
export function suggestionSegments(body: string): SuggestionSegment[] {
  const raw = body.split('\n');
  const blocks: { start: number; end: number; lines: string[]; above: number; below: number }[] = [];
  let open: OpenFence | null = null;
  let listContent = -1;
  for (let at = 0; at < raw.length; at++) {
    const line = raw[at].endsWith('\r') ? raw[at].slice(0, -1) : raw[at];
    let [indent, rest] = leadingColumns(line);
    if (open) {
      const rel = indent - open.indent;
      if (rel >= 0 && rel <= 3 && fenceCloses(rest, open)) {
        if (open.suggestion) blocks.push({ start: open.start, end: at, lines: open.lines, above: open.above, below: open.below });
        open = null;
        continue;
      }
      if (open.suggestion) open.lines.push(stripColumns(line, open.indent));
      continue;
    }
    if (line.trim() === '') continue;
    if (listContent >= 0 && indent < listContent && !LIST_MARKER.test(rest)) listContent = -1;
    const marker = indent <= 3 ? LIST_MARKER.exec(rest) : null;
    if (marker) {
      listContent = indent + marker[0].length + (marker[2] === '' ? 1 : 0);
      [indent, rest] = leadingColumns(rest.slice(marker[0].length));
      open = openFence(rest, indent + listContent, false, at);
      continue;
    }
    if (indent <= 3 && rest.startsWith('>')) continue;
    const inList = listContent >= 0 && indent >= listContent;
    if (indent - (inList ? listContent : 0) > 3) continue;
    open = openFence(rest, indent, !inList, at);
  }
  if (blocks.length === 0) return [{ kind: 'markdown', text: body }];
  const out: SuggestionSegment[] = [];
  let from = 0;
  blocks.forEach((block, index) => {
    const text = raw.slice(from, block.start).join('\n');
    if (text.trim() !== '') out.push({ kind: 'markdown', text });
    out.push({ kind: 'suggestion', index, lines: block.lines, above: block.above, below: block.below });
    from = block.end + 1;
  });
  const tail = raw.slice(from).join('\n');
  if (tail.trim() !== '') out.push({ kind: 'markdown', text: tail });
  return out;
}

/** A backtick fence longer than any backtick run in `lines`, so a line
 * that is itself a fence stays content. */
export function suggestionFence(lines: readonly string[]): string {
  let longest = 0;
  for (const line of lines) {
    for (const run of line.matchAll(/`+/g)) longest = Math.max(longest, run[0].length);
  }
  return '`'.repeat(Math.max(3, longest + 1));
}

/** `body` with a suggestion block holding `lines` inserted at `caret`, on
 * lines of its own, and the caret at the end of the block's content. */
export function insertSuggestion(body: string, caret: number, lines: readonly string[]): { body: string; caret: number } {
  const at = Math.max(0, Math.min(caret, body.length));
  const before = body.slice(0, at);
  const after = body.slice(at);
  const fence = suggestionFence(lines);
  const opening = `${before === '' || before.endsWith('\n') ? '' : '\n'}${fence}suggestion\n`;
  const content = lines.join('\n');
  const closing = `\n${fence}${after === '' || after.startsWith('\n') ? '' : '\n'}`;
  return { body: `${before}${opening}${content}${closing}${after}`, caret: before.length + opening.length + content.length };
}

/** The body's text as it renders around its suggestion blocks, which
 * render as diffs find does not read (they carry `data-find-skip`). */
export function bodyWithoutSuggestions(body: string): string {
  const segments = suggestionSegments(body);
  if (segments.length === 1 && segments[0].kind === 'markdown') return body;
  return segments.flatMap((segment) => (segment.kind === 'markdown' ? [segment.text] : [])).join('\n\n');
}
