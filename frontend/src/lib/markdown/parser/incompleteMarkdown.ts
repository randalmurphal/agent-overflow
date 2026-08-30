/**
 * Speculative completion for the STREAMING VOLATILE TAIL.
 *
 * A half-arrived markdown construct renders as something the next chunk
 * revokes — a `**bold` opener as literal asterisks, an unclosed fence as
 * paragraph text, a lone `-` under a line as an `<h2>`. This pass rewrites
 * the tail so those constructs render as what they are about to become, and
 * it runs ONLY on the volatile tail: the committed prefix and the settled
 * instance pass `parseIncompleteMarkdown === false` and never reach here.
 *
 * This module is the engine and the registration order. The completers
 * themselves live by family:
 *   - `incompleteMarkdown.context.ts`    block contexts + fence sealing
 *   - `incompleteMarkdown.inline.ts`     speculative inline emphasis (disabled)
 *   - `incompleteMarkdown.structural.ts` links, footnotes, math, MDX, dl
 * and the plugin contract plus the line scans they share are in
 * `incompleteMarkdown.plugin.ts`.
 */
import { contextPlugins } from './incompleteMarkdown.context';
import { speculativeInlinePlugins } from './incompleteMarkdown.inline';
import { structuralPlugins } from './incompleteMarkdown.structural';
import type { ParseState, Plugin } from './incompleteMarkdown.plugin';

export type { Plugin } from './incompleteMarkdown.plugin';
export class IncompleteMarkdownParser {
    private plugins: Plugin[] = [];
    private state: ParseState = {
        currentLine: 0,
        context: 'normal',
        blockingContexts: new Set(),
        lineContexts: []
    };
    setState = (state: Partial<ParseState>): void => {
        this.state = { ...this.state, ...state };
    };
    constructor(plugins: Plugin[] = []) {
        this.plugins = plugins;
    }
    // Main parsing methods
    parse(text: string): string {
        if (!text || typeof text !== 'string') {
            return text;
        }
        this.state = {
            currentLine: 0,
            context: 'normal',
            blockingContexts: new Set(),
            lineContexts: [],
            fenceInfo: undefined
        };
        let result = text;
        // Execute preprocess hooks for all plugins
        for (const plugin of this.plugins) {
            if (plugin.preprocess) {
                try {
                    const preprocessResult = plugin.preprocess({
                        text: result,
                        state: this.state,
                        setState: this.setState
                    });
                    if (typeof preprocessResult === 'string') {
                        result = preprocessResult;
                    }
                    else {
                        result = preprocessResult.text;
                        this.setState(preprocessResult.state);
                    }
                }
                catch (error) {
                    console.error(`Plugin ${plugin.name} preprocess hook failed:`, error);
                }
            }
        }
        // Split into lines for processing
        const lines = result.split('\n');
        const processedLines = [...lines];
        // Process each line with each plugin
        for (let i = 0; i < processedLines.length; i++) {
            this.state.currentLine = i;
            let line = processedLines[i];
            for (const plugin of this.plugins) {
                // Skip this plugin if current line is in a blocking context
                const currentLineContext = this.state.lineContexts?.[i];
                const shouldSkip = currentLineContext &&
                    (plugin.skipInBlockTypes || []).some((blockType) => currentLineContext[blockType]);
                if (shouldSkip) {
                    continue;
                }
                try {
                    const match = plugin.pattern ? line.match(plugin.pattern) : line.match(/.*/);
                    if (match && plugin.handler) {
                        line = plugin.handler({
                            line,
                            text: line,
                            match,
                            state: this.state,
                            setState: this.setState
                        });
                    }
                }
                catch (error) {
                    console.error(`Plugin ${plugin.name} failed on line ${i}:`, error);
                }
            }
            processedLines[i] = line;
        }
        // Rebuild text from processed lines
        result = processedLines.join('\n');
        // Execute afterParse hooks for all plugins
        for (const plugin of this.plugins) {
            if (plugin.postprocess) {
                try {
                    result = plugin.postprocess({ text: result, state: this.state, setState: this.setState });
                }
                catch (error) {
                    console.error(`Plugin ${plugin.name} afterParse hook failed:`, error);
                }
            }
        }
        return result;
    }
    // The default completer set, in registration order.
    static createDefaultPlugins(): Plugin[] {
        // Bound to a typed local rather than returned inline: as the receiver
        // of `.filter()` the array literal would have no contextual type, and
        // every plugin's destructured hook parameters would infer as `any`.
        const plugins: Plugin[] = [
            ...contextPlugins,
            ...speculativeInlinePlugins,
            ...structuralPlugins
        ];
        // Divergence 4: the ten speculative inline completers mis-close on a
        // lone delimiter mid-stream, so they are registered and then dropped.
        // The structural completers keep upstream's behavior. Re-enabling the
        // safe ones (bold/italic/strike) is a separate follow-up; this list is
        // where that decision is expressed.
        return plugins.filter((p) => ![
            'boldItalic',
            'bold',
            'doubleUnderscoreItalic',
            'strikethrough',
            'singleAsteriskItalic',
            'inlineCode',
            'singleUnderscoreItalic',
            'subscript',
            'superscript',
            'inlineMath',
        ].includes(p.name));
    }
}
// Legacy function for backward compatibility
const defaultPlugins = IncompleteMarkdownParser.createDefaultPlugins();
const defaultParser = new IncompleteMarkdownParser(defaultPlugins);
// Drop a dangling Setext underline so the streamed line above it is not
// transiently promoted to a heading. CommonMark reads a non-blank line followed
// by a lone run of `-`/`=` as a Setext heading, so mid-stream the underline-only
// line of a nested bullet ("  -" arriving before its text) flips the line above
// to <h2> for one chunk, then collapses back once the bullet text streams in — a
// visible font/margin/re-wrap "balloon". Indented code, thematic breaks, and
// list starts all require a blank line above (or no line above), so guarding on
// a non-blank preceding line leaves them untouched. Runs AFTER
// defaultParser.parse so fence-completion has already sealed any open code fence
// (its trailing `-` is then no longer the last line). Streaming-tail only: the
// committed prefix and the settled single instance pass
// parseIncompleteMarkdown === false and never reach this path.
const stripDanglingSetextUnderline = (text: string): string => {
    const lastNewline = text.lastIndexOf('\n');
    if (lastNewline < 0) {
        return text; // single line: a list-start "-", never a Setext underline
    }
    if (!/^[ \t]*[-=]+[ \t]*$/.test(text.slice(lastNewline + 1))) {
        return text; // last line is not a lone run of `-`/`=`
    }
    const prevLineStart = text.lastIndexOf('\n', lastNewline - 1) + 1;
    if (text.slice(prevLineStart, lastNewline).trim() === '') {
        return text; // blank line above: thematic break / indented code / list start, not Setext
    }
    return text.slice(0, lastNewline); // drop the dangling underline line
};
export const parseIncompleteMarkdown = (text: string): string => {
    if (!text || typeof text !== 'string') {
        return text;
    }
    return stripDanglingSetextUnderline(defaultParser.parse(text));
};
