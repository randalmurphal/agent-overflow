// Read the sidebar's catalogs in a test that cares about one of them.
//
// The projects, the threads under them and the groups that gather those
// threads are one read (lib/stores/sidebarCatalogs.ts), so a test that wants
// the project store populated reads all three. This installs an empty answer
// for whichever of the three the test did not mock itself, so a test states
// only the catalog it is about, and so an unmocked binding cannot fail the
// read and arm a retry timer the test never expected.
import { getBindingMock, setBindingMock } from '../mocks/bindings-app';
import { loadSidebarCatalogs } from '../../lib/stores/sidebarCatalogs';

const CATALOG_BINDINGS = ['ListProjects', 'ListThreads', 'ListThreadGroups'] as const;

/**
 * Read every attached computer's projects, threads and groups, answering
 * empty for any of the three the test has not mocked. Resolves once the
 * round's thread window has ended, as the boot's does.
 */
export async function readSidebarCatalogs(): Promise<void> {
  for (const name of CATALOG_BINDINGS) {
    if (getBindingMock(name) === undefined) setBindingMock(name, async () => []);
  }
  await loadSidebarCatalogs();
}
