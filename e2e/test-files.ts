// Which files each Playwright project of the gate runs. playwright.config.ts
// builds its projects from these, and the wait scan (scripts/check-waits.ts)
// reads the same rules, so the files the scan checks are the files the
// runner runs. Plain data: the scan loads it without loading Playwright.
//
// Compact is a layout mode of the one app (frontend/AGENTS.md § Compact),
// so it is a second PROJECT over the same harness, not a second suite: the
// `compact-*` specs run under a phone descriptor, everything else under a
// desktop one. Playwright matches these patterns against the absolute path,
// so they anchor on the file name: a checkout whose directory names contain
// `compact-` must not move every spec into the compact project.

export const TEST_DIR = './tests';

/**
 * Operator-run investigation drivers (`*.manual.spec.ts`) are never part of
 * the gate: they need locally generated, gitignored fixtures and they leave
 * deliberately extreme state behind. `playwright.manual.config.ts` runs one.
 */
export const MANUAL_SPEC = /.*\.manual\.spec\.ts$/;

export interface ProjectFileRules {
  testMatch?: RegExp | RegExp[];
  testIgnore?: RegExp | RegExp[];
}

export const PROJECT_FILES = {
  desktop: { testIgnore: [MANUAL_SPEC, /[\\/]compact-[^\\/]*\.spec\.ts$/] },
  compact: { testMatch: /[\\/]compact-[^\\/]*\.spec\.ts$/ },
  'mobile-webkit': { testMatch: /[\\/]compact-browser-lock\.spec\.ts$/ },
} satisfies Record<string, ProjectFileRules>;
