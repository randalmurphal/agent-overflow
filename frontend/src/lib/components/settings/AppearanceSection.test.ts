import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import AppearanceSection from './AppearanceSection.svelte';
import {
  getAppearanceThemes,
  loadAppearance,
  resetAppearanceForTest,
  setAppearance,
} from '../../stores/appearance.svelte';
import { applyTheme, resetThemeApplyForTest } from '../../theme/themeApply.svelte';
import { BUILTIN_UI_THEME_ID, BUILTIN_CODE_THEME_ID } from '../../theme/builtins';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

interface ThemeFile {
  id: string;
  raw: string;
}

function seed(themes: ThemeFile[], warnings: string[] = []): void {
  setBindingMock('GetThemeFiles', async () => ({
    dir: '/home/u/.config/agent-overflow/themes',
    themes,
    appearance: { mode: 'system', uiTheme: 'default', codeTheme: 'github' },
    warnings,
  }));
}

function optionIds(select: HTMLElement): string[] {
  return [...select.querySelectorAll('option')].map((option) => option.value);
}

beforeEach(() => {
  localStorage.clear();
  resetAppearanceForTest();
  resetThemeApplyForTest();
  setBindingMock('SetAppearance', async () => undefined);
  seed([]);
});

afterEach(() => {
  localStorage.clear();
  resetAppearanceForTest();
  resetThemeApplyForTest();
});

describe('<AppearanceSection> — controls', () => {
  it('renders one mode picker and one picker per axis', async () => {
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);

    const mode = getByTestId('settings-theme-mode') as HTMLSelectElement;
    expect(optionIds(mode)).toEqual(['system', 'light', 'dark']);
    expect(mode.value).toBe('system');

    // The two axes are independent: a code palette must be selectable without
    // implying a chrome choice (docs/specs/theme-system.md §6.1).
    expect(optionIds(getByTestId('settings-ui-theme'))).toContain(BUILTIN_UI_THEME_ID);
    expect(optionIds(getByTestId('settings-code-theme'))).toContain(BUILTIN_CODE_THEME_ID);
  });

  it('persists a mode change through the appearance RPC', async () => {
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    const select = getByTestId('settings-theme-mode') as HTMLSelectElement;
    select.value = 'dark';
    await fireEvent.change(select);

    expect(getBindingMock('SetAppearance')!.mock.calls[0][0]).toMatchObject({ mode: 'dark' });
  });

  it('persists each axis independently', async () => {
    seed([
      { id: 'nord', raw: JSON.stringify({ name: 'Nord', dark: { colors: { accent: '#88c0d0' } } }) },
      { id: 'mono', raw: JSON.stringify({ name: 'Mono', dark: { ansi: { 'ansi-fg-31': '#f00' } } }) },
    ]);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);

    const ui = getByTestId('settings-ui-theme') as HTMLSelectElement;
    ui.value = 'nord';
    await fireEvent.change(ui);
    expect(getBindingMock('SetAppearance')!.mock.calls[0][0]).toMatchObject({
      uiTheme: 'nord',
      codeTheme: 'github',
    });

    const code = getByTestId('settings-code-theme') as HTMLSelectElement;
    code.value = 'mono';
    await fireEvent.change(code);
    expect(getBindingMock('SetAppearance')!.mock.calls[1][0]).toMatchObject({
      uiTheme: 'nord',
      codeTheme: 'mono',
    });
  });

  it('offers a file only on the axes its sections claim', async () => {
    seed([
      { id: 'chrome', raw: JSON.stringify({ dark: { colors: { accent: '#88c0d0' } } }) },
      { id: 'syntax', raw: JSON.stringify({ dark: { syntax: { 'syntax-keyword': '#f0f' } } }) },
    ]);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);

    expect(optionIds(getByTestId('settings-ui-theme'))).toContain('chrome');
    expect(optionIds(getByTestId('settings-ui-theme'))).not.toContain('syntax');
    expect(optionIds(getByTestId('settings-code-theme'))).toContain('syntax');
    expect(optionIds(getByTestId('settings-code-theme'))).not.toContain('chrome');
  });

  it('marks single-polarity themes with a glyph and a hover note', async () => {
    seed([
      { id: 'neon', raw: JSON.stringify({ name: 'Neon', dark: { colors: { accent: '#b45cff' } } }) },
      {
        id: 'paper',
        raw: JSON.stringify({ name: 'Paper', dark: { colors: { accent: '#333' } }, light: { colors: { accent: '#666' } } }),
      },
    ]);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    const ui = getByTestId('settings-ui-theme') as HTMLSelectElement;
    const option = (id: string) => ui.querySelector(`option[value="${id}"]`) as HTMLOptionElement;

    expect(option('neon').textContent?.trim()).toBe('Neon ⏾');
    expect(option('neon').title).toContain('Dark only');
    // Two-variant files and the identity default (which carries no variants
    // because it names the cascade, which speaks both modes) get no glyph.
    expect(option('paper').textContent?.trim()).toBe('Paper');
    expect(option('paper').title).toBe('');
    expect(option(BUILTIN_UI_THEME_ID).textContent?.trim()).toBe('Default');
    expect(option(BUILTIN_UI_THEME_ID).title).toBe('');
  });

  it('shows the benched cue only while the mode the UI theme lacks is resolved', async () => {
    seed([
      { id: 'neon', raw: JSON.stringify({ name: 'Neon', dark: { colors: { accent: '#b45cff' } } }) },
    ]);
    await loadAppearance();
    const { getByTestId, queryByTestId } = render(AppearanceSection);

    await setAppearance({ uiTheme: 'neon', mode: 'dark' });
    expect(queryByTestId('settings-ui-theme-benched')).toBeNull();

    await setAppearance({ mode: 'light' });
    const cue = getByTestId('settings-ui-theme-benched');
    expect(cue.getAttribute('title')).toContain('Neon is dark-only');
    expect(cue.getAttribute('title')).toContain('light mode');

    await setAppearance({ uiTheme: BUILTIN_UI_THEME_ID });
    expect(queryByTestId('settings-ui-theme-benched')).toBeNull();
  });

  it('labels every entry with its plain name, the axis default first', async () => {
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    const ui = getByTestId('settings-ui-theme') as HTMLSelectElement;
    const options = [...ui.querySelectorAll('option')];
    // No "(built-in)" suffix noise; the default leads the list.
    expect(options.map((o) => o.textContent)).not.toContainEqual(expect.stringContaining('built-in'));
    expect(options[0]?.value).toBe(BUILTIN_UI_THEME_ID);
  });
});

