import type { Extension } from '../index';
import type { Tokens } from '../engine';

type variantType = 'note' | 'tip' | 'important' | 'warning' | 'caution';
import { Lexer } from '../engine';
import { markdownNesting } from './htmlRuns';
const variants: variantType[] = ['note', 'tip', 'important', 'warning', 'caution'];
export function createSyntaxPattern(type: variantType): string {
    return `^\\s*[\\*_]*\\[!${type.toUpperCase()}\\][\\*_]*\\s*`;
}
// Precomputed once per variant instead of recompiling on every blockquote:
//  - syntax: detects the `[!NOTE]` marker at the start of the quoted text
//  - marker: the same marker in the quote's source, from the quote's start
//    through the marker and the blanks after it. Only `>` prefixes and
//    whitespace can precede a marker `syntax` matched.
const VARIANT_PATTERNS = variants.map((type) => ({
    type,
    syntax: new RegExp(createSyntaxPattern(type), 'i'),
    marker: new RegExp(`^[\\s>]*?[\\*_]*\\[!${type.toUpperCase()}\\][\\*_]*[ \\t]*`, 'i')
}));
// Cheap pre-gate. marked calls every registered block extension at every
// block position, so the per-call Lexer below must not be built for the
// overwhelming majority of positions that cannot possibly be a blockquote.
// Mirrors marked's own `rules.other.blockquoteStart`.
const BLOCKQUOTE_OPEN = /^ {0,3}>/;

/**
 * Where a quote's alert marker is: its variant, and the offset in the
 * quote's source past the marker. The rest of that line stays the body's
 * first line, unless `title` took it.
 */
type AlertMarker = { variant: variantType; end: number; title?: string };
type ReadMarker = (lexer: Lexer, measured: Tokens.Blockquote) => AlertMarker | null;

/** Agent chat: a marker, decorated or not, opening the quoted text, at any depth. */
const chatMarker: ReadMarker = (_lexer, measured) => {
    const matched = VARIANT_PATTERNS.find((v) => v.syntax.test(measured.text));
    const marker = matched?.marker.exec(measured.raw);
    return matched && marker ? { variant: matched.type, end: marker[0].length } : null;
};

// A forge marker opening a line, in either case, and the rest of the line.
const FORGE_MARKER = /^[ \t]*\[!(note|tip|important|warning|caution)\]([^\n]*)/i;
const QUOTE_PREFIX = /^ {0,3}>/;

/**
 * GitLab's alert marker at the start of `line` (a quote's first line past
 * its `>`, or a `>>>` fence's opening line past the fence): the variant,
 * and the rest of the line as the alert's title when it holds any.
 */
export const gitlabAlertMarker = (line: string): { variant: variantType; title?: string; length: number } | null => {
    const marker = FORGE_MARKER.exec(line);
    if (!marker) return null;
    const title = marker[2].trim();
    return { variant: marker[1].toLowerCase() as variantType, length: marker[0].length, ...(title === '' ? {} : { title }) };
};

const forgeMarker = (raw: string): (ReturnType<typeof gitlabAlertMarker> & { prefix: number }) | null => {
    const prefix = QUOTE_PREFIX.exec(raw)?.[0].length ?? 0;
    const marker = prefix === 0 ? null : gitlabAlertMarker(raw.slice(prefix));
    return marker && { ...marker, prefix };
};

/**
 * GitHub: the marker alone on the first line of a quote no Markdown
 * container holds (HTML around it does not count).
 */
const githubMarker: ReadMarker = (lexer, measured) => {
    const marker = forgeMarker(measured.raw);
    if (!marker || marker.title !== undefined || markdownNesting(lexer) > 0) return null;
    return { variant: marker.variant, end: marker.prefix + marker.length };
};

/** GitLab: the marker opening a quote's first line, the rest of the line its title. */
const gitlabMarker: ReadMarker = (_lexer, measured) => {
    const marker = forgeMarker(measured.raw);
    return marker && { variant: marker.variant, title: marker.title, end: marker.prefix + marker.length };
};

/**
 * Every block quote, as an alert when `readMarker` finds one. A forge
 * variant runs ahead of the default rules, and a forge surface leaves
 * chat's variant out (`markedForgeGrammar`).
 */
const alertExtension = (readMarker: ReadMarker, precedesDefaults: boolean): Extension => ({
    name: 'alert',
    level: 'block',
    precedesDefaults,
    tokenizer(src) {
        if (!BLOCKQUOTE_OPEN.test(src))
            return undefined;
        // Measuring lexer, thrown away with the call: it reads the quoted
        // text to find an alert marker and contributes no token. A lexer
        // shared across calls would keep every inline queue entry, link
        // definition and footnote this pass pushes, and carry them from one
        // document into the next.
        const measured = new Lexer({ gfm: true }).tokenizer.blockquote(src);
        if (!measured)
            return undefined;
        // The body is lexed by the ACTIVE lexer through the engine's own
        // blockquote rule, so the children and their inline queue entries
        // belong to the document being parsed, a block extension claims a
        // run that crosses a blank quoted line (a `<details>` dropdown,
        // display math), and a lazy line after a non-paragraph stays its own
        // block.
        const active = this.lexer.tokenizer;
        const marker = readMarker(this.lexer, measured);
        if (!marker)
            return active.blockquote(src);
        // The quote without its marker. `lead` stands in for everything up
        // to the marker's end: the rest of the marker's line stays a quoted
        // line, a line holding only the marker goes, and a lazy line after
        // such a line becomes the quote's first line.
        const rest = measured.raw.slice(marker.end);
        const skip = rest.startsWith('\n') ? 1 : 0;
        const lead = skip === 0 ? '>' : BLOCKQUOTE_OPEN.test(rest.slice(1)) ? '' : '> ';
        const body = active.blockquote(lead + rest.slice(skip));
        if (!body)
            return undefined;
        return alertToken(marker, src.slice(0, marker.end + skip + body.raw.length - lead.length), body.text, body.tokens);
    }
});

/**
 * The alert a quote whose marker is `marker` makes, whichever quote form
 * (`>` lines, GitLab's `>>>` fence): `raw` its source, `text` and `blocks`
 * its body without the marker. An alert's children are its blocks alone.
 */
export const alertToken = (
    marker: { variant: variantType; title?: string },
    raw: string,
    text: string,
    blocks: Tokens.Blockquote['tokens']
): AlertToken & { tokens: Tokens.Blockquote['tokens'] } => ({
    type: 'alert',
    variant: marker.variant,
    ...(marker.title === undefined ? {} : { title: marker.title }),
    raw,
    text: text.trim(),
    tokens: blocks.filter((token) => token.type !== 'space')
});

/** Agent chat's alerts. */
export const markedAlert: Extension = alertExtension(chatMarker, false);
/** Alerts as GitHub renders them, for a GitHub surface. */
export const markedGithubAlert: Extension = alertExtension(githubMarker, true);
/** Alerts as GitLab renders them, for a GitLab surface. */
export const markedGitlabAlert: Extension = alertExtension(gitlabMarker, true);

export type AlertToken = {
    type: 'alert';
    variant: variantType;
    /** A GitLab alert's own title, shown in place of the variant's name. */
    title?: string;
    raw: string;
    text: string;
};
