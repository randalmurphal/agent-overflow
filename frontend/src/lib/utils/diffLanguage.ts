// Map a file path → Shiki language id supported by the diff
// highlighter worker. The worker silently downgrades unknown
// languages to plain text, so missing here just means "no syntax
// highlight" — not a crash.
//
// Keep this list in sync with `LANGUAGE_LOADERS` in
// workers/diffHighlighter.worker.ts. Adding a language requires
// both: a loader entry there + an extension mapping here.

const EXTENSION_TO_LANG: Record<string, string> = {
  ts: 'typescript',
  tsx: 'tsx',
  mts: 'typescript',
  cts: 'typescript',
  js: 'javascript',
  jsx: 'jsx',
  mjs: 'javascript',
  cjs: 'javascript',
  json: 'json',
  jsonc: 'json',
  go: 'go',
  py: 'python',
  pyi: 'python',
  sh: 'bash',
  bash: 'bash',
  zsh: 'bash',
  css: 'css',
  scss: 'css',
  html: 'html',
  htm: 'html',
  svelte: 'svelte',
  md: 'markdown',
  mdx: 'markdown',
  markdown: 'markdown',
  yaml: 'yaml',
  yml: 'yaml',
  diff: 'diff',
  patch: 'diff',
  sql: 'sql',
  rs: 'rust',
};

export function languageFromPath(path: string): string {
  const lastSlash = path.lastIndexOf('/');
  const filename = lastSlash >= 0 ? path.slice(lastSlash + 1) : path;
  const dot = filename.lastIndexOf('.');
  if (dot < 0) return 'plaintext';
  const ext = filename.slice(dot + 1).toLowerCase();
  return EXTENSION_TO_LANG[ext] ?? 'plaintext';
}
