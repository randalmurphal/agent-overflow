import { rememberDiagramSource } from './diagramSourceCache';
import { escapeHtml, sanitizeRenderedSvg } from './markdownRender';
import { findPathRanges } from './pathLinkify';
import { OpenInEditor } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { errString } from './errors';

type CodeHighlighter = {
  codeToHtml: (code: string, options: { lang: string; theme: string }) => string;
};

type EnhanceOptions = {
  generation: number;
  renderScope: string;
  isCurrent: (generation: number) => boolean;
  streaming: boolean;
};

let highlighterPromise: Promise<CodeHighlighter> | null = null;

export async function enhanceMarkdown(container: HTMLElement, options: EnhanceOptions): Promise<void> {
  if (options.streaming) {
    return;
  }
  const mermaidBlocks = prepareMermaidBlocks(container, options);
  attachCopyButtons(container);
  await enhanceMath(container, options);
  await enhanceMermaid(mermaidBlocks, options);
  await enhanceCode(container, options);
  // Run path-link enrichment last: enhanceCode replaces <pre> nodes
  // when Shiki succeeds, so deferring keeps us from walking and
  // discarding ranges inside soon-to-be-replaced text. The linkifier
  // skips text inside <pre>/<code> ancestors anyway, but doing it
  // last avoids redundant work on a freshly-replaced subtree.
  enhancePathLinks(container);
}

function attachCopyButtons(container: HTMLElement) {
  for (const pre of container.querySelectorAll('pre')) {
    if (!pre.querySelector(':scope > code')) {
      continue;
    }
    if (pre.querySelector(':scope > button[data-code-copy]')) {
      continue;
    }
    const button = document.createElement('button');
    button.type = 'button';
    button.dataset.codeCopy = 'true';
    button.className = 'code-copy-button';
    button.textContent = 'Copy';
    button.addEventListener('click', async () => {
      const code = pre.querySelector('code');
      await navigator.clipboard.writeText(code?.textContent ?? pre.textContent ?? '');
      button.textContent = 'Copied';
      window.setTimeout(() => {
        button.textContent = 'Copy';
      }, 900);
    });
    pre.prepend(button);
  }
}

async function enhanceMath(container: HTMLElement, options: EnhanceOptions) {
  const mathNodes = Array.from(container.querySelectorAll<HTMLElement>('.math-inline, .math-display'));
  if (mathNodes.length === 0) return;

  const katex = await import('katex');
  await import('katex/dist/katex.min.css');
  if (!options.isCurrent(options.generation)) return;

  for (const node of mathNodes) {
    const displayMode = node.classList.contains('math-display');
    katex.render(node.textContent ?? '', node, {
      displayMode,
      throwOnError: false,
      strict: 'warn',
      trust: false,
    });
    node.classList.add('math-rendered');
  }
}

async function enhanceMermaid(mermaidBlocks: PendingMermaidBlock[], options: EnhanceOptions) {
  if (mermaidBlocks.length === 0) return;

  const mermaid = await loadMermaidRenderer(mermaidBlocks, options);
  if (!mermaid) return;

  for (const block of mermaidBlocks) {
    if (!options.isCurrent(options.generation)) return;

    try {
      const rendered = await mermaid.render(block.id, block.sourceText);
      const sanitizedSvg = sanitizeRenderedSvg(rendered.svg);
      if (options.isCurrent(options.generation)) {
        block.pre.classList.remove('mermaid-pending');
        block.pre.classList.add('mermaid-rendered');
        block.pre.style.minHeight = '';
        block.pre.dataset.renderedMermaid = block.id;
        rememberDiagramSource(block.pre, block.sourceText);
        block.pre.innerHTML = sanitizedSvg;
      }
    } catch {
      restoreMermaidSource(block, options);
    }
  }
}

type MermaidRenderer = {
  initialize: (config: {
    startOnLoad: boolean;
    securityLevel: 'strict';
    theme: 'dark';
    htmlLabels: boolean;
  }) => void;
  render: (id: string, sourceText: string) => Promise<{ svg: string }>;
};

async function loadMermaidRenderer(
  mermaidBlocks: PendingMermaidBlock[],
  options: EnhanceOptions,
): Promise<MermaidRenderer | null> {
  try {
    const mermaid = (await import('mermaid')).default as MermaidRenderer;
    mermaid.initialize({
      startOnLoad: false,
      securityLevel: 'strict',
      theme: 'dark',
      htmlLabels: false,
    });
    return mermaid;
  } catch {
    for (const block of mermaidBlocks) {
      restoreMermaidSource(block, options);
    }
    return null;
  }
}

type PendingMermaidBlock = {
  id: string;
  pre: HTMLPreElement;
  sourceText: string;
};

