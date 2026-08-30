import type { Component, Snippet } from 'svelte';
import type { Theme } from './theme.js';
import type { MermaidConfig } from 'mermaid';
import type { KatexOptions } from 'katex';
import type { ProvenAppend } from './marked/index.js';
export interface StreamdownContext extends Omit<StreamdownProps, keyof Snippets | 'class' | 'theme'> {
    snippets: Snippets;
    theme: Theme;
}
export declare class StreamdownContext {
    footnotes: {
        refs: Map<string, FootnoteRef>;
        footnotes: Map<string, Footnote>;
    };
    pendingAsyncCount: number;
    registerAsyncResource(): () => void;
    staticRetryGeneration: number;
    requestStaticRetry(): void;
    registerStaticRetry(listener: () => void): () => void;
    constructor(props: Omit<StreamdownProps, keyof Snippets | 'class'> & {
        snippets: Snippets;
    });
}
export declare const useStreamdown: () => StreamdownContext;
import type { AlertToken, MathToken, SubSupToken, TableToken, THead, TBody, TFoot, THeadRow, TRow, TD, TH, Extension, GenericToken, CitationToken, MdxToken } from './marked/index.js';
import type { Tokens } from 'marked';
import type { ListItemToken, ListToken } from './marked/marked-list.js';
import type { Footnote, FootnoteRef } from './marked/marked-footnotes.js';
import type { DescriptionDetailToken, DescriptionListToken, DescriptionTermToken, DescriptionToken } from './marked/marked-dl.js';
type TokenSnippet = {
    heading: Tokens.Heading;
    paragraph: Tokens.Paragraph;
    blockquote: Tokens.Blockquote;
    code: Tokens.Code;
    codespan: Tokens.Codespan;
    ul: ListToken;
    ol: ListToken;
    li: ListItemToken;
    table: TableToken;
    thead: THead;
    tbody: TBody;
    tfoot: TFoot;
    tr: THeadRow | TRow;
    td: TD;
    th: TH;
    image: Tokens.Image;
    link: Tokens.Link;
    strong: Tokens.Strong;
    em: Tokens.Em;
    del: Tokens.Del;
    hr: Tokens.Hr;
    br: Tokens.Br;
    math: MathToken;
    alert: AlertToken;
    mermaid: Tokens.Code;
    footnoteRef: FootnoteRef;
    sup: SubSupToken;
    sub: SubSupToken;
    descriptionList: DescriptionListToken;
    description: DescriptionToken;
    descriptionTerm: DescriptionTermToken;
    descriptionDetail: DescriptionDetailToken;
    inlineCitation: CitationToken;
    mdx: MdxToken;
};
type PredefinedElements = keyof TokenSnippet;
export type Snippets = {
    [K in PredefinedElements]?: Snippet<[
        {
            children: Snippet;
            token: TokenSnippet[K];
        } & (K extends 'mdx' ? {
            props: Record<string, number | string | boolean | null | undefined>;
        } : {})
    ]>;
};
export type StreamdownProps = {
    streamdown?: StreamdownContext;
    static?: boolean;
    /** Install source-preserving runtime diagnostics on rendered roots. */
    diagnostics?: boolean;
    /** Render synchronous completed blocks as escaped fixed HTML without Svelte token anchors. */
    compactStaticHtml?: boolean;
    /** Mark the first direct md-blk for host-owned outer-margin trimming. */
    trimFirstBlockMargin?: boolean;
    /** Mark the last direct md-blk for host-owned outer-margin trimming. */
    trimLastBlockMargin?: boolean;
    /** Host-owned serializers for completed custom components. Returning null keeps the component mounted. */
    staticRenderers?: {
        code?: (token: Tokens.Code, id: string, streamdown: StreamdownContext) => string | null;
    };
    /** Host frame coordinator used to amortize completed component retirement. */
    staticWorkScheduler?: {
        request(callback: FrameRequestCallback): number;
        cancel(handle: number): void;
    };
    element?: HTMLElement;
    content: string;
    /** Opaque proof that content extends the previous value. */
    contentAppend?: ProvenAppend;
    class?: string;
    parseIncompleteMarkdown?: boolean;
    /** Host proof that content is an isolated volatile tail, permitting one render lex. */
    isolatedVolatileTail?: boolean;
    defaultOrigin?: string;
    allowedLinkPrefixes?: string[];
    allowedImagePrefixes?: string[];
    theme: Theme;
    mermaidConfig?: MermaidConfig;
    katexConfig?: KatexOptions | ((inline: boolean) => KatexOptions);
    translations?: {
        alert?: {
            note?: string;
            tip?: string;
            warning?: string;
            caution?: string;
            important?: string;
        };
    };
    renderHtml?: boolean | ((token: Tokens.HTML | Tokens.Tag) => string);
    icons?: {
        fullscreen?: Snippet;
        note?: Snippet;
        tip?: Snippet;
        warning?: Snippet;
        caution?: Snippet;
        important?: Snippet;
    };
    extensions?: Extension[];
    children?: Snippet<[{
        streamdown: StreamdownContext;
        token: GenericToken;
        children: Snippet;
    }]>;
    mdxComponents?: Record<string, Component<{
        token: MdxToken;
        children: Snippet;
        props: any;
    }, any, any>>;
    /**
     * Host-owned renderers for the three async element kinds. The library
     * ships none: without a component the block renders its source text.
     */
    components?: {
        code?: Component<{
            token: Tokens.Code;
            id: string;
            textAppend?: ProvenAppend;
        }, any, any>;
        mermaid?: Component<{
            token: Tokens.Code;
            id: string;
        }, any, any>;
        math?: Component<{
            token: MathToken;
            id: string;
        }, any, any>;
    };
    onsettled?: () => void;
} & Partial<Snippets>;
export {};
