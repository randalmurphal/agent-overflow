export { default as Streamdown } from './Streamdown.svelte';
export { useStreamdown, type StreamdownProps } from './context.svelte.js';
export type { Theme } from './theme.js';
export { type Extension, type StreamdownToken, type ParseBlocksCache, type ParseBlocksLexPath, type ParseBlocksLexObserver, type IncrementalLexCache, type IncrementalLexPath, type IncrementalLexObserver, type ProvenAppend, lex, parseBlocks, createParseBlocksCache, updateParseBlockStringMaterialization, incrementalLex, createIncrementalLexCache, createProvenAppend, createMaterializedProvenAppend, matchesProvenAppend } from './marked/index.js';
export { parseIncompleteMarkdown, type Plugin, IncompleteMarkdownParser } from './utils/parse-incomplete-markdown.js';
export { acquireDocumentInteraction, type DocumentInteraction, type InteractionRange } from './document-interaction.js';
export { STREAMDOWN_LITERAL_HOST, attachStreamdownLiteralHost, streamdownLiteralHostOf, type StreamdownLiteralHost, type StreamdownLiteralHostHandle, type StreamdownLiteralHostOwner } from './literal-host.js';
