/**
 * The speculative INLINE completers — all ten of them currently DISABLED.
 *
 * These close an unbalanced inline delimiter at the end of the streaming
 * tail: `**bold`, `` `code ``, `$math$`, `~sub~`, `^sup^`. They are built and
 * registered, then dropped by the disable list in `createDefaultPlugins`,
 * because a speculative close on a LONE delimiter mid-stream renders emphasis
 * that the next chunk revokes — a visible flicker on ordinary prose. Keeping
 * them registered-then-filtered is deliberate: re-enabling the safe subset
 * (bold / italic / strike) is a standing follow-up, and the disable list is
 * the one place that decision is expressed.
 *
 * Order within this array is upstream's. Its position relative to the
 * structural family is not observable while the whole family is filtered out.
 */
import { findEndOfCellOrLineContaining, isWithinFootnoteRef, isWithinMathBlock } from './incompleteMarkdown.plugin';
import type { Plugin } from './incompleteMarkdown.plugin';

export const speculativeInlinePlugins: Plugin[] = [
    {
        name: 'boldItalic',
        pattern: /\*\*\*/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            if (line.trim() === '***') {
                return line;
            }
            const isEndingWithTripleAsterisk = line.endsWith('***');
            const tripleAsterisks = (line.match(/\*\*\*/g) || []).length;
            if (tripleAsterisks % 2 === 1) {
                const lastTripleAsteriskIndex = line.lastIndexOf('***');
                const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastTripleAsteriskIndex);
                if (isEndingWithTripleAsterisk) {
                    return line.substring(0, lastTripleAsteriskIndex);
                }
                const before = line.substring(0, endOfCellOrLine);
                // Part of the closing '***' may have already arrived (a trailing '*' or
                // '**'); only add the missing asterisks so we complete to exactly '***'
                // instead of leaving stray asterisks after the text.
                const trailing = before.match(/\*+$/)?.[0].length ?? 0;
                const missing = trailing >= 1 && trailing <= 2 ? 3 - trailing : 3;
                return before + '*'.repeat(missing) + line.substring(endOfCellOrLine);
            }
            return line;
        }
    },
    {
        name: 'bold',
        pattern: /\*\*/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            if (line.trim() === '***') {
                return line;
            }
            const doubleAsteriskMatches = (line.match(/\*\*/g) || []).length;
            if (doubleAsteriskMatches % 2 === 1) {
                const isEndingWithDoubleAsterisk = line.endsWith('**');
                const lastDoubleAsteriskIndex = line.lastIndexOf('**');
                const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastDoubleAsteriskIndex);
                if (isEndingWithDoubleAsterisk) {
                    return line.substring(0, lastDoubleAsteriskIndex);
                }
                const before = line.substring(0, endOfCellOrLine);
                // If the content already ends with a single '*' — the first half of the
                // closing '**' arriving mid-stream — only add one more '*' to complete it.
                // Otherwise we'd emit '***' and leave a stray '*' after the bold text.
                const closing = before.endsWith('*') && !before.endsWith('**') ? '*' : '**';
                return before + closing + line.substring(endOfCellOrLine);
            }
            return line;
        }
    },
    {
        name: 'doubleUnderscoreItalic',
        pattern: /__/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            if (line.trim() === '___') {
                return line;
            }
            const underscorePairs = (line.match(/__/g) || []).length;
            if (underscorePairs % 2 === 1) {
                const isEndingWithDoubleUnderscore = line.endsWith('__');
                const lastDoubleUnderscoreIndex = line.lastIndexOf('__');
                const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastDoubleUnderscoreIndex);
                if (isEndingWithDoubleUnderscore) {
                    return line.substring(0, lastDoubleUnderscoreIndex);
                }
                const before = line.substring(0, endOfCellOrLine);
                // A half-typed closing '__' (a lone trailing '_') only needs one more '_',
                // not a full '__' that would leave a stray underscore after the text.
                const closing = before.endsWith('_') && !before.endsWith('__') ? '_' : '__';
                return before + closing + line.substring(endOfCellOrLine);
            }
            return line;
        }
    },
    {
        name: 'strikethrough',
        pattern: /~~/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            const tildePairs = (line.match(/~~/g) || []).length;
            if (tildePairs % 2 === 1) {
                const isEndingWithDoubleTilde = line.endsWith('~~');
                const lastDoubleTildeIndex = line.lastIndexOf('~~');
                const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastDoubleTildeIndex);
                // Only complete if there's content after the tildes
                const contentAfterTildes = line.substring(lastDoubleTildeIndex + 2, endOfCellOrLine);
                if (contentAfterTildes.trim().length > 0) {
                    if (isEndingWithDoubleTilde) {
                        return line.substring(0, lastDoubleTildeIndex);
                    }
                    const before = line.substring(0, endOfCellOrLine);
                    // A half-typed closing '~~' (a lone trailing '~') only needs one more '~'.
                    const closing = before.endsWith('~') && !before.endsWith('~~') ? '~' : '~~';
                    return before + closing + line.substring(endOfCellOrLine);
                }
            }
            return line;
        }
    },
    {
        name: 'singleAsteriskItalic',
        pattern: /[\s\S]*/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            if (line.trim() === '***') {
                return line;
            }
            // Inline countSingleAsterisks logic
            let singleAsterisks = 0;
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '*') {
                    const prevChar = i > 0 ? line[i - 1] : '';
                    const nextChar = i < line.length - 1 ? line[i + 1] : '';
                    let lineStartIndex = i;
                    for (let j = i - 1; j >= 0; j--) {
                        if (line[j] === '\n') {
                            lineStartIndex = j + 1;
                            break;
                        }
                        if (j === 0) {
                            lineStartIndex = 0;
                            break;
                        }
                    }
                    const beforeAsterisk = line.substring(lineStartIndex, i);
                    if (beforeAsterisk.trim() === '' && (nextChar === ' ' || nextChar === '\t')) {
                        continue;
                    }
                    if (prevChar !== '*' && nextChar !== '*') {
                        singleAsterisks++;
                    }
                }
            }
            if (singleAsterisks % 2 === 1) {
                // Inline findFirstSingleAsterisk logic
                let firstSingleAsteriskIndex = -1;
                for (let i = 0; i < line.length; i++) {
                    if (line[i] === '*' && line[i - 1] !== '*' && line[i + 1] !== '*') {
                        const prevChar = i > 0 ? line[i - 1] : '';
                        const nextChar = i < line.length - 1 ? line[i + 1] : '';
                        if (/\w/.test(prevChar) && /\w/.test(nextChar))
                            continue;
                        if (/\w/.test(prevChar) && !/\s/.test(prevChar))
                            continue;
                        firstSingleAsteriskIndex = i;
                        break;
                    }
                }
                if (firstSingleAsteriskIndex !== -1) {
                    const endOfCellOrLine = findEndOfCellOrLineContaining(line, firstSingleAsteriskIndex);
                    return line.substring(0, endOfCellOrLine) + '*' + line.substring(endOfCellOrLine);
                }
            }
            return line;
        }
    },
    {
        name: 'inlineCode',
        skipInBlockTypes: ['code', 'math'],
        pattern: /`/,
        handler: ({ line }) => {
            // Inline countSingleBackticks logic
            let singleBacktickCount = 0;
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '`') {
                    const isTripleStart = line.substring(i, i + 3) === '```';
                    const isTripleMiddle = i > 0 && line.substring(i - 1, i + 2) === '```';
                    const isTripleEnd = i > 1 && line.substring(i - 2, i + 1) === '```';
                    const isPartOfTriple = isTripleStart || isTripleMiddle || isTripleEnd;
                    if (!isPartOfTriple) {
                        singleBacktickCount++;
                    }
                }
            }
            // Inline hasCompleteCodeBlock logic
            const tripleBackticks = (line.match(/```/g) || []).length;
            const hasCompleteBlock = tripleBackticks > 0 && tripleBackticks % 2 === 0 && line.includes('\n');
            if (singleBacktickCount % 2 === 1 && !hasCompleteBlock) {
                const lastBacktickIndex = line.lastIndexOf('`');
                const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastBacktickIndex);
                // Only complete if there's content after the backtick and it doesn't contain table delimiters
                const contentAfterBacktick = line.substring(lastBacktickIndex + 1, endOfCellOrLine);
                if (contentAfterBacktick.trim().length > 0 && !contentAfterBacktick.includes('|')) {
                    return line.substring(0, endOfCellOrLine) + '`' + line.substring(endOfCellOrLine);
                }
            }
            return line;
        }
    },
    {
        name: 'singleUnderscoreItalic',
        pattern: /[\s\S]*/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Inline countSingleUnderscores logic
            let singleUnderscores = 0;
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '_') {
                    const prevChar = i > 0 ? line[i - 1] : '';
                    const nextChar = i < line.length - 1 ? line[i + 1] : '';
                    if (prevChar === '\\')
                        continue;
                    if (isWithinMathBlock(line, i))
                        continue;
                    if (prevChar &&
                        nextChar &&
                        /[\p{L}\p{N}_]/u.test(prevChar) &&
                        /[\p{L}\p{N}_]/u.test(nextChar)) {
                        continue;
                    }
                    if (prevChar !== '_' && nextChar !== '_') {
                        singleUnderscores++;
                    }
                }
            }
            if (singleUnderscores % 2 === 1) {
                // Inline findFirstSingleUnderscore logic
                let firstSingleUnderscoreIndex = -1;
                for (let i = 0; i < line.length; i++) {
                    if (line[i] === '_' &&
                        line[i - 1] !== '_' &&
                        line[i + 1] !== '_' &&
                        line[i - 1] !== '\\' &&
                        !isWithinMathBlock(line, i)) {
                        const prevChar = i > 0 ? line[i - 1] : '';
                        const nextChar = i < line.length - 1 ? line[i + 1] : '';
                        if (prevChar &&
                            nextChar &&
                            /[\p{L}\p{N}_]/u.test(prevChar) &&
                            /[\p{L}\p{N}_]/u.test(nextChar)) {
                            continue;
                        }
                        firstSingleUnderscoreIndex = i;
                        break;
                    }
                }
                if (firstSingleUnderscoreIndex !== -1) {
                    const endOfCellOrLine = findEndOfCellOrLineContaining(line, firstSingleUnderscoreIndex);
                    return line.substring(0, endOfCellOrLine) + '_' + line.substring(endOfCellOrLine);
                }
            }
            return line;
        }
    },
    {
        name: 'subscript',
        pattern: /~/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Inline countSingleTildes logic
            let singleTildes = 0;
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '~') {
                    const prevChar = i > 0 ? line[i - 1] : '';
                    const nextChar = i < line.length - 1 ? line[i + 1] : '';
                    if (prevChar === '\\')
                        continue;
                    if (prevChar !== '~' && nextChar !== '~')
                        singleTildes++;
                }
            }
            if (singleTildes % 2 === 1) {
                const lastTildeIndex = line.lastIndexOf('~');
                if (lastTildeIndex !== -1 && !isWithinMathBlock(line, lastTildeIndex)) {
                    const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastTildeIndex);
                    // Only complete if there's content after the tilde
                    const contentAfterTilde = line.substring(lastTildeIndex + 1, endOfCellOrLine);
                    if (contentAfterTilde.trim().length > 0) {
                        return line.substring(0, endOfCellOrLine) + '~' + line.substring(endOfCellOrLine);
                    }
                }
            }
            return line;
        }
    },
    {
        name: 'superscript',
        pattern: /\^/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Inline countSingleCarets logic
            let singleCarets = 0;
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '^') {
                    const prevChar = i > 0 ? line[i - 1] : '';
                    if (prevChar === '\\')
                        continue;
                    if (!isWithinFootnoteRef(line, i))
                        singleCarets++;
                }
            }
            if (singleCarets % 2 === 1) {
                const lastCaretIndex = line.lastIndexOf('^');
                if (lastCaretIndex !== -1 &&
                    !isWithinMathBlock(line, lastCaretIndex) &&
                    !isWithinFootnoteRef(line, lastCaretIndex)) {
                    const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastCaretIndex);
                    // Only complete if there's content after the caret
                    const contentAfterCaret = line.substring(lastCaretIndex + 1, endOfCellOrLine);
                    if (contentAfterCaret.trim().length > 0) {
                        return line.substring(0, endOfCellOrLine) + '^' + line.substring(endOfCellOrLine);
                    }
                }
            }
            return line;
        }
    },
    {
        name: 'inlineMath',
        pattern: /\$/,
        skipInBlockTypes: ['code', 'math'],
        handler: ({ line }) => {
            // Inline countSingleDollarSigns logic
            let singleDollars = 0;
            for (let i = 0; i < line.length; i++) {
                if (line[i] === '$') {
                    const prevChar = i > 0 ? line[i - 1] : '';
                    const nextChar = i < line.length - 1 ? line[i + 1] : '';
                    if (prevChar === '\\')
                        continue;
                    if (prevChar === '$' || nextChar === '$')
                        continue;
                    if (nextChar && /\d/.test(nextChar))
                        continue;
                    singleDollars++;
                }
            }
            if (singleDollars % 2 === 1) {
                let lastDollarIndex = -1;
                for (let i = line.length - 1; i >= 0; i--) {
                    if (line[i] === '$') {
                        const prevChar = i > 0 ? line[i - 1] : '';
                        const nextChar = i < line.length - 1 ? line[i + 1] : '';
                        if (prevChar !== '\\' &&
                            prevChar !== '$' &&
                            nextChar !== '$' &&
                            nextChar !== '' &&
                            !/\d/.test(nextChar)) {
                            lastDollarIndex = i;
                            break;
                        }
                    }
                }
                if (lastDollarIndex !== -1) {
                    const endOfCellOrLine = findEndOfCellOrLineContaining(line, lastDollarIndex);
                    return line.substring(0, endOfCellOrLine) + '$' + line.substring(endOfCellOrLine);
                }
            }
            return line;
        }
    },
];
