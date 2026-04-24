import DOMPurify from 'dompurify';
import { marked } from 'marked';

marked.use({
  gfm: true,
  breaks: false,
});

const inlineMathPattern = /(^|[^\\$])\$([^\s$](?:[^$\n]*?[^\s$])?)\$(?!\d)/g;
const blockMathPattern = /^\$\$\s*\n([\s\S]*?)\n\$\$\s*$/gm;
const fencedCodePattern = /(^|\n)(`{3,}|~{3,})[^\n]*\n[\s\S]*?\n\2[ \t]*(?=\n|$)/g;
const inlineCodePattern = /`[^`\n]*`/g;

export function renderMarkdown(source: string): string {
  const protectedSource = protectCode(source);
  const withMath = protectedSource.restore(injectMathMarkers(protectedSource.source));
  const html = marked.parse(withMath, { async: false }) as string;
  return sanitizeRenderedHtml(html);
}

export function sanitizeRenderedHtml(html: string): string {
  return stripUnsafeUrls(DOMPurify.sanitize(html, {
    USE_PROFILES: { html: true },
    ADD_ATTR: ['class'],
  }));
}

export function sanitizeRenderedSvg(svg: string): string {
  return stripUnsafeUrls(DOMPurify.sanitize(svg, {
    USE_PROFILES: { svg: true, svgFilters: true },
    FORBID_TAGS: ['foreignObject', 'script', 'image'],
  }));
}

const URL_ATTRS = ['href', 'src', 'xlink:href'];
const blockedSvgDataUri = /^\s*data:image\/svg\+xml/i;

function stripUnsafeUrls(html: string): string {
  const template = document.createElement('template');
  template.innerHTML = html;
  for (const element of template.content.querySelectorAll('*')) {
    for (const attr of URL_ATTRS) {
      const value = element.getAttribute(attr);
      if (value && blockedSvgDataUri.test(value)) {
        element.removeAttribute(attr);
      }
    }
  }
  return template.innerHTML;
}

function protectCode(source: string): { source: string; restore: (value: string) => string } {
  const segments: string[] = [];
  const store = (segment: string): string => {
    const index = segments.push(segment) - 1;
    return `\uE000CODE${index}\uE001`;
  };

  const withoutFences = source.replace(fencedCodePattern, (match) => store(match));
  const withoutCode = withoutFences.replace(inlineCodePattern, (match) => store(match));

  return {
    source: withoutCode,
    restore(value: string): string {
      return value.replace(/\uE000CODE(\d+)\uE001/g, (_match, rawIndex: string) => {
        return segments[Number(rawIndex)] ?? '';
      });
    },
  };
}

function injectMathMarkers(source: string): string {
  const withBlockMath = source.replace(blockMathPattern, (_match, body: string) => {
    return `<div class="math-display">${escapeHtml(body.trim())}</div>`;
  });

  return withBlockMath.replace(
    inlineMathPattern,
    (_match, prefix: string, body: string) =>
      `${prefix}<span class="math-inline">${escapeHtml(body)}</span>`,
  );
}

export function escapeHtml(source: string): string {
  return source
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}
