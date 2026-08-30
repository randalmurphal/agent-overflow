<script lang="ts">
  import { createAnimationFrameBatcher } from './animationFrameBatcher';

  const updateFrames = createAnimationFrameBatcher('batcher-browser-update');
  const geometryFrames = createAnimationFrameBatcher(
    'batcher-browser-geometry',
    'before-dom-update',
  );

  let expanded = $state(false);
  let box: HTMLDivElement;

  export function expandAndMeasure(): Promise<number> {
    return new Promise((resolve) => {
      updateFrames.request(() => {
        expanded = true;
      });
      geometryFrames.request(() => {
        resolve(box.getBoundingClientRect().height);
      });
    });
  }
</script>

<div
  bind:this={box}
  data-testid="animation-frame-batcher-box"
  style:height={expanded ? '200px' : '20px'}
></div>
