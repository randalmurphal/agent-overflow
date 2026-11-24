// Every test skipped, and a DOM prototype member left behind: its
// retirement fails it before Vitest publishes its result.
import { it } from 'vitest';

Object.defineProperty(HTMLElement.prototype, 'aoFixtureSkippedLeftover', { value: () => 0, configurable: true });

it.skip('is skipped', () => {});
