// R1 is a rule about the whole app, so it is asserted as a rule here rather
// than re-checked per component: exactly one signal may be amber, exactly one
// may be red, and nothing may pulse. The 2026-08-15 amendment (§13 fourth
// pass) adds exactly two clarity hints on top — the done GLYPH is green and
// the now-marker is accent — without touching the attention hues, and the
// hints are pinned here as exactly-one rules for the same drift reason.

import { describe, expect, it } from 'vitest';
import { runMapTone } from './workflowRunMap';
import type { RunMapSignal } from './workflowRunMap';
import { runMapNodeStyle } from './workflowRunMapStyle';

const SIGNALS: RunMapSignal[] = [
  'done', 'running', 'pending', 'failed', 'dropped', 'parked', 'ghost', 'unknown',
];

describe('runMapNodeStyle', () => {
  it('reserves amber for human-blocked and red for failed, and leaves the rest neutral', () => {
    const amber = SIGNALS.filter((signal) => runMapNodeStyle(signal).tone === 'text-warning');
    const red = SIGNALS.filter((signal) => runMapNodeStyle(signal).tone === 'text-error');
    expect(amber).toEqual(['parked']);
    expect(red).toEqual(['failed']);
  });

  it('puts the only glow on the only human-blocked signal', () => {
    expect(SIGNALS.filter((signal) => runMapNodeStyle(signal).glow !== '')).toEqual(['parked']);
    expect(runMapNodeStyle('parked').glow).toBe('status-glow-warning');
  });

  it('greens exactly the done GLYPH — the label beside it stays neutral', () => {
    const tinted = SIGNALS.filter((signal) => runMapNodeStyle(signal).glyphTone === 'text-success');
    expect(tinted).toEqual(['done']);
    expect(runMapNodeStyle('done').label).not.toContain('text-success');
    // Every other glyph restates its tone: attention hues arrive through ONE
    // mapping, and a second green would be a second place "done" is decided.
    for (const signal of SIGNALS) {
      if (signal === 'done') continue;
      expect(runMapNodeStyle(signal).glyphTone).toBe(runMapNodeStyle(signal).tone);
    }
  });

  it('fills what happened and leaves unboxed signals fill-less', () => {
    // Reality is a surface (§2): every signal that renders as a box carries a
    // fill; ghost is not boxed at all, and unknown keeps its dotted hairline
    // alone so it cannot read as a settled record.
    const fillless = SIGNALS.filter((signal) => runMapNodeStyle(signal).fill === '');
    expect(fillless).toEqual(['ghost', 'unknown']);
    // Settled work is fill WITHOUT visible border ink; live and attention
    // signals keep a real border on top of their fill.
    expect(runMapNodeStyle('done').border).toBe('border-transparent');
    expect(runMapNodeStyle('running').border).toBe('border-border-strong');
    expect(runMapNodeStyle('failed').border).toBe('border-error');
  });

  it('gives running the spinner and no glyph — weight, never a new hue', () => {
    expect(SIGNALS.filter((signal) => runMapNodeStyle(signal).spinner)).toEqual(['running']);
    expect(runMapNodeStyle('running').glyph).toBe('');
    expect(runMapNodeStyle('running').label).toContain('text-fg');
    expect(SIGNALS.every((signal) => !runMapNodeStyle(signal).label.includes('animate'))).toBe(true);
  });

  it('dashes the border of what has not been reached, and only that', () => {
    expect(SIGNALS.filter((signal) => runMapNodeStyle(signal).border.includes('border-dashed')))
      .toEqual(['ghost']);
    expect(runMapNodeStyle('ghost').label).toBe('text-fg-hint');
  });

  it('answers for every signal — a new one cannot render styleless', () => {
    for (const signal of SIGNALS) {
      const style = runMapNodeStyle(signal);
      expect(style.tone).not.toBe('');
      expect(style.label).not.toBe('');
      expect(style.border).not.toBe('');
      expect(style.spinner || style.glyph !== '').toBe(true);
    }
  });

  it('declares each hue ONCE: a coloured label is exactly its tone', () => {
    // The label may step the neutral colour or add weight (§2's emphasis is
    // typographic). It may never restate a hue — that is the second
    // declaration the two R1 meanings would drift apart through.
    for (const signal of SIGNALS) {
      const style = runMapNodeStyle(signal);
      if (style.tone === 'text-warning' || style.tone === 'text-error') {
        expect(style.label).toBe(style.tone);
      }
      expect(style.label.includes('text-warning') || style.label.includes('text-error'))
        .toBe(style.tone === 'text-warning' || style.tone === 'text-error');
    }
  });

  it('gives an unknown engine status its own neutral treatment, never a borrowed one', () => {
    const unknown = runMapNodeStyle('unknown');
    expect(runMapTone('unknown')).toBe('text-fg-muted');
    expect(unknown.glyph).toBe('?');
    // Distinct from every other signal's glyph: it must not read as queued.
    expect(SIGNALS.filter((signal) => runMapNodeStyle(signal).glyph === '?')).toEqual(['unknown']);
    expect([unknown.glow, unknown.spinner]).toEqual(['', false]);
  });

  it('strikes a skipped node so it cannot read as "not yet"', () => {
    // A skipped phase is a ghost by status and the PAST by position (§5.5).
    const ghost = runMapNodeStyle('ghost');
    const skipped = runMapNodeStyle('ghost', true);
    expect(skipped.glyph).not.toBe(ghost.glyph);
    expect(skipped.label).toContain('line-through');
    expect(ghost.label).not.toContain('line-through');
    // Same hue, same border: only its FUTURE is different, not its state.
    expect([skipped.tone, skipped.border]).toEqual([ghost.tone, ghost.border]);
  });
});

