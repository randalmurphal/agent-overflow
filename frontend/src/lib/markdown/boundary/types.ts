// Slim type subset for boundary detection only.
//
// Vendored from incremark/packages/core/src/types — only the shapes
// BoundaryDetector / detector.ts touch. The upstream module also
// exports ParsedBlock, ParserOptions, etc., which depend on
// mdast/micromark and aren't needed for boundary splitting.
//
// See ./LICENSE for the incremark MIT license.

export interface BlockContext {
  readonly inFencedCode: boolean;
  readonly fenceChar?: string;
  readonly fenceLength?: number;
  readonly listDepth: number;
  readonly blockquoteDepth: number;
  readonly inContainer: boolean;
  readonly containerMarkerLength?: number;
  readonly containerName?: string;
  readonly containerDepth: number;
  readonly inList: boolean;
  readonly listOrdered?: boolean;
  readonly listIndent?: number;
  readonly listMayEnd?: boolean;
  readonly inFootnote?: boolean;
  readonly footnoteIdentifier?: string;
}

export interface ContainerConfig {
  marker?: string;
  minMarkerLength?: number;
  allowedNames?: string[];
}

export interface ContainerMatch {
  name: string;
  markerLength: number;
  isEnd: boolean;
}
