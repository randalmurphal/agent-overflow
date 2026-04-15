export interface DiffLine {
  type: 'added' | 'removed' | 'context' | 'header';
  content: string;
}

/**
 * Classify each line of a unified diff into added, removed, header, or context.
 */
export function parseDiffLines(diffText: string): DiffLine[] {
  if (!diffText) return [];

  return diffText.split('\n').map((line): DiffLine => {
    if (line.startsWith('@@')) return { type: 'header', content: line };
    if (line.startsWith('+') && !line.startsWith('+++')) return { type: 'added', content: line };
    if (line.startsWith('-') && !line.startsWith('---')) return { type: 'removed', content: line };
    return { type: 'context', content: line };
  });
}
