// The rendered lines of a code block. Each line holds its segments in one
// signal, recomputed when its text or colors change, so a streamed append
// re-renders only the lines it touched, and adopting a highlight response
// re-renders only the lines whose colors changed. One array of line texts
// would reconcile every line on each append and recompute every line's
// segments on each highlight response. A long open fence retains one line
// object per source line, so a line holds no other signal or derived.

import { spanSegments, type EncodedLine, type SpanSegment } from '../../../utils/syntaxSpans';

/** The runs `spanSegments` colors with, or undefined for a plain line. */
function colorRuns(line: EncodedLine | null): number[] | undefined {
  const runs = line?.r;
  return runs && runs.length >= 2 ? runs : undefined;
}

/** Whether two span lines have the same color runs. */
function sameSpans(a: EncodedLine | null, b: EncodedLine | null): boolean {
  const x = colorRuns(a);
  const y = colorRuns(b);
  if (x === y) return true;
  if (!x || !y || x.length !== y.length) return false;
  for (let i = 0; i < x.length; i++) {
    if (x[i] !== y[i]) return false;
  }
  return true;
}

class CodeLine {
  #text = '';
  #spans: EncodedLine | null = null;
  // Always spanSegments(#text, #spans); '' has no segments.
  #segments = $state.raw<SpanSegment[]>([]);

  constructor(text: string) {
    this.text = text;
  }

  get segments(): SpanSegment[] {
    return this.#segments;
  }

  get text(): string {
    return this.#text;
  }

  set text(text: string) {
    if (text === this.#text) return;
    this.#text = text;
    this.#segments = spanSegments(text, this.#spans);
  }

  paint(spans: EncodedLine | null): void {
    if (!sameSpans(this.#spans, spans)) this.#segments = spanSegments(this.#text, spans);
    // Equal runs keep the segments but take the new line, so a line never
    // holds an older response alive.
    this.#spans = spans;
  }
}

export class CodeLines {
  lines = $state.raw<CodeLine[]>([]);

  constructor(text: string) {
    this.replace(text);
  }

  /** Extends the source by delta, touching only its last line and the
   * lines delta adds. */
  append(delta: string): void {
    const parts = delta.split('\n');
    const lines = this.lines;
    lines[lines.length - 1].text += parts[0];
    if (parts.length > 1) {
      this.lines = lines.concat(parts.slice(1).map((text) => new CodeLine(text)));
    }
  }

  /** Replaces the source, keeping the lines already rendered. */
  replace(text: string): void {
    const parts = text.split('\n');
    const lines = this.lines;
    const kept = Math.min(parts.length, lines.length);
    for (let i = 0; i < kept; i++) lines[i].text = parts[i];
    if (parts.length < lines.length) {
      this.lines = lines.slice(0, parts.length);
    } else if (parts.length > lines.length) {
      this.lines = lines.concat(parts.slice(kept).map((line) => new CodeLine(line)));
    }
  }

  /** Gives each line the spans spansAt returns for it. */
  paint(spansAt: (index: number) => EncodedLine | null): void {
    const lines = this.lines;
    for (let i = 0; i < lines.length; i++) lines[i].paint(spansAt(i));
  }

  texts(): string[] {
    return this.lines.map((line) => line.text);
  }
}
