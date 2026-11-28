// The methods v0.0.19 lacks are derived, not listed: this build's methods,
// as the generated route table names them, less the release's frozen ones.
import routes from '../../lib/transport/methodRoutes.ts?raw';
import { describe, expect, it } from 'vitest';
import { METHODS_AFTER_V0_0_19, METHODS_AT_V0_0_19 } from './olderComputer';

/** The methods the route table names, one row each: `<id>: '<route>', // <Name>`. */
function routedMethods(): string[] {
  const table = routes.slice(0, routes.indexOf('\n}'));
  return [...table.matchAll(/^\t\d+: '\w+', \/\/ (\w+)$/gm)].map((match) => match[1]).sort();
}

describe('the methods v0.0.19 lacks', () => {
  it("are this build's methods less the release's frozen ones", () => {
    const methods = routedMethods();
    expect(methods.length).toBeGreaterThan(METHODS_AFTER_V0_0_19.length);
    expect(METHODS_AT_V0_0_19.size).toBeGreaterThan(0);
    expect(METHODS_AFTER_V0_0_19).toEqual(methods.filter((name) => !METHODS_AT_V0_0_19.has(name)));
  });

  it('name a method added since and no method the release has', () => {
    expect(METHODS_AFTER_V0_0_19).toContain('RecordActivity');
    expect(METHODS_AFTER_V0_0_19).not.toContain('ArchiveThread');
    expect(METHODS_AT_V0_0_19.has('ArchiveThread')).toBe(true);
  });
});
