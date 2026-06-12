<script lang="ts">
  // Leak shape 1: a DEFAULTED prop compiles to prop() with a fallback,
  // which reads the parent's prop-expression memo during component init
  // even if app code never touches the prop. The init-time read happens
  // while the active reader is an unconnected derived, and on pristine
  // svelte 5.56.3 force-connects everything the memo reads. The prop is
  // deliberately never read from the template, so no connected reader
  // ever subscribes and unmount cannot cascade — keep it that way or the
  // test stops exercising the bug.
  interface Props {
    workspacePath?: string;
    title: string;
    onInit: (workspacePath: string) => void;
  }
  let { workspacePath = '', title, onInit }: Props = $props();
  // The init-time, non-reactive read is the point of this fixture.
  // svelte-ignore state_referenced_locally
  onInit(workspacePath);
</script>

<div>{title}</div>
