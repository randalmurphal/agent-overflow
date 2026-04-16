import type { HighlighterGeneric, BundledLanguage, BundledTheme } from 'shiki';

const LANGUAGES: BundledLanguage[] = [
  'typescript', 'javascript', 'python', 'go', 'rust',
  'bash', 'json', 'html', 'css', 'svelte', 'sql',
  'markdown', 'diff',
];

let highlighter: HighlighterGeneric<BundledLanguage, BundledTheme> | null = null;
let initPromise: Promise<HighlighterGeneric<BundledLanguage, BundledTheme>> | null = null;

async function getHighlighter(): Promise<HighlighterGeneric<BundledLanguage, BundledTheme>> {
  if (highlighter) return highlighter;
  if (initPromise) return initPromise;

  initPromise = import('shiki').then(async ({ createHighlighter }) => {
    const hl = await createHighlighter({
      themes: ['github-dark'],
      langs: LANGUAGES,
    });
    highlighter = hl;
    return hl;
  });

  return initPromise;
}

function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

export async function highlightCode(code: string, lang: string): Promise<string> {
  try {
    const hl = await getHighlighter();
    const loadedLangs = hl.getLoadedLanguages();
    if (!loadedLangs.includes(lang as BundledLanguage)) {
      return `<pre><code>${escapeHtml(code)}</code></pre>`;
    }
    return hl.codeToHtml(code, { lang, theme: 'github-dark' });
  } catch {
    return `<pre><code>${escapeHtml(code)}</code></pre>`;
  }
}
