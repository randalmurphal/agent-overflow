// Wall-clock timing contracts, gated.
//
// A relative-cost assertion ("this append must cost a fifth of a full
// re-lex") is a real contract, but it is measured against the host's
// clock and the host is not always idle: the soak rig, the harness
// bench, a parallel vitest project and a perf profile all steal enough
// CPU to fail it while the code is fine. A test that fails for reasons
// unrelated to the code teaches people to ignore it.
//
// So the MEASUREMENT always runs — it exercises the same paths and the
// numbers are reported either way — and only the ASSERTION is gated on
// `AO_PERF_CONTRACT=1`. `make test` sets it, so the contract still runs
// by default in the gate; an ad-hoc `pnpm test` beside a perf rig
// reports the numbers as an annotation instead of failing on them.
//
// This covers wall-clock only. Deterministic WORK budgets (code units
// fed to the lexer, largest input, which fast path was taken) do not
// vary with host load and stay unconditional.

export const PERF_CONTRACT_ENABLED = process.env.AO_PERF_CONTRACT === '1';

export interface PerfContractContext {
  annotate: (message: string) => unknown;
}

/**
 * Run `assert` when the contract is enabled, otherwise annotate the run
 * with the numbers and why they were not asserted.
 */
export async function assertTimingContract(
  ctx: PerfContractContext,
  measurements: string,
  assert: () => void,
): Promise<void> {
  if (PERF_CONTRACT_ENABLED) {
    assert();
    return;
  }
  await ctx.annotate(
    `timing contract not asserted (${measurements}) — `
      + 'wall-clock bounds are unreliable beside a perf rig; '
      + 'set AO_PERF_CONTRACT=1 to enforce them',
  );
}
