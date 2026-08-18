import { afterEach, describe, expect, it } from 'vitest';
import {
  popoverCloseRestoresFocus,
  resolvePopoverClipBoundary,
  type PopoverFloatingEl,
} from './popoverOwnership';

describe('resolvePopoverClipBoundary', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('finds the nearest boundary ancestor of the anchor', () => {
    const boundary = document.createElement('div');
    boundary.setAttribute('data-popover-clip-boundary', '');
    const anchor = document.createElement('button');
    boundary.appendChild(anchor);
    document.body.appendChild(boundary);

    expect(resolvePopoverClipBoundary(anchor)).toBe(boundary);
  });

  it('answers null for an anchor with no boundary ancestor', () => {
    const anchor = document.createElement('button');
    document.body.appendChild(anchor);

    expect(resolvePopoverClipBoundary(anchor)).toBeNull();
  });

  it('walks portal hops: a submenu anchored inside a floating element inherits the root trigger boundary', () => {
    // Root trigger lives inside the boundary; its popover is portaled to
    // <body>, so the submenu's anchor (a row inside that floating element)
    // has no boundary ancestor by DOM ancestry alone.
    const boundary = document.createElement('div');
    boundary.setAttribute('data-popover-clip-boundary', '');
    const rootTrigger = document.createElement('button');
    boundary.appendChild(rootTrigger);
    document.body.appendChild(boundary);

    const rootFloating = document.createElement('div') as unknown as PopoverFloatingEl;
    rootFloating.setAttribute('data-popover', '');
    rootFloating.__popoverAnchor = rootTrigger;
    const submenuAnchor = document.createElement('button');
    rootFloating.appendChild(submenuAnchor);
    document.body.appendChild(rootFloating);

    expect(resolvePopoverClipBoundary(submenuAnchor)).toBe(boundary);
  });

  it('stops on a floating element with no anchor pointer instead of looping', () => {
    const orphanFloating = document.createElement('div');
    orphanFloating.setAttribute('data-popover', '');
    const anchor = document.createElement('button');
    orphanFloating.appendChild(anchor);
    document.body.appendChild(orphanFloating);

    expect(resolvePopoverClipBoundary(anchor)).toBeNull();
  });

  it('terminates on a malformed two-node anchor cycle instead of looping forever', () => {
    // floatingA's anchor lives in floatingB and vice versa — a shape no
    // real popover tree produces, but stale `__popoverAnchor` stamps can.
    const floatingA = document.createElement('div') as unknown as PopoverFloatingEl;
    floatingA.setAttribute('data-popover', '');
    const anchorInA = document.createElement('button');
    floatingA.appendChild(anchorInA);
    const floatingB = document.createElement('div') as unknown as PopoverFloatingEl;
    floatingB.setAttribute('data-popover', '');
    const anchorInB = document.createElement('button');
    floatingB.appendChild(anchorInB);
    floatingA.__popoverAnchor = anchorInB;
    floatingB.__popoverAnchor = anchorInA;
    document.body.appendChild(floatingA);
    document.body.appendChild(floatingB);

    expect(resolvePopoverClipBoundary(anchorInA)).toBeNull();
  });

  it('the "none" value terminates the walk: a fixed panel inside a boundary subtree stays unclipped', () => {
    // A Modal rendered by strip content: its DOM sits inside the strip's
    // boundary subtree, but its fixed panel does not scroll with the strip,
    // so pickers it hosts must not inherit the strip's clip edge.
    const boundary = document.createElement('div');
    boundary.setAttribute('data-popover-clip-boundary', '');
    const panel = document.createElement('div');
    panel.setAttribute('data-popover-clip-boundary', 'none');
    const anchor = document.createElement('button');
    panel.appendChild(anchor);
    boundary.appendChild(panel);
    document.body.appendChild(boundary);

    expect(resolvePopoverClipBoundary(anchor)).toBeNull();
  });
});

describe('popoverCloseRestoresFocus', () => {
  it('licenses a restore only for dismissals aimed at the popup itself', () => {
    expect(popoverCloseRestoresFocus(undefined)).toBe(true);
    expect(popoverCloseRestoresFocus('escape')).toBe(true);
    expect(popoverCloseRestoresFocus('tab')).toBe(true);
    expect(popoverCloseRestoresFocus('outside-click')).toBe(false);
    expect(popoverCloseRestoresFocus('anchor-gone')).toBe(false);
  });
});
