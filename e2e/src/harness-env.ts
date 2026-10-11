// Tests never hand the developer's credentials, provider settings or homes to
// the app under test (root AGENTS.md, Validation). A process the suite starts
// from a product binary inherits only these variables; everything else it
// gets, its launcher sets explicitly. A spec that needs another variable
// passes it as an override rather than widening this list.
const INHERITED_NAMES = [
  'PATH',
  'HOME',
  'TMPDIR',
  'LANG',
  'LANGUAGE',
  'TZ',
  'CI',
  // A windowed or real-browser backend draws through these.
  'DISPLAY',
  'WAYLAND_DISPLAY',
  'XDG_RUNTIME_DIR',
];
const INHERITED_PREFIXES = ['LC_', 'AO_HARNESS_', 'AO_E2E_', 'AO_MOCK'];
// Matched by a prefix above but never inherited: AO_HARNESS_KEEP_HOME makes
// the backend keep the real HOME instead of its temp home (main_harness.go,
// isolateHarnessHome).
const EXCLUDED_NAMES = ['AO_HARNESS_KEEP_HOME'];
// What a Windows process needs to start at all (Winsock fails without
// SystemRoot) and where the runtime finds its temp, home and app data.
const INHERITED_WINDOWS_NAMES = [
  'SystemRoot',
  'windir',
  'ComSpec',
  'PATHEXT',
  'TEMP',
  'TMP',
  'USERPROFILE',
  'APPDATA',
  'LOCALAPPDATA',
  'ProgramData',
];

/**
 * The environment for a product process the suite launches: the inherited
 * variables above taken from `source`, then `overrides`, which pass whatever
 * their names. Windows names match case-insensitively and keep the source's
 * spelling, since Node reports `Path` there.
 */
export function launchEnvironment(
  overrides: Record<string, string> = {},
  source: NodeJS.ProcessEnv = process.env,
  platform: NodeJS.Platform = process.platform,
): Record<string, string> {
  const windows = platform === 'win32';
  const fold = (name: string) => (windows ? name.toUpperCase() : name);
  const names = new Set([...INHERITED_NAMES, ...(windows ? INHERITED_WINDOWS_NAMES : [])].map(fold));
  const prefixes = INHERITED_PREFIXES.map(fold);
  const excluded = new Set(EXCLUDED_NAMES.map(fold));
  const env: Record<string, string> = {};
  for (const [name, value] of Object.entries(source)) {
    if (value === undefined) continue;
    const key = fold(name);
    if (excluded.has(key)) continue;
    if (names.has(key) || prefixes.some((prefix) => key.startsWith(prefix))) env[name] = value;
  }
  for (const [name, value] of Object.entries(overrides)) {
    if (windows) {
      for (const existing of Object.keys(env)) {
        if (fold(existing) === fold(name)) delete env[existing];
      }
    }
    env[name] = value;
  }
  return env;
}
