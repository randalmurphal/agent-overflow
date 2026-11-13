// The text the `Suggest change` control writes: a suggestion fence whose
// backticks outnumber any run in its lines, so a line that is itself a
// fence stays content. Reading a body's fences is the backend's
// (`internal/mdsuggest`): the read carries each suggestion's place in its
// body and `ParseCommentSuggestions` answers an editor's preview.

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
 * lines of its own, and the caret at the end of the block's content.
 * The body and caret are the text a textarea holds (its line breaks are
 * LF; a CR, alone or before LF, reads back as one LF), so the result is
 * written in LF and its caret lands where the textarea shows it. A
 * line's trailing CR is its line ending, not content, and is dropped. */
export function insertSuggestion(body: string, caret: number, lines: readonly string[]): { body: string; caret: number } {
  const text = body.replace(/\r\n?/g, '\n');
  const at = Math.max(0, Math.min(caret, text.length));
  const before = text.slice(0, at);
  const after = text.slice(at);
  const fence = suggestionFence(lines);
  const opening = `${before === '' || before.endsWith('\n') ? '' : '\n'}${fence}suggestion\n`;
  const content = lines.map((line) => (line.endsWith('\r') ? line.slice(0, -1) : line)).join('\n');
  const closing = `\n${fence}${after === '' || after.startsWith('\n') ? '' : '\n'}`;
  return { body: `${before}${opening}${content}${closing}${after}`, caret: before.length + opening.length + content.length };
}
