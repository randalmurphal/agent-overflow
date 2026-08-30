/* eslint-disable no-use-before-define */
/**
 * The token shapes the lexer emits.
 *
 * Absorbed from marked 16.4.2 (`src/Tokens.ts`, MIT — see `../../LICENSE`),
 * trimmed to what this tree reaches: the parser/renderer-only `Checkbox` and
 * `TableRow` helper shapes are gone with the rest of marked's output half.
 */

export type MarkedToken = (
  Tokens.Blockquote
  | Tokens.Br
  | Tokens.Code
  | Tokens.Codespan
  | Tokens.Def
  | Tokens.Del
  | Tokens.Em
  | Tokens.Escape
  | Tokens.Heading
  | Tokens.Hr
  | Tokens.HTML
  | Tokens.Image
  | Tokens.Link
  | Tokens.List
  | Tokens.ListItem
  | Tokens.Paragraph
  | Tokens.Space
  | Tokens.Strong
  | Tokens.Table
  | Tokens.Tag
  | Tokens.Text
);

export type Token = (
  MarkedToken
  | Tokens.Generic);

export namespace Tokens {
  export interface Blockquote {
    type: 'blockquote';
    raw: string;
    text: string;
    tokens: Token[];
  }

  export interface Br {
    type: 'br';
    raw: string;
  }

  export interface Code {
    type: 'code';
    raw: string;
    codeBlockStyle?: 'indented';
    lang?: string;
    text: string;
    escaped?: boolean;
  }

  export interface Codespan {
    type: 'codespan';
    raw: string;
    text: string;
  }

  export interface Def {
    type: 'def';
    raw: string;
    tag: string;
    href: string;
    title: string;
  }

  export interface Del {
    type: 'del';
    raw: string;
    text: string;
    tokens: Token[];
  }

  export interface Em {
    type: 'em';
    raw: string;
    text: string;
    tokens: Token[];
  }

  export interface Escape {
    type: 'escape';
    raw: string;
    text: string;
  }

  /**
   * An extension may hang any field it likes on a token, so the index
   * signature is marked's declared contract rather than a widening added here.
   */
  export interface Generic {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    [index: string]: any;
    type: string;
    raw: string;
    tokens?: Token[];
  }

  export interface Heading {
    type: 'heading';
    raw: string;
    depth: number;
    text: string;
    tokens: Token[];
  }

  export interface Hr {
    type: 'hr';
    raw: string;
  }

  export interface HTML {
    type: 'html';
    raw: string;
    pre: boolean;
    text: string;
    block: boolean;
  }

  export interface Image {
    type: 'image';
    raw: string;
    href: string;
    title: string | null;
    text: string;
    tokens: Token[];
  }

  export interface Link {
    type: 'link';
    raw: string;
    href: string;
    title?: string | null;
    text: string;
    tokens: Token[];
  }

  export interface List {
    type: 'list';
    raw: string;
    ordered: boolean;
    start: number | '';
    loose: boolean;
    items: ListItem[];
  }

  export interface ListItem {
    type: 'list_item';
    raw: string;
    task: boolean;
    checked?: boolean;
    loose: boolean;
    text: string;
    tokens: Token[];
  }

  export interface Paragraph {
    type: 'paragraph';
    raw: string;
    pre?: boolean;
    text: string;
    tokens: Token[];
  }

  export interface Space {
    type: 'space';
    raw: string;
  }

  export interface Strong {
    type: 'strong';
    raw: string;
    text: string;
    tokens: Token[];
  }

  export interface Table {
    type: 'table';
    raw: string;
    align: Array<'center' | 'left' | 'right' | null>;
    header: TableCell[];
    rows: TableCell[][];
  }

  export interface TableCell {
    text: string;
    tokens: Token[];
    header: boolean;
    align: 'center' | 'left' | 'right' | null;
  }

  export interface Tag {
    type: 'html';
    raw: string;
    inLink: boolean;
    inRawBlock: boolean;
    text: string;
    block: boolean;
  }

  export interface Text {
    type: 'text';
    raw: string;
    text: string;
    tokens?: Token[];
    escaped?: boolean;
  }
}

/** The per-document reference-link table, keyed by lowercased label. */
export type Links = Record<string, Pick<Tokens.Link | Tokens.Image, 'href' | 'title'>>;

export type TokensList = Token[] & {
  links: Links;
};
