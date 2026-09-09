import { afterEach, expect, it } from 'vitest';
import { getSidebarJumpThreadIds, getVisibleSidebarThreadIds } from './sidebarThreadOrder';

afterEach(() => { document.body.innerHTML = ''; });

it('numbers front-burner targets across projects without changing cursor navigation', () => {
  document.body.innerHTML = `
    <section>
      <div data-sidebar-thread-id="draft"></div>
      <div data-sidebar-thread-id="front-a" data-sidebar-jump-target></div>
      <div data-sidebar-thread-id="child"></div>
      <div data-sidebar-thread-id="front-b" data-sidebar-jump-target></div>
      <div data-sidebar-thread-id="front-c" data-sidebar-jump-target></div>
      <div data-sidebar-thread-id="back"></div>
      <div data-sidebar-thread-id="unpinned"></div>
    </section>
    <section>
      <div data-sidebar-group-id="group"></div>
      <div data-sidebar-thread-id="member"></div>
      <div data-sidebar-thread-id="front-d" data-sidebar-jump-target></div>
      <div data-sidebar-thread-id="front-e" data-sidebar-jump-target></div>
    </section>`;
  expect(getSidebarJumpThreadIds()).toEqual(['front-a', 'front-b', 'front-c', 'front-d', 'front-e']);
  expect(getVisibleSidebarThreadIds()).toEqual([
    'draft', 'front-a', 'child', 'front-b', 'front-c', 'back', 'unpinned', 'member', 'front-d', 'front-e',
  ]);
});

it('caps jumps at nine distinct targets and ignores empty ids', () => {
  document.body.innerHTML = '<div data-sidebar-thread-id="" data-sidebar-jump-target></div>'
    + Array.from({ length: 12 }, (_, i) =>
      `<div data-sidebar-thread-id="t${i}" data-sidebar-jump-target></div>`
      + `<div data-sidebar-thread-id="t${i}" data-sidebar-jump-target></div>`,
    ).join('');
  expect(getSidebarJumpThreadIds()).toEqual(Array.from({ length: 9 }, (_, i) => `t${i}`));
});

it('returns no targets for an empty or absent sidebar', () => {
  expect(getSidebarJumpThreadIds()).toEqual([]);
  expect(getSidebarJumpThreadIds(null)).toEqual([]);
});
