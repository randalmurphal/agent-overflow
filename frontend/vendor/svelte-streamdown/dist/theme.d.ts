/**
 * The class table `<Streamdown>` renders from. Upstream shipped a Tailwind
 * base table here plus a `mergeTheme` helper that tailwind-merged a host's
 * partial override over it on every read; both are gone. The host now owns
 * ONE flat table and hands it in whole, so this file declares only the shape
 * the render path reads.
 *
 * Every slot below has at least one reader in `Block.svelte`,
 * `Elements/*.svelte`, `static-html.ts` or the host's own code hosts. Adding a
 * slot here without a reader, or reading one that is not declared, fails
 * `chat/markdown/streamdownTheme.test.ts`, which derives the roster from the
 * render path's source.
 */
export interface Theme {
    link: {
        base: string;
        blocked: string;
    };
    h1: {
        base: string;
    };
    h2: {
        base: string;
    };
    h3: {
        base: string;
    };
    h4: {
        base: string;
    };
    h5: {
        base: string;
    };
    h6: {
        base: string;
    };
    paragraph: {
        base: string;
    };
    ul: {
        base: string;
    };
    ol: {
        base: string;
    };
    li: {
        base: string;
        checkbox: string;
    };
    /** `header`, `buttons`, `language`, `skeleton` and `line` died with the
     * vendored shiki code component: the host renders code DOM itself. */
    code: {
        base: string;
        container: string;
        pre: string;
    };
    codespan: {
        base: string;
    };
    image: {
        base: string;
        image: string;
    };
    blockquote: {
        base: string;
    };
    alert: {
        base: string;
        title: string;
        icon: string;
        note: string;
        tip: string;
        warning: string;
        caution: string;
        important: string;
    };
    table: {
        base: string;
        table: string;
    };
    thead: {
        base: string;
    };
    tbody: {
        base: string;
    };
    tfoot: {
        base: string;
    };
    tr: {
        base: string;
    };
    td: {
        base: string;
    };
    th: {
        base: string;
    };
    sup: {
        base: string;
    };
    sub: {
        base: string;
    };
    hr: {
        base: string;
    };
    strong: {
        base: string;
    };
    em: {
        base: string;
    };
    del: {
        base: string;
    };
    /** `icon` died with the download control; the surviving expand button
     * renders `icons.fullscreen`, which carries its own sizing. */
    mermaid: {
        base: string;
        buttons: string;
    };
    math: {
        block: string;
        inline: string;
    };
    footnoteRef: {
        base: string;
    };
    descriptionList: {
        base: string;
    };
    descriptionTerm: {
        base: string;
    };
    descriptionDetail: {
        base: string;
    };
    /** `popover` died with the footnote popover. */
    components: {
        button: string;
    };
}
