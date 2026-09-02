// Every template expression inside a nullable-guarded `{#if}` branch must be
// TOTAL — see frontend/CLAUDE.md → Anti-Patterns.
//
// The hazard: a branch expression that dereferences the guarded nullable AND
// carries at least one OTHER reactive dep compiles to its own render effect.
// That effect can be asked to re-run on the surviving dep after the nullable
// went null and before the `{#if}` tears the branch down, and TypeScript sees
// nothing — inside the branch it has the value narrowed to non-null, which is
// exactly why the class survived review. Production hit it on 2026-08-29:
// `Cannot read properties of null (reading 'y')` from MessageNavRail's preview
// card, whose `style:top` folded `railTop` with `previewAnchor.y`.
//
// Why this is a SOURCE rule and not a component test: Svelte's ordinary
// batched flush is parent-first (`Batch.#traverse` walks the effect tree depth
// first, so the `{#if}` block effect re-runs and destroys its branch before any
// render effect inside that branch is reached). Four reproduction shapes were
// tried against a deliberately un-fixed MessageNavRail — the interleaved
// dying-frame flush, a guard on a separate signal, a nested `flushSync` from a
// user effect, and a mid-pass write — and NONE of them throws; the last is
// refused outright by svelte's own `state_unsafe_mutation`. It takes a
// tree-order violation to expose, which is what a nested `flushSync` inside
// `update_effect` does and what the repo's `flush-caps` patch produces when it
// ABORTS a wedged flush mid-pass. Both exist here in production paths (the
// scroll controller's `withViewportBottomHeld` flushes synchronously from
// inside effects) and neither is stageable from a component test. So the
// durable guard is the SHAPE.
//
// The rule is universal over `src/` and its allowlist is EMPTY, which is the
// claim that the tree carries no instance of the class. The fix for a failure
// is folding the branch's inputs into ONE nullable `$derived` the template
// reads through `?.`, or optional-chaining with a neutral default — never an
// allowlist entry.

import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { join } from 'node:path';
import { SRC_ROOT, expectAllowlistExact, repoPath, scannedSources } from '../test/sourceScan';

/** Everything after the last `</script>`: the template half of the file. */
function templateOf(source: string): string {
  const end = source.lastIndexOf('</script>');
  return end < 0 ? source : source.slice(end + '</script>'.length);
}

function scriptOf(source: string): string {
  const end = source.lastIndexOf('</script>');
  return end < 0 ? '' : source.slice(0, end);
}

/** An identifier NOT in property position — `a.item` is not a read of `item`. */
const IDENTIFIER = /(?<![.\w$])[A-Za-z_$][\w$]*/g;

/**
 * Names whose reads are reactive: rune-backed locals and destructured props.
 * A plain `const` is a constant and can never invalidate an effect, so it is
 * deliberately not collected — counting one as a second dep would flag the
 * ~116 legal single-dep branches in this tree.
 */
function reactiveNames(script: string): Set<string> {
  const names = new Set<string>();
  for (const m of script.matchAll(
    /(?:let|const)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*\$(?:state|derived)\b/g,
  )) {
    names.add(m[1]);
  }
  for (const m of script.matchAll(/(?:let|const)\s*\{([^}]*)\}\s*(?::[^=]+)?=\s*\$props\(\)/g)) {
    for (const part of m[1].split(',')) {
      const name = part.split(':')[0].split('=')[0].trim();
      if (/^[A-Za-z_$][\w$]*$/.test(name)) names.add(name);
    }
  }
  return names;
}

interface IfBlock {
  condition: string;
  body: string;
}

