import ts from 'typescript';

export interface CompositorSourceFinding {
  readonly kind: string;
  readonly line: number;
  readonly source: string;
}

interface FindingPattern {
  readonly kind: string;
  readonly expression: RegExp;
  readonly ignore?: (match: RegExpExecArray) => boolean;
}

const EMPTY_OR_NONE_CSS_VALUE = /^(?:''|""|``|'none'|"none"|`none`)$/;

const FINDING_PATTERNS: readonly FindingPattern[] = [
  {
    kind: 'will-change declaration',
    expression: /\bwill-change\s*:/g,
  },
  {
    kind: 'will-change utility',
    expression: /\bwill-change-(?:\[[^\]]+\]|[a-z0-9_-]+)/gi,
  },
  {
    kind: 'willChange property assignment',
    expression: /\.style\.willChange\s*=\s*([^;\n]+)/g,
    ignore: (match) => EMPTY_OR_NONE_CSS_VALUE.test(match[1].trim()),
  },
  {
    kind: 'will-change setProperty',
    expression: /\.setProperty\(\s*(['"])will-change\1\s*,\s*([^,\)\n]+)/g,
    ignore: (match) => EMPTY_OR_NONE_CSS_VALUE.test(match[2].trim()),
  },
  {
    kind: 'Svelte transform style directive',
    expression: /style:(?:transform|translate|rotate|scale)(?![-\w])/g,
  },
  {
    kind: 'DOM transform property assignment',
    expression: /\.style\.(?:transform|translate|rotate|scale)\s*=\s*([^;\n]+)/g,
    ignore: (match) => EMPTY_OR_NONE_CSS_VALUE.test(match[1].trim()),
  },
  {
    kind: 'transform setProperty',
    expression: /\.setProperty\(\s*(['"])(?:transform|translate|rotate|scale)\1\s*,\s*([^,\)\n]+)/g,
    ignore: (match) => EMPTY_OR_NONE_CSS_VALUE.test(match[2].trim()),
  },
  {
    // Covers CSS declarations, inline style strings, and Web Animations
    // keyframes while excluding parameter annotations such as
    // `transform: string` and properties such as `transform-origin`.
    kind: 'transform declaration or keyframe',
    expression: /(?:^|[{\[,;])\s*(?:transform|translate|rotate|scale)(?![-\w])\s*:/gm,
  },
  {
    kind: 'Tailwind transform utility',
    expression:
      /(?<![\w-])(?:-?translate-[xyz]-(?:\[[^\]]+\]|[a-z0-9_./-]+)|-?rotate-(?:\[[^\]]+\]|[xyz]-[a-z0-9_./-]+|[0-9][a-z0-9_./-]*)|scale-(?:\[[^\]]+\]|[xyz]-[a-z0-9_./-]+|[0-9][a-z0-9_./-]*)|transform-(?:gpu|cpu|none))(?![\w-])/gi,
  },
  {
    kind: 'continuous spin animation',
    expression: /(?<![\w-])animate-spin(?![\w-])/g,
  },
];

/**
 * Blank comments without changing source offsets. The TypeScript scanner
 * understands strings, templates, and regular-expression literals, so a URL
 * or a `//` inside authored content is retained. HTML comments are removed
 * first because Svelte markup sits outside the TypeScript grammar.
 */
function withoutComments(source: string): string {
  const chars = [...source];
  const blank = (start: number, end: number): void => {
    for (let i = start; i < end; i += 1) {
      if (chars[i] !== '\n' && chars[i] !== '\r') chars[i] = ' ';
    }
  };

  for (const match of source.matchAll(/<!--[\s\S]*?-->/g)) {
    blank(match.index, match.index + match[0].length);
  }

  const htmlStripped = chars.join('');
  const scanner = ts.createScanner(
    ts.ScriptTarget.Latest,
    false,
    ts.LanguageVariant.Standard,
    htmlStripped,
  );
  for (let token = scanner.scan(); token !== ts.SyntaxKind.EndOfFileToken; token = scanner.scan()) {
    if (
      token === ts.SyntaxKind.SingleLineCommentTrivia
      || token === ts.SyntaxKind.MultiLineCommentTrivia
    ) {
      blank(scanner.getTokenPos(), scanner.getTextPos());
    }
  }
  return chars.join('');
}

function lineNumberAt(source: string, offset: number): number {
  let line = 1;
  for (let i = 0; i < offset; i += 1) {
    if (source.charCodeAt(i) === 10) line += 1;
  }
  return line;
}

/** Finds authored properties that can create or animate a compositor node. */
export function findCompositorSourceFindings(source: string): CompositorSourceFinding[] {
  const searchable = withoutComments(source);
  const findings: CompositorSourceFinding[] = [];
  for (const pattern of FINDING_PATTERNS) {
    pattern.expression.lastIndex = 0;
    for (let match = pattern.expression.exec(searchable); match; match = pattern.expression.exec(searchable)) {
      if (pattern.ignore?.(match)) continue;
      const line = lineNumberAt(source, match.index);
      findings.push({
        kind: pattern.kind,
        line,
        source: source.split(/\r?\n/, line)[line - 1].trim().replace(/\s+/g, ' '),
      });
    }
  }
  return findings.sort((a, b) => a.line - b.line || a.kind.localeCompare(b.kind));
}
