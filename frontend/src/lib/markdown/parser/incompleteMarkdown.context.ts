/**
 * The block-context completer: one plugin that pre-scans the whole tail.
 *
 * It records, per source line, which blocking construct that line sits inside
 * (fenced code, `$$` math, `[center]`, `[right]`) so the inline completers can
 * skip lines they must not touch, then seals whatever is still open at the end
 * of the document. Sealing an open fence replicates the OPENER's leading
 * prefix, fence char and run length (divergence 11): a flush-left ``` under a
 * list-indented fence is not a closer per CommonMark — it terminates the list
 * and opens a new top-level fence, which rendered as a phantom empty code
 * block under the streaming one until the real closer arrived.
 */
import type { FenceInfo, LineContext, Plugin } from './incompleteMarkdown.plugin';

export const contextPlugins: Plugin[] = [
    {
        name: 'contextManager',
        preprocess: ({ text }) => {
            // Pre-scan the entire text to establish blocking contexts
            const lines = text.split('\n');
            let inCodeBlock = false;
            // The fence awaiting its closer: leading prefix (indentation
            // and/or blockquote markers), fence char, run length, and the
            // line it opened on. Sealing an open fence must replicate all
            // of these — a flush-left ``` closer under a list-indented
            // fence is NOT a closer per CommonMark: it terminates the
            // list and opens a NEW top-level fence, which renders as a
            // phantom empty code block under the streaming one until the
            // real closer arrives.
            let openFence: FenceInfo | undefined;
            let inMathBlock = false;
            let inCenterBlock = false;
            let inRightBlock = false;
            let centerOpenLine = -1;
            let rightOpenLine = -1;
            // Track which lines are in which contexts for state management
            const lineContexts: LineContext[] = [];
            for (let i = 0; i < lines.length; i++) {
                const line = lines[i];
                // Check for block boundaries (fences may be quoted inside blockquotes/alerts: "> ```")
                const fenceLine = line.replace(/^[ \t]*(?:>[ \t]*)*/, '');
                const fenceRun = fenceLine.match(/^(`{3,}|~{3,})/)?.[1];
                if (fenceRun) {
                    if (!inCodeBlock) {
                        inCodeBlock = true;
                        openFence = {
                            prefix: line.slice(0, line.length - fenceLine.length),
                            char: fenceRun[0],
                            length: fenceRun.length,
                            lineIndex: i
                        };
                    }
                    else if (openFence &&
                        fenceRun[0] === openFence.char &&
                        fenceRun.length >= openFence.length &&
                        fenceLine.slice(fenceRun.length).trim() === '') {
                        // A closing fence must be a bare run of the SAME char,
                        // at least as long as the opener (CommonMark). A ```
                        // line inside a ~~~ fence (or a shorter run inside a
                        // longer one, or a run with an info string) is content
                        // — treating it as a closer desyncs the seal from what
                        // marked actually lexes.
                        inCodeBlock = false;
                        openFence = undefined;
                    }
                }
                if (line.trim().startsWith('$$') && !line.trim().includes('$$', 2)) {
                    inMathBlock = !inMathBlock;
                }
                if (line.trim() === '[center]') {
                    inCenterBlock = true;
                    centerOpenLine = i;
                }
                if (line.trim() === '[/center]') {
                    inCenterBlock = false;
                }
                if (line.trim() === '[right]') {
                    inRightBlock = true;
                    rightOpenLine = i;
                }
                if (line.trim() === '[/right]') {
                    inRightBlock = false;
                }
                lineContexts[i] = {
                    code: inCodeBlock,
                    math: inMathBlock,
                    center: inCenterBlock,
                    right: inRightBlock
                };
            }
            // Set the final blocking contexts (for postprocessing)
            const finalContexts = new Set<'code' | 'math' | 'center' | 'right'>();
            if (inCodeBlock)
                finalContexts.add('code');
            if (inMathBlock)
                finalContexts.add('math');
            // Only auto-close center/right when content follows the opening tag;
            // a bare trailing '[center]'/'[right]' line is left untouched.
            if (inCenterBlock && centerOpenLine < lines.length - 1)
                finalContexts.add('center');
            if (inRightBlock && rightOpenLine < lines.length - 1)
                finalContexts.add('right');
            // Return both the text and the updated state
            return {
                text: text, // Don't modify text in preprocess
                state: {
                    blockingContexts: finalContexts,
                    lineContexts,
                    fenceInfo: inCodeBlock ? openFence : undefined
                }
            };
        },
        postprocess: ({ text, state }) => {
            // Complete incomplete blocks at end of input.
            // Close inner blocks (code/math) before alignment wrappers.
            let result = text;
            if (state.blockingContexts.has('code')) {
                const fence = state.fenceInfo;
                if (fence) {
                    // Drop a trailing PARTIAL closer first — a bare,
                    // too-short run of the fence char (a half-streamed
                    // ` or `` before the full closing ```). Left in
                    // place it renders as a one-chunk content line that
                    // vanishes when the real closer lands (a visible
                    // grow-then-shrink flicker). A bare run long enough
                    // to close would have toggled the fence shut in
                    // preprocess, so anything still here is partial.
                    // Never strips the opener: that line carries the
                    // fence run at the START, and lineIndex guards the
                    // one-line case.
                    const lastNewline = result.lastIndexOf('\n');
                    if (lastNewline >= 0 && fence.lineIndex < (state.lineContexts?.length ?? 0) - 1) {
                        const lastLine = result
                            .slice(lastNewline + 1)
                            .replace(/^[ \t]*(?:>[ \t]*)*/, '');
                        const isPartialCloser = lastLine.length > 0 &&
                            lastLine.length < fence.length &&
                            Array.from(lastLine).every((ch) => ch === fence.char);
                        if (isPartialCloser) {
                            result = result.slice(0, lastNewline);
                        }
                    }
                    // Seal with a closer marked will actually accept:
                    // same leading prefix (list indentation, blockquote
                    // markers), same fence char, same run length.
                    result += '\n' + fence.prefix + fence.char.repeat(fence.length);
                }
                else {
                    result += '\n```';
                }
            }
            if (state.blockingContexts.has('math')) {
                result += '\n$$';
            }
            if (state.blockingContexts.has('center')) {
                result += '\n[/center]';
            }
            if (state.blockingContexts.has('right')) {
                result += '\n[/right]';
            }
            return result;
        }
    },
];
