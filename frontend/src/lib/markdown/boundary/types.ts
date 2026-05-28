// Slim type subset for boundary detection only.
//
// Vendored from incremark/packages/core/src/types — only the shapes
// BoundaryDetector / detector.ts touch. The upstream module also
// exports ParsedBlock, ParserOptions, etc., which depend on
// mdast/micromark and aren't needed for boundary splitting.
//
// See ./LICENSE for the incremark MIT license.

export interface BlockContext {
  inFencedCode: boolean;
  fenceChar?: string;
  fenceLength?: number;
  listDepth: number;
  blockquoteDepth: number;
  inContainer: boolean;
  containerMarkerLength?: number;
  containerName?: string;
  containerDepth: number;
  inList: boolean;
  listOrdered?: boolean;
  listIndent?: number;
  listMayEnd?: boolean;
  inFootnote?: boolean;
  footnoteIdentifier?: string;
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