/** `{#if …}` blocks with their bodies, brace- and nesting-aware. */
function ifBlocks(template: string): IfBlock[] {
  const blocks: IfBlock[] = [];
  const opener = /\{#if\s/g;
  let match: RegExpExecArray | null;
  while ((match = opener.exec(template)) !== null) {
    let i = match.index + match[0].length;
    let depth = 1;
    while (i < template.length && depth > 0) {
      if (template[i] === '{') depth++;
      else if (template[i] === '}') depth--;
      i++;
    }
    const condition = template.slice(match.index + match[0].length, i - 1);
    let j = i;
    let nesting = 1;
    while (j < template.length && nesting > 0) {
      const nextOpen = template.indexOf('{#if', j);
      const nextClose = template.indexOf('{/if}', j);
      if (nextClose === -1) break;
      if (nextOpen !== -1 && nextOpen < nextClose) {
        nesting++;
        j = nextOpen + 4;
      } else {
        nesting--;
        j = nextClose + 5;
      }
    }
    blocks.push({ condition, body: template.slice(i, Math.max(i, j - 5)) });
  }
  return blocks;
}

/**
 * Every expression in a body that becomes a render effect: mustaches, attribute
 * and directive values, and `{@const}` initializers. `{@const}` is included on
 * purpose — ThemeSettings's instance of the class WAS a `{@const}` folding
 * the extra dep, read by two attributes.
 */
function renderExpressions(body: string): string[] {
  const found: string[] = [];
  for (let i = 0; i < body.length; i++) {
    if (body[i] !== '{') continue;
    const next = body[i + 1] ?? '';
    if (/[#/:]/.test(next)) continue;
    let depth = 1;
    let j = i + 1;
    while (j < body.length && depth > 0) {
      if (body[j] === '{') depth++;
      else if (body[j] === '}') depth--;
      j++;
    }
    const raw = body.slice(i + 1, j - 1);
    i = j - 1;
    if (next === '@') {
      const constant = /^@const\s+[^=]+=([\s\S]*)$/.exec(raw);
      if (constant) found.push(constant[1]);
      continue;
    }
    found.push(raw);
  }
  return found;
}

/**
 * An arrow BODY does not run at render time — the deref happens on the event,
 * when the branch is demonstrably alive. Neutralize them before judging, or
 * every `onclick={() => x.foo()}` in the tree reads as an offender.
 */
function renderTimeSlice(expression: string): string {
  return expression.replace(/=>\s*\{[\s\S]*\}|=>[^,;]*/g, '=>0');
}

/**
 * The nullables a condition guards the EXISTENCE of: the name appears as a bare
 * operand (`x`, `x !== null`, `x && y`, `!x`). `{#if x.prop}` tests a property
 * of a value it already assumes present and is not this class — that
 * distinction is what takes the scan from 6 false positives to none.
 */
function guardedNullables(condition: string, reactive: Set<string>): string[] {
  const candidates = new Set([...condition.matchAll(IDENTIFIER)].map((m) => m[0]));
  return [...candidates].filter(
    (name) =>
      reactive.has(name)
      && new RegExp(String.raw`(?<![.\w$])${name}(?![\w$.])`).test(condition),
  );
}

interface Scan {
  offenders: Map<string, string[]>;
  /** Legal single-dep derefs, counted so the rule cannot pass vacuously. */
  singleDep: number;
}

function scanTree(): Scan {
  const offenders = new Map<string, string[]>();
  let singleDep = 0;
  for (const file of scannedSources(/\.svelte$/)) {
    const source = readFileSync(file, 'utf8');
    const template = templateOf(source);
    const reactive = reactiveNames(scriptOf(source));
    for (const block of ifBlocks(template)) {
      for (const name of guardedNullables(block.condition, reactive)) {
        const bareDeref = new RegExp(String.raw`(?<![\w$?.])${name}\.`);
        for (const expression of renderExpressions(block.body)) {
          const rendered = renderTimeSlice(expression);
          if (!bareDeref.test(rendered)) continue;
          const extraDeps = [...new Set([...rendered.matchAll(IDENTIFIER)].map((m) => m[0]))]
            .filter((other) => other !== name && reactive.has(other));
          if (extraDeps.length === 0) {
            singleDep++;
            continue;
          }
          const path = repoPath(file);
          const why = `\`${name}\` deref'd bare beside \`${extraDeps.join('`, `')}\` in: `
            + rendered.replace(/\s+/g, ' ').trim().slice(0, 120);
          offenders.set(path, [...(offenders.get(path) ?? []), why]);
        }
      }
    }
  }
  return { offenders, singleDep };
}

describe('nullable-guarded branches render total expressions', () => {
  const scan = scanTree();

  it('finds no branch expression that derefs its guard beside another dep', () => {
    expectAllowlistExact(
      scan.offenders,
      {},
      'A nullable-guarded branch dereferences its guard bare in an expression that carries another reactive dep.',
      'That expression is its own render effect and can run after the value nulls, before the branch tears down. '
        + 'Fold the branch\'s inputs into ONE nullable `$derived` the template reads through `?.`, or optional-chain '
        + 'with a neutral default. The `{#if}` keeps existence semantics; the fallback exists only for the dying frame.',
    );
  });

  it('scanned real guarded branches', () => {
    // Single-dep derefs inside a nullable guard are LEGAL and there are ~116 of
    // them. Counting them is the vacuity guard: if the template parsing or the
    // reactive-name collection ever breaks, this drops to zero and the rule
    // above would pass forever having examined nothing.
    expect(scan.singleDep).toBeGreaterThan(80);
  });

  // Totality is only half the contract. `?.` on its own degrades a MISSING card
  // into a blank one that renders forever, so the eight nullables fixed for the
  // 2026-08-29 crash must still gate existence through their `{#if}`.
  const STILL_GATED: Array<[string, string]> = [
    ['lib/components/chat/MessageNavRail.svelte', 'previewCard'],
    ['lib/components/settings/ThemeSettings.svelte', 'benchedUiTheme'],
    ['lib/components/chat/ProviderStatusBanner.svelte', 'providerStatus'],
    ['lib/components/review/ReviewDiffBody.svelte', 'stickyFile'],
    ['lib/components/composer/ActivityRail.svelte', 'liveTodo'],
    ['lib/components/workflows/WorkflowFailureEvidence.svelte', 'failedCheck'],
    ['lib/components/chat/WorktreeSetupPanel.svelte', 'view'],
    ['lib/components/chat/CommandResultRow.svelte', 'expansion'],
  ];

  for (const [file, name] of STILL_GATED) {
    it(`${file}: \`${name}\` still gates its branch`, () => {
      const template = templateOf(readFileSync(join(SRC_ROOT, ...file.split('/')), 'utf8'));
      const gated = ifBlocks(template).some((block) =>
        new RegExp(String.raw`(?<![.\w$])${name}(?![\w$.])`).test(block.condition),
      );
      expect(gated, `${file} must still gate on \`${name}\` — \`?.\` is not a substitute for the guard`)
        .toBe(true);
    });
  }
});