describe('<AppearanceSection> — broken files', () => {
  it('lists an unselectable file disabled, with its reason, rather than hiding it', async () => {
    // A theme that simply vanishes from the picker is indistinguishable from
    // one that was never written — which is the failure the warnings-as-data
    // path exists to prevent.
    seed([{ id: 'oops', raw: '{ not json' }]);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);

    const ui = getByTestId('settings-ui-theme') as HTMLSelectElement;
    const broken = ui.querySelector('option[value="oops"]') as HTMLOptionElement;
    expect(broken).toBeTruthy();
    expect(broken.disabled).toBe(true);
    expect(broken.title.length).toBeGreaterThan(0);
    // Listed on BOTH axes: a file that parses to nothing claims neither.
    expect(
      (getByTestId('settings-code-theme') as HTMLSelectElement).querySelector(
        'option[value="oops"]',
      ),
    ).toBeTruthy();
  });

  it('renders backend file warnings verbatim', async () => {
    seed([], ['huge.json is larger than 64 KB and was skipped.']);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    expect(getByTestId('settings-theme-warnings').textContent).toContain(
      'huge.json is larger than 64 KB and was skipped.',
    );
  });

  it('renders two identical sentences from two variants without crashing', async () => {
    // The list used to key on the message TEXT. The same typo in both variants
    // produces the same sentence twice, and Svelte throws `each_key_duplicate`
    // on a duplicate key in production — so the surface that exists to explain
    // a broken theme crashed on the input it explains.
    seed([
      {
        id: 'twice',
        raw: JSON.stringify({
          dark: { colors: { nonsuch: '#111111' } },
          light: { colors: { nonsuch: '#eeeeee' } },
        }),
      },
    ]);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    const block = getByTestId('settings-theme-warnings');
    const items = [...block.querySelectorAll('li li')];
    const nonsuch = items.filter((item) => item.textContent?.includes('"nonsuch"'));
    expect(nonsuch).toHaveLength(2);
    // …and the two are distinguishable to a human, not just to the keyer.
    expect(nonsuch[0]!.textContent).toContain('dark.colors.nonsuch');
    expect(nonsuch[1]!.textContent).toContain('light.colors.nonsuch');
  });

  it('reports a typo in a file that is NOT selected', async () => {
    // "I wrote a theme and nothing happened": the applier only ever sees the
    // two selected themes, so an unselected file used to report nowhere.
    seed([
      { id: 'unselected', raw: JSON.stringify({ dark: { colors: { srface1: '#111111' } } }) },
    ]);
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    const text = getByTestId('settings-theme-warnings').textContent ?? '';
    expect(text).toContain('unselected');
    expect(text).toContain('srface1');
  });

  it('reports each warning once when a file is both loaded and selected', async () => {
    seed([
      {
        id: 'picked',
        raw: JSON.stringify({ dark: { colors: { accent: '#88c0d0', srface1: '#111111' } } }),
      },
    ]);
    await loadAppearance();
    await setAppearance({ uiTheme: 'picked' });
    applyTheme({
      mode: 'dark',
      appearance: { uiTheme: 'picked', codeTheme: 'github' },
      themes: getAppearanceThemes(),
      revision: 1,
    });
    const { getByTestId } = render(AppearanceSection);
    const items = [...getByTestId('settings-theme-warnings').querySelectorAll('li li')];
    // The applier's list and the per-file list overlap on the selected theme.
    expect(items.filter((item) => item.textContent?.includes('srface1'))).toHaveLength(1);
  });

  it("attributes the applier's own warnings to the file that caused them", async () => {
    await loadAppearance();
    // happy-dom cannot run the applier's color check, so drive the resolver's
    // own refusal instead: a selection naming a theme that is not there.
    applyTheme({
      mode: 'dark',
      appearance: { uiTheme: 'ghost', codeTheme: 'github' },
      themes: [],
      revision: 1,
    });
    const { getByTestId } = render(AppearanceSection);
    expect(getByTestId('settings-theme-warnings').textContent).toContain('ghost');
  });

  it('says which axis fell back', async () => {
    await loadAppearance();
    applyTheme({
      mode: 'dark',
      appearance: { uiTheme: 'ghost', codeTheme: 'github' },
      themes: [],
      revision: 1,
    });
    const { container } = render(AppearanceSection);
    expect(container.textContent).toContain('interface theme fell back');
  });
});

