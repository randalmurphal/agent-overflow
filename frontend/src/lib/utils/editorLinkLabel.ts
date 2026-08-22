export function openInEditorLabel(
  target: string,
  line: number | undefined = 0,
  col: number | undefined = 0,
): string {
  const suffix = line && line > 0 ? `:${line}${col && col > 0 ? `:${col}` : ''}` : '';
  const labelWithLocation = suffix && !target.endsWith(suffix) ? `${target}${suffix}` : target;
  return `Open ${labelWithLocation} in editor`;
}
