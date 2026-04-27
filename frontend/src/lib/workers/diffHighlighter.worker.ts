/// <reference lib="webworker" />
/*
 * Shiki tokenizer worker for the per-tool diff sidebar. Lives off
 * the main thread so syntax highlighting never blocks scroll/typing.
 *
 * Lifetime is managed by the main-thread pool wrapper
 * (utils/diffHighlighterPool.ts): the worker is lazy-booted on first
 * tokenize request and idle-terminated after 5 minutes. On
 * termination the highlighter and registered grammars are gone;
 * a re-boot incurs the lang-resolution cost again.
 *
 * Engine: shiki/engine/javascript (no WASM). Matches the existing
 * markdown highlighter setup so we don't ship two parallel runtime
 * environments.
 *
 * Themes: github-dark + github-light loaded on init. Theme is sent
 * per-request so tokens are stable even if the user toggles theme
 * mid-tokenization.
 */

import type { HighlighterCore } from 'shiki/core';

interface RequestTokenize {
  id: number;
  kind: 'tokenize';
  lines: string[];
  lang: string;
  theme: 'github-dark' | 'github-light';
}

type Request = RequestTokenize;

type ResponseEnvelope =
  | { id: number; kind: 'tokens'; theme: string; tokens: WireToken[][] }
  | { id: number; kind: 'error'; error: string };

interface WireToken {
  content: string;
  color?: string;
  fontStyle?: number;
}

const TOKENIZE_MAX_LINE_LENGTH = 1000;

let highlighterPromise: Promise<HighlighterCore> | null = null;
const loadedLanguages = new Set<string>();
// Permanently-failed grammar loads — we mark these to avoid a
// retry storm when a grammar import keeps throwing on every line.
// Failed languages render plain (no syntax tinting); the user
// sees correct content with degraded color, not a broken UI.
const failedLanguages = new Set<string>();
const languageLoadPromises = new Map<string, Promise<void>>();

const LANGUAGE_LOADERS: Record<string, () => Promise<{ default: unknown }>> = {
  typescript: () => import('shiki/langs/typescript.mjs'),
  tsx: () => import('shiki/langs/tsx.mjs'),
  javascript: () => import('shiki/langs/javascript.mjs'),
  jsx: () => import('shiki/langs/jsx.mjs'),
  json: () => import('shiki/langs/json.mjs'),
  go: () => import('shiki/langs/go.mjs'),
  python: () => import('shiki/langs/python.mjs'),
  bash: () => import('shiki/langs/bash.mjs'),
  css: () => import('shiki/langs/css.mjs'),
  html: () => import('shiki/langs/html.mjs'),
  markdown: () => import('shiki/langs/markdown.mjs'),
  yaml: () => import('shiki/langs/yaml.mjs'),
  diff: () => import('shiki/langs/diff.mjs'),
  sql: () => import('shiki/langs/sql.mjs'),
  rust: () => import('shiki/langs/rust.mjs'),
  svelte: () => import('shiki/langs/svelte.mjs'),
};

async function getHighlighter(): Promise<HighlighterCore> {
  if (!highlighterPromise) {
    highlighterPromise = (async () => {
      const [
        { createHighlighterCore },
        { createJavaScriptRegexEngine },
        darkModule,
        lightModule,
      ] = await Promise.all([
        import('shiki/core'),
        import('shiki/engine/javascript'),
        import('shiki/themes/github-dark.mjs'),
        import('shiki/themes/github-light.mjs'),
      ]);
      return createHighlighterCore({
        themes: [darkModule.default, lightModule.default],
        langs: [],
        engine: createJavaScriptRegexEngine(),
      });
    })();
  }
  return highlighterPromise;
}

async function ensureLanguage(highlighter: HighlighterCore, lang: string): Promise<void> {
  if (loadedLanguages.has(lang) || failedLanguages.has(lang)) return;
  const loader = LANGUAGE_LOADERS[lang];
  if (!loader) {
    // Unknown language — silently fall through with no tokens
    // (the main thread will render plain text). Mark as loaded so
    // we don't retry on every line.
    loadedLanguages.add(lang);
    return;
  }
  let pending = languageLoadPromises.get(lang);
  if (!pending) {
    pending = (async () => {
      const mod = (await loader()) as { default: unknown };
      const grammar = mod.default;
      if (Array.isArray(grammar)) {
        await highlighter.loadLanguage(...(grammar as Parameters<HighlighterCore['loadLanguage']>));
      } else {
        await highlighter.loadLanguage(grammar as Parameters<HighlighterCore['loadLanguage']>[0]);
      }
      loadedLanguages.add(lang);
    })();
    languageLoadPromises.set(lang, pending);
  }
  try {
    await pending;
  } catch {
    // Permanent grammar load failure — mark and stop retrying.
    // The line still renders, just without syntax tokens.
    failedLanguages.add(lang);
  } finally {
    languageLoadPromises.delete(lang);
  }
}

async function handleTokenize(req: RequestTokenize): Promise<ResponseEnvelope> {
  try {
    const highlighter = await getHighlighter();
    await ensureLanguage(highlighter, req.lang);

    // For unknown languages or empty lines, return plain text tokens
    // (no color). Long lines are skipped per the cap (cheap insurance
    // against minified files crashing the tokenizer).
    const tokens: WireToken[][] = req.lines.map((line) => {
      if (line.length === 0) return [];
      if (line.length > TOKENIZE_MAX_LINE_LENGTH) {
        return [{ content: line }];
      }
      try {
        const result = highlighter.codeToTokens(line, {
          lang: loadedLanguages.has(req.lang) ? (req.lang as Parameters<HighlighterCore['codeToTokens']>[1]['lang']) : 'plaintext',
          theme: req.theme,
        });
        // codeToTokens returns lines, but we passed one line so
        // result.tokens is `[[token1, token2, ...]]`.
        const lineTokens = result.tokens[0] ?? [];
        return lineTokens.map((t) => {
          const out: WireToken = { content: t.content };
          if (t.color) out.color = t.color;
          if (t.fontStyle !== undefined && t.fontStyle !== 0) out.fontStyle = t.fontStyle;
          return out;
        });
      } catch {
        return [{ content: line }];
      }
    });

    return { id: req.id, kind: 'tokens', theme: req.theme, tokens };
  } catch (err) {
    return {
      id: req.id,
      kind: 'error',
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

self.addEventListener('message', (event: MessageEvent<Request>) => {
  const req = event.data;
  if (!req || typeof req !== 'object') return;
  if (req.kind === 'tokenize') {
    void handleTokenize(req).then((response) => {
      (self as unknown as Worker).postMessage(response);
    });
  }
});

export {};