describe('<AppearanceSection> — degraded session', () => {
  it('names the themes directory when there is one', async () => {
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    expect(getByTestId('settings-theme-dir').textContent).toBe(
      '/home/u/.config/agent-overflow/themes',
    );
  });

  it('says so, and keeps the built-ins selectable, when the RPCs are refused', async () => {
    setBindingMock('GetThemeFiles', async () => {
      throw Object.assign(new Error('nope'), { code: 'method_not_found' });
    });
    await loadAppearance();
    const { container, getByTestId, queryByTestId } = render(AppearanceSection);

    expect(container.textContent).toContain('only the built-in themes');
    expect(queryByTestId('settings-theme-dir')).toBeNull();
    expect(optionIds(getByTestId('settings-ui-theme'))).toContain(BUILTIN_UI_THEME_ID);
  });

  it('distinguishes "no themes directory" from "cannot write the selection"', async () => {
    // The LAN posture: the read is allowed, both writes are local-only.
    await loadAppearance();
    setBindingMock('SetAppearance', async () => {
      throw Object.assign(new Error('nope'), { code: 'method_not_found' });
    });
    await setAppearance({ mode: 'dark' });

    const { getByTestId, queryByTestId } = render(AppearanceSection);
    // The themes directory IS readable here, so the built-ins-only callout
    // would be a lie.
    expect(getByTestId('settings-theme-local-only')).toBeTruthy();
    expect(queryByTestId('settings-theme-dir')).not.toBeNull();
  });

  it('still applies the selection locally in a refused session', async () => {
    setBindingMock('GetThemeFiles', async () => {
      throw Object.assign(new Error('nope'), { code: 'method_not_found' });
    });
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    const mode = getByTestId('settings-theme-mode') as HTMLSelectElement;
    mode.value = 'light';
    await fireEvent.change(mode);

    expect(getBindingMock('SetAppearance')!.mock.calls).toHaveLength(0);
    expect(JSON.parse(localStorage.getItem('agent-overflow:appearance')!).mode).toBe('light');
  });
});

describe('<AppearanceSection> — reactivity', () => {
  it('follows a selection changed from anywhere else', async () => {
    await loadAppearance();
    const { getByTestId } = render(AppearanceSection);
    await setAppearance({ mode: 'dark' });
    expect((getByTestId('settings-theme-mode') as HTMLSelectElement).value).toBe('dark');
  });
});
