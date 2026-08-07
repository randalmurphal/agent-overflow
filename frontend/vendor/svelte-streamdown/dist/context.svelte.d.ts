import type { Component, Snippet } from 'svelte';
import type { DeepPartialTheme, Theme } from './theme.js';
import type { MermaidConfig } from 'mermaid';
import type { KatexOptions } from 'katex';
import type { LanguageInfo } from './utils/bundledLanguages.js';
import type { ThemeRegistration } from 'shiki';
export interface StreamdownContext extends Omit<StreamdownProps, keyof Snippets | 'class' | 'theme' | 'shikiTheme' | 'inlineCitationsMode'> {
    snippets: Snippets;
    shikiTheme: string;
    theme: Theme;
    controls: {
        code: boolean;
        mermaid: boolean;
        mermaidMouseWheelZoom: boolean;
        table: boolean;
    };
    inlineCitationsMode: 'list' | 'carousel';
    animation: {
        enabled: boolean;
    } & StreamdownProps['animation'];
}
export declare class StreamdownContext<Source extends Record<string, any> = Record<string, any>> {
    footnotes: {
        refs: Map<string, FootnoteRef>;
        footnotes: Map<string, Footnote>;
    };
    isMounted: boolean;
    pendingAsyncCount: number;
    registerAsyncResource(): () => void;
    get animationTextStyle(): string | undefined;
    get animationBlockStyle(): string | undefined;
    constructor(props: Omit<StreamdownProps, keyof Snippets | 'class'> & {
        snippets: Snippets<Source>;
    });
}
export declare const useStreamdown: () => StreamdownContext<Record<string, any>>;
import type { AlertToken, MathToken, SubSupToken, TableToken, THead, TBody, TFoot, THeadRow, TRow, TD, TH, Extension, GenericToken, CitationToken, MdxToken } from './marked/index.js';
import type { Tokens } from 'marked';
import type { ListItemToken, ListToken } from './marked/marked-list.js';
import type { Footnote, FootnoteRef, FootnoteToken } from './marked/marked-footnotes.js';
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
    footnotePopover: FootnoteToken;
    sup: SubSupToken;
    sub: SubSupToken;
    descriptionList: DescriptionListToken;
    description: DescriptionToken;
    descriptionTerm: DescriptionTermToken;
    descriptionDetail: DescriptionDetailToken;
    inlineCitation: CitationToken;
    inlineCitationPopover: CitationToken;
    inlineCitationContent: CitationToken;
    inlineCitationPreview: CitationToken;
    mdx: MdxToken;
};
type PredefinedElements = keyof TokenSnippet;
export type Snippets<Source extends Record<string, any> = Record<string, any>> = {
    [K in PredefinedElements]?: Snippet<[
        {
            children: Snippet;
            token: TokenSnippet[K];
        } & (K extends 'inlineCitationContent' ? {
            source: Source;
            key: string;
        } : K extends 'mdx' ? {
            props: Record<string, number | string | boolean | null | undefined>;
        } : {})
    ]>;
};
export type StreamdownProps<Source extends Record<string, any> = Record<string, any>> = {
    streamdown?: StreamdownContext;
    static?: boolean;
    sources?: {
        [key: string]: Source;
    };
    inlineCitationsMode?: 'list' | 'carousel';
    element?: HTMLElement;
    content: string;
    class?: string;
    parseIncompleteMarkdown?: boolean;
    defaultOrigin?: string;
    allowedLinkPrefixes?: string[];
    allowedImagePrefixes?: string[];
    theme?: DeepPartialTheme;
    baseTheme?: 'tailwind' | 'shadcn';
    mergeTheme?: boolean;
    shikiTheme?: string;
    shikiLanguages?: LanguageInfo[];
    shikiThemes?: Record<string, ThemeRegistration>;
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
    controls?: {
        code?: boolean;
        mermaid?: boolean | {
            enabled?: boolean;
            mouseWheelZoom?: boolean;
        };
        table?: boolean;
    };
    renderHtml?: boolean | ((token: Tokens.HTML | Tokens.Tag) => string);
    animation?: {
        animateOnMount?: boolean;
        enabled?: boolean;
        type?: 'fade' | 'blur' | 'slideUp' | 'slideDown';
        duration?: number;
        timingFunction?: 'ease' | 'ease-in' | 'ease-out' | 'ease-in-out' | 'linear';
        tokenize?: 'word' | 'char';
    };
    icons?: {
        copy?: Snippet;
        download?: Snippet;
        fullscreen?: Snippet;
        zoomIn?: Snippet;
        zoomOut?: Snippet;
        fitView?: Snippet;
        note?: Snippet;
        tip?: Snippet;
        warning?: Snippet;
        caution?: Snippet;
        important?: Snippet;
        chevronLeft?: Snippet;
        chevronRight?: Snippet;
        check?: Snippet;
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
    components?: {
        code?: Component<{
            token: Tokens.Code;
            id: string;
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
} & Partial<Snippets<Source>>;
export {};
