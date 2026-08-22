// Raw-JSON assistant messages rendered as a formatted `json` code block.
//
// A session running under an output schema (every workflow phase, on
// both providers) answers each turn with one JSON document, typically a
// single line tens of KB long. Fed to the markdown pipeline as prose,
// that line is a paragraph that never closes: every reveal tick re-lexes
// it, and the `_` and backtick characters inside its string values pair
// up as they arrive, retroactively restyling the stretch between them
// (prose ↔ italic ↔ inline-code slab, with font and wrap changes). On a
// 22KB envelope that measured as 16 ticks out of 150 restyling text the
// user had already read, up to 5KB in one tick — a whole-screen flash at
// the tail of the stream (incident 2026-08-22).
//
// The fix is to never let that text reach the prose path. The message is
// wrapped as a ```json fence and PRETTY-PRINTED by the state machine
// below, whose one load-bearing property is prefix stability: the output
// for any prefix of the input is a prefix of the output for the whole
// input. Nothing already rendered can change shape when more arrives, so
// the code host's incremental line rendering stays incremental and the
// restyle class is gone by construction, not by tuning.
//
// Detection is a shape test on the first two significant characters
// (`{"`, `{}`, `[{`, `["`, `[[`, `[]`, or a lone opener while the first
// chunk is still arriving). Prose never starts that way, fenced JSON starts
// with backticks, and a schema session's answer always does — so there
// is no wire flag: one would have had to be gated on this same test
// anyway, because Codex emits prose progress notes mid-turn in the same
// session.

const INDENT = '  ';

/** Whether a message body is an unfenced JSON document (or the start of
 * one). Cheap: reads leading whitespace and at most two characters. */
export function isRawJsonSource(source: string): boolean {
  let i = 0;
  const n = source.length;
  while (i < n && isWs(source.charCodeAt(i))) i += 1;
  if (i === n) return false;
  const open = source[i];
  if (open !== '{' && open !== '[') return false;
  i += 1;
  while (i < n && isWs(source.charCodeAt(i))) i += 1;
  // A lone opener is the first tick of a JSON stream; committing to the
  // fence now means the block never flips from prose later.
  if (i === n) return true;
  const next = source[i];
  if (open === '{') return next === '"' || next === '}';
  return next === '"' || next === '{' || next === '[' || next === ']';
}

function isWs(code: number): boolean {
  return code === 0x20 || code === 0x0a || code === 0x0d || code === 0x09;
}

/**
 * Prefix-stable pretty printer over a growing JSON source. One instance
 * per streaming row: `render` resumes from the previous call when the
 * new source extends it (the reveal drain appends), so the per-tick cost
 * is the delta, not the document.
 *
 * Formatting rules (all decided left to right, which is what makes
 * output a pure prefix function of input):
 *   - whitespace outside strings is dropped and re-derived;
 *   - `{` / `[` emit the opener and DEFER the newline+indent until the
 *     next significant character, so `{}` stays on one line without
 *     the printer having to look ahead;
 *   - `,` emits `,\n` + indent, `:` emits `: `, closers emit
 *     `\n` + indent + closer;
 *   - strings are copied verbatim, escapes included.
 *
 * Once the root value closes, the fence is closed and everything after
 * it passes through untouched so a model that appends prose after its
 * JSON still gets that prose rendered as markdown. A source that never
 * closes its root (truncated output) gets the fence closed when
 * `streaming` ends.
 */
export class RawJsonFenceFormatter {
  private prev = '';
  private out = '';
  private depth = 0;
  private inString = false;
  private escape = false;
  private pendingOpen = false;
  private rootClosed = false;
  private isJson = false;

  /** The markdown source ChatMarkdown should render for `source`. Non-JSON
   * sources come back unchanged (same string identity). Idempotent for a
   * given (source, streaming) pair, so it is safe inside a `$derived`. */
  render(source: string, streaming: boolean): string {
    if (!isRawJsonSource(source)) {
      if (this.isJson) this.reset();
      return source;
    }
    if (!this.isJson || source.length < this.prev.length || !source.startsWith(this.prev)) {
      this.reset();
      this.isJson = true;
    }
    this.consume(source, this.prev.length);
    this.prev = source;
    const body = '```json\n' + this.out;
    if (this.rootClosed || streaming) return body;
    return body + '\n```';
  }

  private reset(): void {
    this.prev = '';
    this.out = '';
    this.depth = 0;
    this.inString = false;
    this.escape = false;
    this.pendingOpen = false;
    this.rootClosed = false;
    this.isJson = false;
  }

  private consume(source: string, from: number): void {
    let out = '';
    const n = source.length;
    for (let i = from; i < n; i += 1) {
      if (this.rootClosed) {
        out += source.slice(i);
        break;
      }
      const ch = source[i];
      if (this.inString) {
        out += ch;
        if (this.escape) this.escape = false;
        else if (ch === '\\') this.escape = true;
        else if (ch === '"') this.inString = false;
        continue;
      }
      if (isWs(source.charCodeAt(i))) continue;
      if (this.pendingOpen) {
        this.pendingOpen = false;
        if (ch === '}' || ch === ']') {
          this.depth -= 1;
          out += ch;
          if (this.depth === 0) out += this.closeRoot();
          continue;
        }
        out += '\n' + INDENT.repeat(this.depth);
      }
      switch (ch) {
        case '{':
        case '[':
          out += ch;
          this.depth += 1;
          this.pendingOpen = true;
          break;
        case '}':
        case ']':
          this.depth = Math.max(0, this.depth - 1);
          out += '\n' + INDENT.repeat(this.depth) + ch;
          if (this.depth === 0) out += this.closeRoot();
          break;
        case ',':
          out += ',\n' + INDENT.repeat(this.depth);
          break;
        case ':':
          out += ': ';
          break;
        case '"':
          this.inString = true;
          out += ch;
          break;
        default:
          out += ch;
      }
    }
    this.out += out;
  }

  private closeRoot(): string {
    this.rootClosed = true;
    return '\n```';
  }
}
