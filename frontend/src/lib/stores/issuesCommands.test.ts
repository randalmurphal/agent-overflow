import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { clearCommandRegistry, isCommandEnabled } from './commandRegistry.svelte';
import { makeCommandContext } from './builtinCommands.svelte';
import { registerIssuesCommands } from './issuesCommands';
import { openTool } from './toolPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { grantBackendScopes, pairWithScopes, resetToLocalPage } from '../../test/helpers/scopes';
import { HOME_BACKEND } from '../transport/backendKey';

describe('issues commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetPaneLayoutForTest();
    setPaneLayoutItemsForTest([]);
    registerIssuesCommands();
  });

  afterEach(() => resetToLocalPage());

  it('offers Issues: Open only while a computer lets this window read issue trackers', async () => {
    await pairWithScopes(['threads:read']);
    expect(isCommandEnabled('issues.toggle', makeCommandContext(null, {}))).toBe(false);
    await grantBackendScopes(HOME_BACKEND, ['threads:read', 'tracker:read']);
    expect(isCommandEnabled('issues.toggle', makeCommandContext(null, {}))).toBe(true);
  });

  it('refuses to open Issues in a window no computer lets read issue trackers', async () => {
    await pairWithScopes(['threads:read']);
    expect(() => openTool('issues')).toThrow(/tracker:read/);
  });
});
