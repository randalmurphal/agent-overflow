// The modal's one primary button. Every case asserts the LABEL and the IDS
// together: the whole reason this is one function is that a label describing
// a different set than the click imports is the failure mode.

import { describe, expect, it } from 'vitest';
import { resolveImportCta, type ImportCtaInput } from './sessionImportCta';

function input(over: Partial<ImportCtaInput> = {}): ImportCtaInput {
  return {
    status: 'ready',
    run: null,
    importUngranted: false,
    failedIds: [],
    selection: new Set(),
    filteredIds: new Set(['claude:a', 'codex:b']),
    ...over,
  };
}

describe('resolveImportCta', () => {
  it('offers everything the filters show when nothing is selected', () => {
    expect(resolveImportCta(input())).toEqual({
      targetIds: ['claude:a', 'codex:b'],
      label: 'Import all (2)',
      enabled: true,
    });
  });

  it('switches to the selection, including rows the filters now hide', () => {
    const cta = resolveImportCta(
      input({ selection: new Set(['codex:z']), filteredIds: new Set(['claude:a']) }),
    );
    expect(cta.targetIds).toEqual(['codex:z']);
    expect(cta.label).toBe('Import (1)');
  });

  it('reports progress instead of an action while a run holds the surface', () => {
    const cta = resolveImportCta(input({ run: { active: true, completed: 3, total: 10 } }));
    expect(cta.label).toBe('Importing 3 of 10…');
    expect(cta.enabled).toBe(false);
  });

  it('becomes a retry over exactly the failed rows once a run settles', () => {
    // The store returns failed ids only for a SETTLED run, so their presence
    // is itself the retry condition — no second "is it finished" flag.
    const cta = resolveImportCta(
      input({
        run: { active: false, completed: 2, total: 2 },
        failedIds: ['codex:b'],
        selection: new Set(['claude:a']),
      }),
    );
    expect(cta.targetIds).toEqual(['codex:b']);
    expect(cta.label).toBe('Retry failed (1)');
    expect(cta.enabled).toBe(true);
  });

  it('is disabled when the filters leave nothing, but still says so', () => {
    const cta = resolveImportCta(input({ filteredIds: new Set() }));
    expect(cta.label).toBe('Import all (0)');
    expect(cta.enabled).toBe(false);
  });

  it.each([
    ['a view-only session', input({ importUngranted: true })],
    ['a catalog that is still loading', input({ status: 'loading' })],
    ['a catalog that failed to scan', input({ status: 'error' })],
  ])('refuses %s', (_label, over) => {
    expect(resolveImportCta(over).enabled).toBe(false);
  });

  it('copies the id sets rather than handing back the caller’s', () => {
    const selection = new Set(['claude:a']);
    const cta = resolveImportCta(input({ selection }));
    cta.targetIds.push('mutated');
    expect([...selection]).toEqual(['claude:a']);
  });
});
