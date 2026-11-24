import { describe, expect, it } from 'vitest';
import { PAIRING_TIERS, foldPairing, pairingSettled, pairingTier, type PairingAnswer, type PairingPhase } from './pairingProgress';

const PHASES: PairingPhase[] = PAIRING_TIERS.flat();
const FIRST_END: PairingPhase = 'lapsed';

// Two answers of one phase that differ, so a test can tell which was kept.
const answer = (phase: PairingPhase, which: 'first' | 'second'): PairingAnswer => ({ phase, ending: `${phase}-${which}`, label: `${phase}-${which}` });
const fold = (...answers: PairingAnswer[]) => answers.reduce(foldPairing, null as ReturnType<typeof foldPairing> | null)!;

describe('foldPairing', () => {
  it('places every phase in exactly one tier', () => {
    for (const phase of PHASES) expect(PHASES.filter((p) => p === phase)).toHaveLength(1);
    for (const phase of PHASES) expect(pairingTier(phase)).toBeGreaterThanOrEqual(0);
  });

  // Every ordered pair of phases, in both arrival orders: the held phase is
  // the one later in the host's order, whichever answer arrived first.
  for (const a of PHASES) {
    for (const b of PHASES) {
      const [ta, tb] = [pairingTier(a), pairingTier(b)];
      if (ta === tb) continue;
      const later = ta > tb ? a : b;
      it(`holds ${later} for ${a} and ${b} in either order`, () => {
        expect(fold(answer(a, 'first'), answer(b, 'second')).phase).toBe(later);
        expect(fold(answer(b, 'first'), answer(a, 'second')).phase).toBe(later);
      });
    }
  }

  // Within one tier an undecided phase takes the newer answer, and an end
  // keeps the first one held.
  for (const a of PHASES) {
    for (const b of PHASES) {
      if (pairingTier(a) !== pairingTier(b)) continue;
      const end = pairingTier(a) >= pairingTier(FIRST_END);
      it(`${end ? 'keeps the first' : 'takes the newer'} of ${a} then ${b}`, () => {
        const held = fold(answer(a, 'first'), answer(b, 'second'));
        expect(held.label).toBe(end ? `${a}-first` : `${b}-second`);
      });
    }
  }

  // The host facts the order rests on (pairingProgress.ts): the dialog's
  // deadline stands in for the host's ending; an approval can land after a
  // deadline ending; an allowed device can lose its access, never regain it.
  it('orders the ends as the host does', () => {
    const order: PairingPhase[] = ['ready', 'lapsed', 'ended', 'confirmed', 'accessEnded'];
    for (let i = 1; i < order.length; i++) expect(pairingTier(order[i])).toBeGreaterThan(pairingTier(order[i - 1]));
    // Restore and Remove turn either into the other, so neither outranks it.
    expect(pairingTier('deviceRemoved')).toBe(pairingTier('accessEnded'));
  });

  it('settles on every end and on nothing before it', () => {
    for (const phase of PHASES) {
      expect(pairingSettled(fold(answer(phase, 'first')))).toBe(pairingTier(phase) >= pairingTier(FIRST_END));
    }
  });

  it('returns what it held when an answer changes nothing, and keeps fields an answer does not carry', () => {
    const ready = fold({ phase: 'ready', linkId: 'link-1', number: '135 791', label: 'Laptop' });
    expect(foldPairing(ready, { phase: 'waiting' })).toBe(ready);
    const confirmed = foldPairing(ready, { phase: 'confirmed' });
    expect(confirmed).toMatchObject({ phase: 'confirmed', linkId: 'link-1', label: 'Laptop', redeemed: true });
    expect(fold({ phase: 'waiting' }, { phase: 'ended', ending: 'canceled' }).redeemed).toBe(false);
  });
});
