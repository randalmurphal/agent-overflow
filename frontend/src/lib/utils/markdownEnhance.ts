import { rememberDiagramSource } from './diagramSourceCache';
import { sanitizeRenderedSvg } from './markdownRender';

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
  attachCopyButtons(container);
  await enhanceMath(container, options);
  await enhanceMermaid(container, options);
  await enhanceCode(container, options);
}

function attachCopyButtons(container: HTMLElement) {
  for (const pre of container.querySelectorAll('pre')) {
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

async function enhanceMermaid(container: HTMLElement, options: EnhanceOptions) {
  const mermaidBlocks = Array.from(
    container.querySelectorAll<HTMLElement>('pre > code.language-mermaid, pre > code.lang-mermaid'),
  );
  if (mermaidBlocks.length === 0) return;

  const mermaid = (await import('mermaid')).default;
  mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'dark' });
  for (const [blockIndex, code] of mermaidBlocks.entries()) {
    if (!options.isCurrent(options.generation)) return;

    const pre = code.parentElement;
    if (!pre) continue;

    const sourceText = code.textContent ?? '';
    const id = `${options.renderScope}-mermaid-${options.generation}-${blockIndex}-${hashString(sourceText)}`;
    try {
      const rendered = await mermaid.render(id, sourceText);
      const sanitizedSvg = sanitizeRenderedSvg(rendered.svg);
      if (options.isCurrent(options.generation)) {
        pre.classList.add('mermaid', 'mermaid-rendered');
        pre.dataset.renderedMermaid = id;
        rememberDiagramSource(pre, sourceText);
        pre.innerHTML = sanitizedSvg;
      }
    } catch {
      pre.classList.add('mermaid-error');
    }
  }
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
