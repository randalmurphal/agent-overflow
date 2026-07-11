import type { PatchFile } from './patchFiles';

export type ReviewTreeNode = ReviewTreeDirectoryNode | ReviewTreeFileNode;

export interface ReviewTreeDirectoryNode {
  kind: 'directory';
  name: string;
  path: string;
  children: ReviewTreeNode[];
}

export interface ReviewTreeFileNode {
  kind: 'file';
  name: string;
  path: string;
  fileIndex: number;
  /** PatchFile.kind: modified | added | deleted | renamed. */
  fileKind: string;
  additions: number;
  deletions: number;
}

export interface ReviewTreeVisibleNode {
  node: ReviewTreeNode;
  depth: number;
}

interface MutableDirectory {
  kind: 'directory';
  name: string;
  path: string;
  directories: Map<string, MutableDirectory>;
  files: ReviewTreeFileNode[];
}

/**
 * `fileIndexes` maps each position to its index in the UNFILTERED file
 * list (a filtered tree's nodes must still address the original diff for
 * jump + active-file highlight). Omitted, positions are the indexes.
 */
export function buildReviewTree(
  files: readonly PatchFile[],
  fileIndexes?: readonly number[],
): ReviewTreeNode[] {
  const root: MutableDirectory = {
    kind: 'directory',
    name: '',
    path: '',
    directories: new Map(),
    files: [],
  };

  files.forEach((file, position) => {
    const fileIndex = fileIndexes?.[position] ?? position;
    const parts = file.path.split('/').filter(Boolean);
    const fileName = parts.pop() ?? file.path;
    let dir = root;
    for (const part of parts) {
      let child = dir.directories.get(part);
      if (!child) {
        const path = dir.path ? `${dir.path}/${part}` : part;
        child = {
          kind: 'directory',
          name: part,
          path,
          directories: new Map(),
          files: [],
        };
        dir.directories.set(part, child);
      }
      dir = child;
    }
    dir.files.push({
      kind: 'file',
      name: fileName,
      path: file.path,
      fileIndex,
      fileKind: file.kind,
      additions: file.additions,
      deletions: file.deletions,
    });
  });

  return materializeChildren(root);
}

/**
 * Compare two file paths in tree display order: directories before
 * files at every level, alphabetical within each group — the exact
 * order `buildReviewTree` + `flattenReviewTree` render. Sorting the
 * flat file list with this comparator makes the diff body's file
 * sequence match the tree's top-to-bottom reading order.
 */
export function comparePathsTreeOrder(a: string, b: string): number {
  const aParts = a.split('/');
  const bParts = b.split('/');
  const shared = Math.min(aParts.length, bParts.length);
  for (let i = 0; i < shared; i += 1) {
    const aIsDir = i < aParts.length - 1;
    const bIsDir = i < bParts.length - 1;
    if (aIsDir !== bIsDir) return aIsDir ? -1 : 1;
    if (aParts[i] !== bParts[i]) return aParts[i].localeCompare(bParts[i]);
  }
  return aParts.length - bParts.length;
}

/** Copy of `files` sorted into tree display order (input untouched —
 * parse results are shared immutable arrays). */
export function sortFilesTreeOrder(files: readonly PatchFile[]): PatchFile[] {
  return [...files].sort((a, b) => comparePathsTreeOrder(a.path, b.path));
}

/**
 * Type-filter chip label for a file: the extension (`.ts`, `.svelte`),
 * or the whole basename when there is none — for `Makefile` or
 * `.gitignore` the name IS the type.
 */
export function fileExtensionLabel(path: string): string {
  const name = path.split('/').pop() ?? path;
  const dot = name.lastIndexOf('.');
  if (dot <= 0) return name;
  return name.slice(dot);
}

export interface FilteredReviewFiles {
  files: PatchFile[];
  /** Original (unfiltered) index of each kept file, for buildReviewTree. */
  fileIndexes: number[];
}

/** Case-insensitive path substring + extension-chip filter for the tree. */
export function filterReviewFiles(
  files: readonly PatchFile[],
  query: string,
  extensions: ReadonlySet<string>,
): FilteredReviewFiles {
  const needle = query.trim().toLowerCase();
  const out: FilteredReviewFiles = { files: [], fileIndexes: [] };
  files.forEach((file, index) => {
    if (needle && !file.path.toLowerCase().includes(needle)) return;
    if (extensions.size > 0 && !extensions.has(fileExtensionLabel(file.path))) return;
    out.files.push(file);
    out.fileIndexes.push(index);
  });
  return out;
}

export function flattenReviewTree(
  nodes: readonly ReviewTreeNode[],
  collapsedPaths: ReadonlySet<string>,
): ReviewTreeVisibleNode[] {
  const out: ReviewTreeVisibleNode[] = [];
  function visit(node: ReviewTreeNode, depth: number): void {
    out.push({ node, depth });
    if (node.kind !== 'directory' || collapsedPaths.has(node.path)) return;
    for (const child of node.children) visit(child, depth + 1);
  }
  for (const node of nodes) visit(node, 0);
  return out;
}

function materializeChildren(dir: MutableDirectory): ReviewTreeNode[] {
  const directories = Array.from(dir.directories.values(), materializeDirectory);
  const files = [...dir.files].sort((a, b) => a.name.localeCompare(b.name));
  return [...directories.sort((a, b) => a.name.localeCompare(b.name)), ...files];
}

function materializeDirectory(dir: MutableDirectory): ReviewTreeDirectoryNode {
  let node: ReviewTreeDirectoryNode = {
    kind: 'directory',
    name: dir.name,
    path: dir.path,
    children: materializeChildren(dir),
  };

  while (node.children.length === 1 && node.children[0]?.kind === 'directory') {
    const child = node.children[0];
    node = {
      kind: 'directory',
      name: `${node.name}/${child.name}`,
      path: child.path,
      children: child.children,
    };
  }

  return node;
}
