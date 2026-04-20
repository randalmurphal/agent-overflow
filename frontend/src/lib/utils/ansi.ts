const ESCAPE_RE = /\u001b\[([0-9;]*)m/g;

type AnsiStyle = {
  bold: boolean;
  fg: string | null;
  bg: string | null;
};

const DEFAULT_STYLE: AnsiStyle = {
  bold: false,
  fg: null,
  bg: null,
};

const FG_CLASS_BY_CODE = new Map<number, string>([
  [30, 'text-zinc-200'],
  [31, 'text-red-400'],
  [32, 'text-emerald-400'],
  [33, 'text-amber-300'],
  [34, 'text-sky-400'],
  [35, 'text-fuchsia-400'],
  [36, 'text-cyan-300'],
  [37, 'text-zinc-50'],
  [90, 'text-zinc-500'],
  [91, 'text-red-300'],
  [92, 'text-emerald-300'],
  [93, 'text-amber-200'],
  [94, 'text-sky-300'],
  [95, 'text-fuchsia-300'],
  [96, 'text-cyan-200'],
  [97, 'text-white'],
]);

const BG_CLASS_BY_CODE = new Map<number, string>([
  [40, 'bg-zinc-900'],
  [41, 'bg-red-950/80'],
  [42, 'bg-emerald-950/80'],
  [43, 'bg-amber-950/80'],
  [44, 'bg-sky-950/80'],
  [45, 'bg-fuchsia-950/80'],
  [46, 'bg-cyan-950/80'],
  [47, 'bg-zinc-200/20'],
  [100, 'bg-zinc-700/80'],
  [101, 'bg-red-700/70'],
  [102, 'bg-emerald-700/70'],
  [103, 'bg-amber-700/70'],
  [104, 'bg-sky-700/70'],
  [105, 'bg-fuchsia-700/70'],
  [106, 'bg-cyan-700/70'],
  [107, 'bg-white/30'],
]);

export function ansiToHtml(input: string): string {
  if (!input.includes('\u001b[')) {
    return escapeHtml(input);
  }

  let output = '';
  let style = { ...DEFAULT_STYLE };
  let lastIndex = 0;

  for (const match of input.matchAll(ESCAPE_RE)) {
    const index = match.index ?? 0;
    const text = input.slice(lastIndex, index);
    output += wrapAnsiText(escapeHtml(text), style);

    const codes = parseSgrCodes(match[1] ?? '');
    style = applySgrCodes(style, codes);
    lastIndex = index + match[0].length;
  }

  output += wrapAnsiText(escapeHtml(input.slice(lastIndex)), style);
  return output;
}

function wrapAnsiText(text: string, style: AnsiStyle): string {
  if (text === '') return '';

  const classes: string[] = [];
  if (style.bold) classes.push('font-semibold');
  if (style.fg) classes.push(style.fg);
  if (style.bg) classes.push(style.bg);
  if (classes.length === 0) return text;

  return `<span class="${classes.join(' ')}">${text}</span>`;
}

function parseSgrCodes(raw: string): number[] {
  if (raw.trim() === '') return [0];
  return raw.split(';').map((part) => Number.parseInt(part, 10)).filter(Number.isFinite);
}

function applySgrCodes(style: AnsiStyle, codes: number[]): AnsiStyle {
  let next = { ...style };
  for (const code of codes) {
    switch (code) {
      case 0:
        next = { ...DEFAULT_STYLE };
        break;
      case 1:
        next.bold = true;
        break;
      case 22:
        next.bold = false;
        break;
      case 39:
        next.fg = null;
        break;
      case 49:
        next.bg = null;
        break;
      default:
        if (FG_CLASS_BY_CODE.has(code)) {
          next.fg = FG_CLASS_BY_CODE.get(code) ?? null;
          continue;
        }
        if (BG_CLASS_BY_CODE.has(code)) {
          next.bg = BG_CLASS_BY_CODE.get(code) ?? null;
        }
        break;
    }
  }
  return next;
}

function escapeHtml(input: string): string {
  return input
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}