function prepareMermaidBlocks(container: HTMLElement, options: EnhanceOptions): PendingMermaidBlock[] {
  const codeBlocks = Array.from(
    container.querySelectorAll<HTMLElement>('pre > code.language-mermaid, pre > code.lang-mermaid'),
  );

  return codeBlocks.flatMap((code, blockIndex): PendingMermaidBlock[] => {
    const pre = code.parentElement;
    if (!pre || pre.tagName !== 'PRE') return [];

    const sourceText = code.textContent ?? '';
    const id = `${options.renderScope}-mermaid-${options.generation}-${blockIndex}-${hashString(sourceText)}`;
    pre.classList.add('mermaid', 'mermaid-pending');
    pre.style.minHeight = `${estimateMermaidPlaceholderHeight(sourceText)}px`;
    pre.innerHTML = '<div class="mermaid-placeholder" aria-live="polite">Rendering diagram...</div>';

    return [{ id, pre: pre as HTMLPreElement, sourceText }];
  });
}

function estimateMermaidPlaceholderHeight(sourceText: string): number {
  const lineCount = sourceText.split('\n').length;
  return Math.min(520, Math.max(180, lineCount * 34));
}

function restoreMermaidSource(block: PendingMermaidBlock, options: EnhanceOptions) {
  if (!options.isCurrent(options.generation)) return;

  block.pre.classList.remove('mermaid-pending');
  block.pre.classList.add('mermaid-error');
  block.pre.style.minHeight = '';
  block.pre.innerHTML = `<code class="language-mermaid">${escapeHtml(block.sourceText)}</code>`;
  attachCopyButtons(block.pre.parentElement ?? block.pre);
}

async function enhanceCode(container: HTMLElement, options: EnhanceOptions) {
  const codeBlocks = Array.from(container.querySelectorAll<HTMLElement>('pre > code'));
  const highlightable = codeBlocks.filter((code) => {
    const language = languageFromClass(code.className);
    return language && language !== 'mermaid';
  });
  if (highlightable.length === 0) return;

  const highlighter = await getCodeHighlighter();
  for (const code of highlightable) {
    if (!options.isCurrent(options.generation)) return;

    const language = normalizeLanguage(languageFromClass(code.className));
    if (!language) continue;

    try {
      const highlighted = highlighter.codeToHtml(code.textContent ?? '', {
        lang: language,
        theme: 'github-dark',
      });
      const template = document.createElement('template');
      template.innerHTML = highlighted;
      const highlightedPre = template.content.querySelector('pre');
      const pre = code.parentElement;
      if (pre && highlightedPre && options.isCurrent(options.generation)) {
        pre.replaceWith(highlightedPre);
        attachCopyButtons(highlightedPre);
      }
    } catch {
      // Unknown languages stay as plain code.
    }
  }
}

function getCodeHighlighter(): Promise<CodeHighlighter> {
  highlighterPromise ??= createCodeHighlighter();
  return highlighterPromise;
}

async function createCodeHighlighter(): Promise<CodeHighlighter> {
  const [
    { createHighlighterCore },
    { createJavaScriptRegexEngine },
    themeModule,
    ...languageModules
  ] = await Promise.all([
    import('shiki/core'),
    import('shiki/engine/javascript'),
    import('shiki/themes/github-dark.mjs'),
    import('shiki/langs/typescript.mjs'),
    import('shiki/langs/tsx.mjs'),
    import('shiki/langs/javascript.mjs'),
    import('shiki/langs/jsx.mjs'),
    import('shiki/langs/json.mjs'),
    import('shiki/langs/go.mjs'),
    import('shiki/langs/python.mjs'),
    import('shiki/langs/bash.mjs'),
    import('shiki/langs/css.mjs'),
    import('shiki/langs/html.mjs'),
    import('shiki/langs/markdown.mjs'),
    import('shiki/langs/yaml.mjs'),
    import('shiki/langs/diff.mjs'),
    import('shiki/langs/sql.mjs'),
    import('shiki/langs/rust.mjs'),
  ]);
  return createHighlighterCore({
    themes: [themeModule.default],
    langs: languageModules.flatMap((module) => module.default),
    engine: createJavaScriptRegexEngine(),
  });
}

function languageFromClass(className: string): string {
  const match = className.match(/(?:language|lang)-([a-zA-Z0-9_+-]+)/);
  return match?.[1]?.toLowerCase() ?? '';
}

const languageAliases: Record<string, string> = {
  cjs: 'javascript',
  js: 'javascript',
  mjs: 'javascript',
  ts: 'typescript',
  jsonc: 'json',
  py: 'python',
  sh: 'bash',
  shell: 'bash',
  zsh: 'bash',
  md: 'markdown',
  yml: 'yaml',
  rs: 'rust',
};

function normalizeLanguage(language: string): string {
  return languageAliases[language] ?? language;
}

function hashString(sourceText: string): string {
  let hash = 0;
  for (let index = 0; index < sourceText.length; index += 1) {
    hash = (hash * 31 + sourceText.charCodeAt(index)) | 0;
  }
  return Math.abs(hash).toString(36);
}

