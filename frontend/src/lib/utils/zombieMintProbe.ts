import { reportFrontendDiagnostic } from './frontendErrorCapture';

/*
 * Receiver for the patched Svelte runtime's zombie-mint probe
 * (patches/svelte@5.56.3.patch). The patch calls
 * `globalThis.__svelteZombieMint(report)` whenever a derived is
 * (re)connected into its dependencies' reactions while the active
 * reader is not CONNECTED — the reader can then never register into the
 * derived's own reactions, leaving it permanently subscribed with no
 * way to disconnect. Heap snapshots show thousands of diff-row prop
 * memos stuck exactly like this, retaining detached DOM (~520 MB in a
 * 12-hour session). The report's stack names the minting call site.
 *
 * The same patch also FIXES the mint: an unconnected derived reader
 * (e.g. an init-time read of a parent prop-expression memo during
 * component init) no longer force-connects what it reads — see the
 * "zombie-mint fix" hunk and
 * src/test/integration/svelte-patch-zombie-leak.test.ts. With the fix
 * applied the probe condition is provably unreachable in 5.56.3:
 * derived readers must be CONNECTED for should_connect, and effects are
 * created CONNECTED (effects.js) and never lose it. The probe is kept
 * as a tripwire that fires only if a future svelte patch re-roll
 * regresses the fix hunk; it cannot observe other leak classes (e.g. a
 * CONNECTED reader aborted by a render throw — upstream #18414).
 *
 * Remove the probe once upstream ships an equivalent fix and the app
 * upgrades past the patched svelte version.
 */

interface ZombieMintReport {
  kind: string;
  readerIsDerived: boolean;
  isUpdatingEffect: boolean;
  isDestroyingEffect: boolean;
  derivedFn: string;
  readerFn: string;
  stack: string;
}

type ProbeGlobal = typeof globalThis & {
  __svelteZombieMint?: (report: ZombieMintReport) => void;
};

export function installZombieMintProbe(): void {
  const g = globalThis as ProbeGlobal;
  if (g.__svelteZombieMint !== undefined) return;
  g.__svelteZombieMint = (report) => {
    const reader = report.readerIsDerived ? 'derived' : 'effect';
    reportFrontendDiagnostic(
      `zombie-mint ${report.kind} reader=${reader}` +
        ` updating=${report.isUpdatingEffect} destroying=${report.isDestroyingEffect}` +
        ` derivedFn=${report.derivedFn} readerFn=${report.readerFn}`,
      report.stack,
    );
  };
}

export function resetZombieMintProbeForTest(): void {
  delete (globalThis as ProbeGlobal).__svelteZombieMint;
}
