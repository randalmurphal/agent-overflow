import { afterEach, describe, expect, it } from 'vitest';
import { resetAppearanceForTest, setAppearance } from '../../../stores/appearance.svelte';
import { resetBindingMocks, setBindingMock } from '../../../../test/mocks/bindings-app';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { createMarkdownFindTextCache, markdownFindText } from './markdownFindText';

// A GitHub image hidden in one color scheme ends no run there, so find's
// projection follows the theme, and a held projection is dropped with it.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const SOURCE = '<p>before <img src="https://example.invalid/x.png#gh-dark-mode-only"> after</p>';
const OPTIONS = { embeddedHtml: true, forgeSource: GITHUB };

afterEach(() => {
  resetAppearanceForTest();
  resetBindingMocks();
});

describe('find text of a theme-specific image', () => {
  it('follows the theme, held or fresh', () => {
    setBindingMock('SetAppearance', async () => {});
    void setAppearance({ mode: 'dark' });
    const cache = createMarkdownFindTextCache(OPTIONS);
    const dark = markdownFindText(SOURCE, OPTIONS).runs;
    expect(cache(SOURCE).runs).toEqual(dark);
    void setAppearance({ mode: 'light' });
    const light = markdownFindText(SOURCE, OPTIONS).runs;
    expect(light).not.toEqual(dark);
    expect(light).toHaveLength(1);
    expect(cache(SOURCE).runs).toEqual(light);
  });
});
