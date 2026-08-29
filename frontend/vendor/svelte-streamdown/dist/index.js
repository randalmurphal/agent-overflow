export { default as Streamdown } from './Streamdown.svelte';
export { useStreamdown } from './context.svelte.js';
export { theme, shadcnTheme, mergeTheme } from './theme.js';
export { lex, parseBlocks, createParseBlocksCache, updateParseBlockStringMaterialization, incrementalLex, createIncrementalLexCache, createProvenAppend, createMaterializedProvenAppend, matchesProvenAppend } from './marked/index.js';
export { parseIncompleteMarkdown, IncompleteMarkdownParser } from './utils/parse-incomplete-markdown.js';
export { bundledLanguagesInfo, createLanguageSet } from './utils/bundledLanguages.js';
export { acquireDocumentInteraction } from './document-interaction.js';
