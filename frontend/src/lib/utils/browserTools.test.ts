import { afterEach, describe, expect, it } from 'vitest';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../test/helpers/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import {
  BROWSER_TOOLS_SERVER,
  backendHasBrowser,
  isBrowserToolMeta,
} from './browserTools';

// The name is stated in `internal/browser/types.go` and repeated in this
// module, so it gets its own case: a rename there that is not made here
// stops every browser row from being recognised, silently.
describe('browser tools', () => {
  it('names the server the Go side registers', () => {
    expect(BROWSER_TOOLS_SERVER).toBe('ao-browser-tools');
  });

  describe('recognising a row', () => {
    it.each([
      ['the browser server', { mcp: { server: BROWSER_TOOLS_SERVER, tool: 'browser_click' } }, true],
      ['another server', { mcp: { server: 'docs', tool: 'lookup' } }, false],
      ['no mcp pair at all', { input: {} }, false],
      ['an mcp value that is not an object', { mcp: 'ao-browser-tools' }, false],
      ['an mcp array', { mcp: [BROWSER_TOOLS_SERVER] }, false],
    ])('%s', (_name, meta, expected) => {
      expect(isBrowserToolMeta(meta as Record<string, unknown>)).toBe(expected);
    });

    it('answers false for a row with no meta', () => {
      expect(isBrowserToolMeta(null)).toBe(false);
    });
  });

  describe('asking a machine', () => {
    afterEach(() => resetStagedBackends());

    it('answers false for a machine that has not said hello', () => {
      stageBackend();
      expect(backendHasBrowser('laptop')).toBe(false);
    });

    it('answers false for a machine that advertises other things', () => {
      stageBackend({
        hello: { capabilities: ['terminal'], backendId: REMOTE_BACKEND_UUID } as never,
      });
      expect(backendHasBrowser('laptop')).toBe(false);
    });

    it('answers true only for a machine that advertises it', () => {
      stageBackend({
        hello: { capabilities: ['terminal', 'browser'], backendId: REMOTE_BACKEND_UUID } as never,
      });
      expect(backendHasBrowser('laptop')).toBe(true);
    });

    it('answers for a machine that is not attached without inventing one', () => {
      expect(backendHasBrowser('never-seen')).toBe(false);
      expect(backendHasBrowser(HOME_BACKEND)).toBe(false);
    });
  });
});
