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
