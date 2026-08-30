export { default as Streamdown } from './Streamdown.svelte';
export { useStreamdown } from './context.svelte.js';
export { lex, parseBlocks, createParseBlocksCache, updateParseBlockStringMaterialization, incrementalLex, createIncrementalLexCache, createProvenAppend, createMaterializedProvenAppend, matchesProvenAppend } from './marked/index.js';
export { parseIncompleteMarkdown, IncompleteMarkdownParser } from './utils/parse-incomplete-markdown.js';
export { acquireDocumentInteraction } from './document-interaction.js';
export { STREAMDOWN_LITERAL_HOST, attachStreamdownLiteralHost, streamdownLiteralHostOf } from './literal-host.js';
