/**
 * The STRUCTURAL completers: the ones that stay enabled on the streaming
 * tail.
 *
 * Each closes a construct whose half-arrived form renders as something
 * visibly wrong rather than as plain text — a bare `[^lab` chip, a half
 * `$$`-block, a dangling `[text](` link, an unclosed MDX tag. Unlike the
 * inline emphasis family (`incompleteMarkdown.inline.ts`), a lone delimiter
 * here is unambiguous, so speculative completion does not flicker.
 *
 * Registration order is load-bearing: `footnoteRef` must run before
 * `inlineCitation`, or a trailing `[^label` gets closed with a plain `]` and
 * the footnote marker is lost.
 */
import { findEndOfCellOrLineContaining } from './incompleteMarkdown.plugin';
import type { ParseState, Plugin } from './incompleteMarkdown.plugin';

export const structuralPlugins: Plugin[] = [
    {
        // Must run before inlineCitation: otherwise a trailing `[^label` gets
        // closed with a plain `]`, defeating the streamdown:footnote marker.
        name: 'footnoteRef',
        pattern: /\[\^[^\]\s,]*/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            if (!line.includes(']')) {
                return line.replace(/\[\^[^\]\s,]*/, '[^streamdown:footnote]');
            }
            return line;
        }
    },
    {
        name: 'inlineCitation',
        pattern: /\[/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Lines that already contain link/image "](" syntax belong to the
            // linksAndImages plugin: completing or preserving them is its job.
            if (line.includes('](')) {
                return line;
            }
            // Collect unescaped opening brackets without a matching closing bracket
            const unclosedPositions = [];
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '[' && (i === 0 || line[i - 1] !== '\\')) {
                    // Check if this bracket has a matching closing bracket later in the line
                    if (line.indexOf(']', i + 1) === -1) {
                        unclosedPositions.push(i);
                    }
                }
            }
            // Close every unclosed citation bracket (right to left so indices stay
            // valid). Brackets that look like incomplete images (`![`), footnotes
            // (`[^`), link text containing markdown formatting, table-cell content,
            // or a trailing bracket preceded by a completed `[...]` pair (evidence
            // of an in-progress link) are left for the dedicated plugins
            // (footnoteRef, linksAndImages).
            let result = line;
            for (let k = unclosedPositions.length - 1; k >= 0; k--) {
                const pos = unclosedPositions[k];
                const endOfCellOrLine = findEndOfCellOrLineContaining(result, pos);
                const content = result.substring(pos + 1, endOfCellOrLine);
                const isImage = pos > 0 && result[pos - 1] === '!';
                const isFootnote = content.startsWith('^');
                const hasFormatting = /[*~`_]/.test(content);
                const isTableCell = endOfCellOrLine < result.length && result[endOfCellOrLine] === '|';
                const hasPriorCompletedPair = /\[[^\]]*\]/.test(line.substring(0, pos));
                if (isImage || isFootnote || hasFormatting || isTableCell || hasPriorCompletedPair) {
                    continue;
                }
                if (k === unclosedPositions.length - 1) {
                    // Last bracket: close at end of cell/line (keeps multi-key citations together)
                    result =
                        result.substring(0, endOfCellOrLine) + ']' + result.substring(endOfCellOrLine);
                }
                else {
                    // Earlier brackets: close right after the citation key (first word)
                    const keyMatch = content.match(/^\s*\S+/);
                    if (keyMatch) {
                        const insertAt = pos + 1 + keyMatch[0].length;
                        result = result.substring(0, insertAt) + ']' + result.substring(insertAt);
                    }
                }
            }
            return result;
        }
    },
    {
        name: 'blockMath',
        pattern: /\$\$/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Don't process block boundaries (lines that are just $$)
            if (line.trim() === '$$')
                return line;
            const dollarPairs = (line.match(/\$\$/g) || []).length;
            if (dollarPairs % 2 === 0)
                return line;
            const firstDollarIndex = line.indexOf('$$');
            // Only complete if there's content after $$ on the same line (no newline immediately after)
            const hasNewlineAfterStart = line.indexOf('\n', firstDollarIndex) !== -1;
            if (!hasNewlineAfterStart) {
                // Single line case: $$content → $$content$$
                return line + '$$';
            }
            // Multi-line cases are handled by contextManager
            return line;
        }
    },
    {
        name: 'descriptionList',
        pattern: /^(\s*):/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Check if this is a description list item that needs completion
            const colonMatch = line.match(/^(\s*):(.+)$/);
            if (colonMatch) {
                const [, , content] = colonMatch;
                // Only complete if the content doesn't already contain a colon
                if (!content.includes(':')) {
                    const endOfCellOrLine = findEndOfCellOrLineContaining(line, line.length - 1);
                    return line.substring(0, endOfCellOrLine) + ':' + line.substring(endOfCellOrLine);
                }
            }
            return line;
        }
    },
    {
        name: 'linksAndImages',
        pattern: /(!?\[.*)$/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Check for incomplete links with URLs: [text](url
            const urlMatch = line.match(/(!?\[[^\]]*\]\()([^)]*?)$/);
            if (urlMatch) {
                const url = urlMatch[2];
                if (url.length > 0) {
                    // Inline isUrlIncomplete logic
                    let isIncomplete = true;
                    if (url && url.length >= 4) {
                        if ((url.startsWith('http://') && url.length >= 12) ||
                            (url.startsWith('https://') && url.length >= 13)) {
                            let domain = url;
                            if (url.startsWith('http://'))
                                domain = url.substring(7);
                            else if (url.startsWith('https://'))
                                domain = url.substring(8);
                            domain = domain.split('/')[0].split('?')[0].split('#')[0];
                            const domainParts = domain.split('.');
                            if (domainParts.length >= 2) {
                                const extension = domainParts[domainParts.length - 1];
                                if (extension.length >= 2 && /^[a-zA-Z]+$/.test(extension)) {
                                    isIncomplete = false;
                                }
                            }
                        }
                    }
                    if (isIncomplete) {
                        const marker = urlMatch[1].startsWith('!')
                            ? 'streamdown:incomplete-image'
                            : 'streamdown:incomplete-link';
                        return line.replace(url, marker) + ')';
                    }
                    else {
                        return line + ')';
                    }
                }
                else {
                    const marker = urlMatch[1].startsWith('!')
                        ? 'streamdown:incomplete-image'
                        : 'streamdown:incomplete-link';
                    return line + marker + ')';
                }
            }
            // Check for incomplete links without URLs: [text
            const linkMatch = line.match(/(!?\[)([^\]]*?)$/);
            if (linkMatch && !line.includes('](')) {
                const [, openBracket, linkTextWithPossibleBoundary] = linkMatch;
                // Position of the matched opening bracket (the regex matches the first
                // bracket that stays unclosed through the end of the line). Using the
                // match index keeps the replacement aligned with the captured link text;
                // `lastIndexOf` could point at a different bracket and duplicate text.
                const bracketIndex = linkMatch.index ?? 0;
                const endOfCellOrLine = findEndOfCellOrLineContaining(line, bracketIndex);
                // Extract the clean link text (remove any trailing | or whitespace)
                const linkText = linkTextWithPossibleBoundary.replace(/[\s|]+$/, '');
                const marker = openBracket.startsWith('!')
                    ? 'streamdown:incomplete-image'
                    : 'streamdown:incomplete-link';
                // Replace from bracket to end of cell/line, including boundary if it's |
                const includeBoundary = endOfCellOrLine < line.length && line[endOfCellOrLine] === '|';
                const incompleteEnd = includeBoundary ? endOfCellOrLine + 1 : endOfCellOrLine;
                const completedPart = openBracket + linkText + '](' + marker + ')' + (includeBoundary ? '|' : '');
                return line.substring(0, bracketIndex) + completedPart + line.substring(incompleteEnd);
            }
            return line;
        }
    },
    {
        name: 'mdx',
        skipInBlockTypes: ['code', 'math', 'center', 'right'],
        preprocess: ({ text, state }) => {
            // Track MDX component states across the entire text
            const lines = text.split('\n');
            const openTags: NonNullable<ParseState['mdxUnclosedTags']> = [];
            const mdxLineStates: NonNullable<ParseState['mdxLineStates']> = [];
            for (let i = 0; i < lines.length; i++) {
                // Lines inside code fences or math blocks are opaque content: MDX-looking
                // tags there must not open/close/track components.
                const lineCtx = state.lineContexts?.[i];
                if (lineCtx?.code || lineCtx?.math) {
                    mdxLineStates[i] = { inMdx: false, incompletePositions: [] };
                    continue;
                }
                const line = lines[i];
                let inMdx = false;
                const incompletePositions: number[] = [];
                // Find all MDX tags in the line
                let searchPos = 0;
                while (searchPos < line.length) {
                    // Look for opening bracket with capital letter (MDX component)
                    const tagStart = line.indexOf('<', searchPos);
                    if (tagStart === -1 || tagStart >= line.length - 1)
                        break;
                    const nextChar = line[tagStart + 1];
                    // Closing tag for a component opened on an earlier line. Handled
                    // inside the scan so a close that is part of a same-line complete
                    // pair (consumed below) is never double-counted against the stack.
                    const closeTagMatch = line.substring(tagStart).match(/^<\/([A-Z][a-zA-Z0-9]*)>/);
                    if (closeTagMatch) {
                        const tagName = closeTagMatch[1];
                        // Pop the innermost same-name open (LIFO) so the auto-appended
                        // closers keep the right nesting order.
                        for (let openIndex = openTags.length - 1; openIndex >= 0; openIndex--) {
                            if (openTags[openIndex].tagName === tagName) {
                                openTags.splice(openIndex, 1);
                                break;
                            }
                        }
                        searchPos = tagStart + closeTagMatch[0].length;
                        continue;
                    }
                    // Only match if starts with capital letter (MDX component)
                    if (!/[A-Z]/.test(nextChar)) {
                        searchPos = tagStart + 1;
                        continue;
                    }
                    // Try to match complete self-closing tag
                    const selfClosingMatch = line
                        .substring(tagStart)
                        .match(/^<([A-Z][a-zA-Z0-9]*)((?:\s+\w+=(?:"[^"]*"|{[^}]*}))*)\s*\/>/);
                    if (selfClosingMatch) {
                        searchPos = tagStart + selfClosingMatch[0].length;
                        continue;
                    }
                    // Try to match complete opening tag with immediate closing
                    const completeMatch = line
                        .substring(tagStart)
                        .match(/^<([A-Z][a-zA-Z0-9]*)((?:\s+\w+=(?:"[^"]*"|{[^}]*}))*)\s*>.*?<\/\1>/);
                    if (completeMatch) {
                        searchPos = tagStart + completeMatch[0].length;
                        continue;
                    }
                    // Try to match opening tag
                    const openTagMatch = line
                        .substring(tagStart)
                        .match(/^<([A-Z][a-zA-Z0-9]*)((?:\s+\w+=(?:"[^"]*"|{[^}]*}))*)\s*>/);
                    if (openTagMatch) {
                        const tagName = openTagMatch[1];
                        openTags.push({ tagName, lineIndex: i });
                        inMdx = true;
                        searchPos = tagStart + openTagMatch[0].length;
                        continue;
                    }
                    // Check for incomplete self-closing (e.g., <Component /)
                    const incompleteSelfClosing = line
                        .substring(tagStart)
                        .match(/^<([A-Z][a-zA-Z0-9]*)[^>]*\/$/);
                    if (incompleteSelfClosing) {
                        incompletePositions.push(tagStart);
                        break; // This is at the end of the line
                    }
                    // Check for incomplete tag (no closing >) - only at end of line
                    const incompleteTag = line
                        .substring(tagStart)
                        .match(/^<([A-Z][a-zA-Z0-9]*)(?:\s+[^>]*)?$/);
                    if (incompleteTag) {
                        incompletePositions.push(tagStart);
                        break; // This is at the end of the line
                    }
                    searchPos = tagStart + 1;
                }
                mdxLineStates[i] = { inMdx, incompletePositions };
            }
            return {
                text,
                state: {
                    mdxUnclosedTags: openTags,
                    mdxLineStates
                }
            };
        },
        handler: ({ line, state }) => {
            // Remove incomplete MDX syntax (don't render it)
            const lineStates = state.mdxLineStates || [];
            const currentState = lineStates[state.currentLine];
            if (currentState?.incompletePositions && currentState.incompletePositions.length > 0) {
                // Process incomplete positions from right to left to preserve indices
                let result = line;
                for (let i = currentState.incompletePositions.length - 1; i >= 0; i--) {
                    const pos = currentState.incompletePositions[i];
                    const before = result.substring(0, pos);
                    // Simply remove the incomplete MDX tag
                    result = before;
                }
                return result;
            }
            return line;
        },
        postprocess: ({ text, state }) => {
            // Complete unclosed MDX components at the end
            const unclosedTags = state.mdxUnclosedTags || [];
            if (unclosedTags.length > 0) {
                // Close tags in reverse order (innermost first)
                let result = text;
                for (let i = unclosedTags.length - 1; i >= 0; i--) {
                    result += `\n</${unclosedTags[i].tagName}>`;
                }
                return result;
            }
            return text;
        }
    }
];
