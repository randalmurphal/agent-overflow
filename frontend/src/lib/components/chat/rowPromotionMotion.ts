// The frame loop of a row promotion: the box of the row that leaves closes
// while the box of the row that replaces it opens, both on the scroll
// spring's velocity law, so the change reads as the same motion as the
// follow that chases it.
//
// Frame-driven on purpose. app.css silences CSS transitions inside the
// timeline, and a transition would move the scroll height on a curve the
// bottom follow cannot see coming.

import { SpringValue } from '../../utils/scroll/motion';
import type { RowPromotionPlan } from './rowPromotionPlan';

const FRAME_MS = 1000 / 60;
/** A frame longer than this advances the springs as if it were this long. */
const MAX_FRAME_MS = 48;
/** The leaving text is gone when its box is this far closed. */
const DEPART_FADE_SPAN = 1 / 1.6;
/** The arriving text starts to show when its box is this far open. */
const ARRIVE_FADE_START = 0.2;

export interface RowPromotionElements {
  /** The timeline row (`[data-row-index]`) that hosts the run. */
  row: HTMLElement;
  /** The box that closes: the leaving row's wrapper, or the whole run. */
  departing: HTMLElement;
}

/**
 * The mounted elements of the promotion `plan` describes, or null when the
 * run or its last row is not in the DOM under `root`.
 */
export function findRowPromotionElements(
  root: ParentNode,
  plan: Pick<RowPromotionPlan, 'itemId' | 'runId' | 'removesRun'>,
): RowPromotionElements | null {
  const run = root.querySelector<HTMLElement>(`[data-run-id="${CSS.escape(plan.runId)}"]`);
  const row = run?.closest<HTMLElement>('[data-row-index]');
  if (!run || !row) return null;
  const leaf = run.querySelector<HTMLElement>(`[data-item-id="${CSS.escape(plan.itemId)}"]`);
  const child = leaf?.closest<HTMLElement>('[data-run-key]');
  if (!child) return null;
  return { row, departing: plan.removesRun ? run : child };
}

export interface RowPromotionMotion {
  departing: HTMLElement;
  /** The box that opens. Its height is this loop's to write. */
  stage: HTMLElement;
  /** The stage's content, laid out at its natural height inside the stage. */
  content: HTMLElement;
  /** A top margin that moves with the departure, or null. */
  margin: { element: HTMLElement; from: number; to: number } | null;
  /** Both boxes landed. The stage is at its natural height. */
  onLanded(): void;
}

/**
 * Run the promotion. Returns a cancel that stops the loop and, when it had
 * not landed, puts back every style it wrote.
 */
export function startRowPromotionMotion(motion: RowPromotionMotion): () => void {
  const { departing, stage, content, margin } = motion;
  const saved = {
    height: departing.style.height,
    overflow: departing.style.overflow,
    opacity: departing.style.opacity,
    margin: margin?.element.style.marginTop ?? '',
  };
  const from = departing.getBoundingClientRect().height;
  let closing = from;
  let opening = 0;
  const closingSpring = new SpringValue();
  const openingSpring = new SpringValue();
  let frame = 0;
  let lastTs = 0;
  let ended = false;

  function write(target: number): void {
    const gone = from > 0 ? 1 - closing / from : 1;
    const open = target > 0 ? opening / target : 1;
    departing.style.height = `${closing}px`;
    stage.style.height = `${opening}px`;
    // Opacity rides the geometry, not the clock: a slow frame never shows
    // a full box with no text in it.
    departing.style.opacity = String(Math.max(0, 1 - gone / DEPART_FADE_SPAN));
    content.style.opacity = String(
      Math.max(0, Math.min(1, (open - ARRIVE_FADE_START) / (1 - ARRIVE_FADE_START))),
    );
    if (margin) margin.element.style.marginTop = `${margin.from + (margin.to - margin.from) * gone}px`;
  }

  function land(): void {
    ended = true;
    // The departing box stays closed: the install removes it.
    stage.style.height = '';
    content.style.opacity = '';
    motion.onLanded();
  }

  function step(ts: number): void {
    frame = 0;
    if (ended) return;
    if (!departing.isConnected || !stage.isConnected) {
      land();
      return;
    }
    const frames = lastTs === 0 ? 1 : Math.min(MAX_FRAME_MS, Math.max(0, ts - lastTs)) / FRAME_MS;
    lastTs = ts;
    // Read before writing: late typesetting can still change the height.
    const target = content.getBoundingClientRect().height;
    closing = closingSpring.step(closing, 0, frames);
    opening = openingSpring.step(opening, target, frames);
    write(target);
    if (closing === 0 && opening === target) {
      land();
      return;
    }
    frame = requestAnimationFrame(step);
  }

  departing.style.overflow = 'hidden';
  write(content.getBoundingClientRect().height);
  frame = requestAnimationFrame(step);

  return () => {
    if (frame !== 0) cancelAnimationFrame(frame);
    frame = 0;
    if (ended) return;
    ended = true;
    departing.style.height = saved.height;
    departing.style.overflow = saved.overflow;
    departing.style.opacity = saved.opacity;
    if (margin) margin.element.style.marginTop = saved.margin;
    stage.style.height = '';
    content.style.opacity = '';
  };
}
