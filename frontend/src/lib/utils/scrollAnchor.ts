type MeasuredRowChange = {
  previousHeight: number;
  nextHeight: number;
  rowBottom: number;
  viewportTop: number;
};

export function scrollDeltaForMeasuredRowChange(change: MeasuredRowChange): number {
  if (change.previousHeight <= 0) return 0;

  const delta = Math.ceil(change.nextHeight) - Math.ceil(change.previousHeight);
  if (delta === 0) return 0;

  const previousRowBottom = change.rowBottom - delta;
  if (previousRowBottom > change.viewportTop) return 0;

  return delta;
}
