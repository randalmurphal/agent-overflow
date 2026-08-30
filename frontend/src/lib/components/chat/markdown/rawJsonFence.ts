import {
  createProvenAppend,
  matchesProvenAppend,
  type ProvenAppend,
} from 'svelte-streamdown';

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

type RawJsonPrefixState = 'pending' | 'tentative' | 'json' | 'prose';

class RawJsonPrefixClassifier {
  private state: RawJsonPrefixState = 'pending';
  private opener = 0;

  get isRawJson(): boolean {
    return this.state === 'tentative' || this.state === 'json';
  }

  push(chunk: string): void {
    if (this.state === 'json' || this.state === 'prose') return;
    for (let index = 0; index < chunk.length; index++) {
      const code = chunk.charCodeAt(index);
      if (isWs(code)) continue;
      if (this.opener === 0) {
        if (code !== 0x7b && code !== 0x5b) {
          this.state = 'prose';
          return;
        }
        this.opener = code;
        this.state = 'tentative';
        continue;
      }
      this.state = isValidSecondSignificantCodeUnit(this.opener, code)
        ? 'json'
        : 'prose';
      return;
    }
  }

  reset(): void {
    this.state = 'pending';
    this.opener = 0;
  }
}

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

function isValidSecondSignificantCodeUnit(open: number, next: number): boolean {
  return open === 0x7b
    ? next === 0x22 || next === 0x7d
    : next === 0x22 || next === 0x7b || next === 0x5b || next === 0x5d;
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
  private readonly classifier = new RawJsonPrefixClassifier();
  private prev = '';
  private out = '';
  private depth = 0;
  private inString = false;
  private escape = false;
  private pendingOpen = false;
  private rootClosed = false;
  private rootTailStarted = false;
  private isJson = false;
  private lastRenderAppended = false;
  private lastRendered = '';
  private previousStreaming = false;
  private lastOutputAppend: ProvenAppend | undefined;

  get sourceIsRawJson(): boolean {
    return this.classifier.isRawJson;
  }

  get outputAppend(): ProvenAppend | undefined {
    return this.lastOutputAppend;
  }

  /** The markdown source ChatMarkdown should render for `source`. Non-JSON
   * sources come back unchanged (same string identity). Idempotent for a
   * given (source, streaming) pair, so it is safe inside a `$derived`. */
  render(source: string, streaming: boolean, sourceAppend?: ProvenAppend): string {
    if (source === this.prev) {
      this.lastRenderAppended = false;
      return this.finish(
        this.isJson ? this.renderFence(streaming) : source,
        streaming,
      );
    }

    const previousSourceLength = this.prev.length;
    let chunk: string;
    const appendProven = matchesProvenAppend(sourceAppend, this.prev, source);
    if (appendProven && sourceAppend) {
      chunk = sourceAppend.delta;
      this.lastRenderAppended = true;
    } else if (
      source.length > this.prev.length &&
      source.startsWith(this.prev)
    ) {
      chunk = source.slice(this.prev.length);
      this.lastRenderAppended = true;
    } else {
      this.reset();
      chunk = source;
      this.lastRenderAppended = false;
    }

    const wasJson = this.isJson;
    const wasRootClosed = this.rootClosed;
    this.classifier.push(chunk);
    this.isJson = this.classifier.isRawJson;
    if (!this.isJson) {
      if (wasJson) this.clearFormatting();
      this.prev = source;
      return this.finish(
        source,
        streaming,
        this.lastRenderAppended && !wasJson && !appendProven ? chunk : undefined,
        appendProven && !wasJson ? sourceAppend : undefined,
      );
    }

    const formattedDelta = this.consume(chunk);
    this.prev = source;
    const rendered = this.renderFence(streaming);
    const outputStayedAppendOnly = wasRootClosed ||
      (this.previousStreaming && (streaming || this.rootClosed));
    const outputDelta = this.lastRenderAppended
      ? wasJson
        ? outputStayedAppendOnly ? formattedDelta : undefined
        : previousSourceLength === 0
          ? rendered
          : undefined
      : undefined;
    return this.finish(rendered, streaming, outputDelta);
  }

  private reset(): void {
    this.classifier.reset();
    this.prev = '';
    this.isJson = false;
    this.lastRenderAppended = false;
    this.lastOutputAppend = undefined;
    this.clearFormatting();
  }

  private clearFormatting(): void {
    this.out = '';
    this.depth = 0;
    this.inString = false;
    this.escape = false;
    this.pendingOpen = false;
    this.rootClosed = false;
    this.rootTailStarted = false;
  }

  private renderFence(streaming: boolean): string {
    const body = '```json\n' + this.out;
    return this.rootClosed || streaming ? body : body + '\n```';
  }

  private finish(
    rendered: string,
    streaming: boolean,
    outputDelta?: string,
    outputAppend?: ProvenAppend,
  ): string {
    this.lastOutputAppend = undefined;
    if (
      outputAppend &&
      matchesProvenAppend(outputAppend, this.lastRendered, rendered)
    ) {
      this.lastOutputAppend = outputAppend;
      rendered = outputAppend.next;
    } else if (outputDelta !== undefined) {
      const append = createProvenAppend(this.lastRendered, outputDelta);
      // Formatting is prefix-stable by construction and `outputDelta` is the
      // only mutation applied above. Make that append the canonical returned
      // string rather than rebuilding and comparing the growing document.
      // The randomized prefix/final differential tests guard this invariant.
      this.lastOutputAppend = append;
      rendered = append.next;
    }
    this.lastRendered = rendered;
    this.previousStreaming = streaming;
    return rendered;
  }

  private consume(source: string): string {
    let out = '';
    const n = source.length;
    for (let i = 0; i < n; i += 1) {
      if (this.rootClosed) {
        const tail = source.slice(i);
        if (!this.rootTailStarted) {
          this.rootTailStarted = true;
          // A fence closer is valid only when nothing follows its marker on
          // that physical line. Preserve a source-owned line break. Insert
          // one when trailing prose begins inline so the promised markdown
          // tail cannot turn the closer back into code content.
          const first = tail.charCodeAt(0);
          if (first !== 0x0a && first !== 0x0d) out += '\n';
        }
        out += tail;
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
    return out;
  }

  private closeRoot(): string {
    this.rootClosed = true;
    return '\n```';
  }
}