// Path-link enrichment.
//
// Walks the rendered markdown for text nodes whose ancestor chain is
// NOT a <pre> (block code) but tolerates being inside an inline <code>
// (so things like ``src/lib/foo.ts`` linkify, but anything inside a
// fenced code block does not). Each matching text node is replaced
// with a sequence of plain text + <a class="editor-link" data-path>...
// fragments. A single document-level click delegate (installed lazily
// on first use) handles the actual binding call.

const EDITOR_LINK_CLASS = 'editor-link';
let pathLinkDelegateInstalled = false;

function ensurePathLinkDelegate(): void {
  if (pathLinkDelegateInstalled) return;
  if (typeof document === 'undefined') return;
  pathLinkDelegateInstalled = true;
  document.addEventListener('click', handlePathLinkClick);
}

function handlePathLinkClick(event: MouseEvent): void {
  const target = event.target;
  if (!(target instanceof HTMLElement)) return;
  const link = target.closest<HTMLElement>(`.${EDITOR_LINK_CLASS}`);
  if (!link) return;
  const path = link.dataset.path;
  if (!path) return;
  // Anchors with href="#" would scroll to the top of the page; cancel
  // the default before the async binding call kicks off.
  event.preventDefault();
  const line = Number(link.dataset.line ?? '0') || 0;
  const col = Number(link.dataset.col ?? '0') || 0;
  void invokePathLink(path, line, col);
}

async function invokePathLink(path: string, line: number, col: number): Promise<void> {
  try {
    await OpenInEditor(path, line, col);
  } catch (err) {
    addToast('error', errString(err));
  }
}

function enhancePathLinks(container: HTMLElement): void {
  ensurePathLinkDelegate();
  // Collect candidate text nodes first so the in-place replacement
  // doesn't disturb the iterator (replaceWith mutates the parent's
  // child list).
  const textNodes: Text[] = [];
  const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      // Only Text nodes pass SHOW_TEXT, but TS still types this as
      // Node. Cast and inspect ancestor chain.
      const text = node as Text;
      const value = text.nodeValue;
      if (!value || value.length < 3) return NodeFilter.FILTER_REJECT;
      if (!hasPathSeparator(value)) return NodeFilter.FILTER_REJECT;
      if (insidePre(text)) return NodeFilter.FILTER_REJECT;
      if (insideEditorLink(text)) return NodeFilter.FILTER_REJECT;
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  let current = walker.nextNode();
  while (current) {
    textNodes.push(current as Text);
    current = walker.nextNode();
  }
  for (const text of textNodes) {
    linkifyTextNode(text);
  }
}

function hasPathSeparator(text: string): boolean {
  // Cheap pre-filter so we don't run the full regex on every prose
  // text node. A path always has at least one `/`.
  return text.indexOf('/') !== -1;
}

function insidePre(node: Node): boolean {
  let cursor: Node | null = node.parentNode;
  while (cursor) {
    if (cursor instanceof HTMLElement && cursor.tagName === 'PRE') return true;
    cursor = cursor.parentNode;
  }
  return false;
}

function insideEditorLink(node: Node): boolean {
  let cursor: Node | null = node.parentNode;
  while (cursor) {
    if (cursor instanceof HTMLElement && cursor.classList.contains(EDITOR_LINK_CLASS)) {
      return true;
    }
    cursor = cursor.parentNode;
  }
  return false;
}

function linkifyTextNode(text: Text): void {
  const value = text.nodeValue ?? '';
  const ranges = findPathRanges(value);
  if (ranges.length === 0) return;
  const parent = text.parentNode;
  if (!parent) return;
  const fragment = document.createDocumentFragment();
  let cursor = 0;
  for (const range of ranges) {
    if (range.start > cursor) {
      fragment.appendChild(document.createTextNode(value.slice(cursor, range.start)));
    }
    const link = document.createElement('a');
    link.className = EDITOR_LINK_CLASS;
    // href="#" gives anchor styling + keyboard activation; the global
    // click delegate cancels the default navigation.
    link.href = '#';
    link.dataset.path = range.path;
    if (range.line) link.dataset.line = String(range.line);
    if (range.col) link.dataset.col = String(range.col);
    link.textContent = value.slice(range.start, range.end);
    fragment.appendChild(link);
    cursor = range.end;
  }
  if (cursor < value.length) {
    fragment.appendChild(document.createTextNode(value.slice(cursor)));
  }
  parent.replaceChild(fragment, text);
}

// Test-only export: lets specs reset delegate state so installation is
// observable across cases. Not part of the public API.
export function __resetPathLinkDelegateForTest(): void {
  if (pathLinkDelegateInstalled && typeof document !== 'undefined') {
    document.removeEventListener('click', handlePathLinkClick);
  }
  pathLinkDelegateInstalled = false;
}
