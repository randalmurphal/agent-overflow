import { expect, it } from 'vitest';

// setup.browser.ts gives each test a focused frame. The first test leaves
// the page focus outside the frame; the second starts with it back.

it('can leave the page focus outside the test frame', () => {
  window.parent.focus();
  expect(document.hasFocus()).toBe(false);
});

it('starts in a focused frame after a test that left the focus outside it', () => {
  expect(document.hasFocus()).toBe(true);
});
