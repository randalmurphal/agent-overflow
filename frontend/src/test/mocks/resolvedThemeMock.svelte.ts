// A reactive resolved theme a test can flip, for a `vi.mock` of
// `stores/themeMode.svelte` that keeps the module's other exports:
//
//   vi.mock('../../stores/themeMode.svelte', async (importOriginal) => ({
//     ...(await importOriginal<typeof import('../../stores/themeMode.svelte')>()),
//     getResolvedTheme: (await import('.../resolvedThemeMock.svelte')).getResolvedThemeMock,
//   }));
import type { ResolvedTheme } from '../../lib/stores/themeMode.svelte';

let mode = $state<ResolvedTheme>('dark');

export function getResolvedThemeMock(): ResolvedTheme {
  return mode;
}

export function setResolvedThemeForTest(next: ResolvedTheme): void {
  mode = next;
}
