import type { PatchFile, PatchLine } from './patchFiles';

const markerPrefixes = ['<<<<<<<', '|||||||', '=======', '>>>>>>>'];

export function conflictPatchFile(path: string, content: string): PatchFile {
  const lines = content.split('\n');
  if (lines.at(-1) === '') lines.pop();
  return {
    path,
    kind: 'conflict',
    additions: 0,
    deletions: 0,
    lines: lines.map(conflictLine),
  };
}

function conflictLine(content: string): PatchLine {
  return {
    content,
    type: markerPrefixes.some((prefix) => content.startsWith(prefix)) ? 'meta' : 'context',
  };
}
