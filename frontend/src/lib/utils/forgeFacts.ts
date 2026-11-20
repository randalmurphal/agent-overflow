// Forge facts: what the forge or the PR's repository answers about a name
// a PR/MR body mentions (`#12` exists and is a pull request, `~bug` is red,
// a commit's subject). The wire shape, the keys and their kinds, shared by
// the reference extension that reads facts (forgeReferenceExtension.ts),
// the scan that finds the keys a body can read, and the store that asks
// for them (stores/forgeFacts.svelte.ts). A key is canonical: built here
// only, so the scan and the tokenizers name a fact with the same string.

import type { ForgeFactReader as MarkdownForgeFactReader, ForgeFactState as MarkdownForgeFactState } from '../markdown';

/** One answer, as `ResolveForgeFacts` gives it. Fields are by kind. */
export interface ForgeFact {
  key: string;
  status: 'found' | 'absent' | 'unavailable';
  url?: string;
  title?: string;
  kind?: string;
  state?: string;
  color?: string;
  textColor?: string;
  oid?: string;
  size?: number;
  width?: number;
  height?: number;
  mime?: string;
  lfs?: boolean;
  text?: string;
  path?: string;
  /** The frontend's own fact (`REVISION_FACT_KEY`), never on the wire. */
  revision?: FactRevision;
}

/**
 * What the PR's frame names its head and base: their commit ids and branch
 * names, '' where it names none. A repository file written by branch or
 * relative to the PR resolves to a commit by it (repositoryFiles.ts).
 */
export interface FactRevision {
  readonly head: string;
  readonly base: string;
  readonly headRef: string;
  readonly baseRef: string;
}

export const NO_REVISION: FactRevision = Object.freeze({ head: '', base: '', headRef: '', baseRef: '' });

/**
 * The key a lex reads the frame's revision under, answered by the facts
 * store itself and never asked for: a block that resolves a file by it
 * rebuilds when it changes, and no other block does.
 */
export const REVISION_FACT_KEY = 'revision:';

/** The revision a reader's answer for `REVISION_FACT_KEY` holds. */
export function factRevision(state: ForgeFactState): FactRevision {
  return typeof state === 'object' && state.revision ? state.revision : NO_REVISION;
}

/** `ResolveForgeFacts`' answer: one fact per requested key, same order. */
export interface ForgeFacts {
  facts: ForgeFact[];
}

export type ForgeFactState = MarkdownForgeFactState<ForgeFact>;
export type ForgeFactReader = MarkdownForgeFactReader<ForgeFact>;

/** The keys one body asks for at most; a reference past them renders by shape. */
export const FACT_KEYS_PER_BODY = 256;

export const refFactKey = (project: string, number: string): string => `ref:${project}#${number}`;
export const mrFactKey = (project: string, number: string): string => `mr:${project}!${number}`;
export const userFactKey = (name: string): string => `user:${name}`;
export const labelFactKey = (project: string, title: string): string => `label:${project}~${title}`;
export const milestoneFactKey = (project: string, title: string): string => `milestone:${project}%${title}`;
export const snippetFactKey = (project: string, id: string): string => `snippet:${project}$${id}`;
export const epicFactKey = (group: string, number: string): string => `epic:${group}&${number}`;
export const commitFactKey = (hex: string): string => `commit:${hex}`;
/** A file of the PR's repository at a full commit id, as an image. */
export const imageFactKey = (commit: string, path: string): string => `image:${commit}:${path}`;
/** Lines `start..end` (1 based, inclusive) of a file at a full commit id. */
export const linesFactKey = (sha: string, path: string, start: number, end: number): string => `lines:${sha}:${path}:${start}-${end}`;

/** Kinds the PR's local repository answers, at the pane's loaded
 * revision: asked again when the revision becomes known or its head
 * moves. */
const LOCAL_FACT_KINDS: readonly string[] = ['commit:', 'image:', 'lines:'];

export function isLocalFactKey(key: string): boolean {
  for (const kind of LOCAL_FACT_KINDS) if (key.startsWith(kind)) return true;
  return false;
}

/** A key's kind (`ref`, `commit`), the only part of a key a diagnostic names. */
export function factKind(key: string): string {
  const colon = key.indexOf(':');
  return colon > 0 ? key.slice(0, colon) : 'unknown';
}

/** `state` when it is an answer the forge gave (found or absent). */
export function factAnswer(state: ForgeFactState): ForgeFact | null {
  return typeof state === 'object' && state !== null && state.status !== 'unavailable' ? state : null;
}

/** Whether two answers render alike: the same status and fields. */
export function sameFact(a: ForgeFact, b: ForgeFact): boolean {
  return a.status === b.status && a.url === b.url && a.title === b.title && a.kind === b.kind && a.state === b.state &&
    a.color === b.color && a.textColor === b.textColor && a.oid === b.oid && a.size === b.size && a.width === b.width &&
    a.height === b.height && a.mime === b.mime && a.lfs === b.lfs && a.text === b.text && a.path === b.path;
}
