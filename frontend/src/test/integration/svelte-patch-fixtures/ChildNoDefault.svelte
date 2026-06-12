<script lang="ts">
  // Leak shape 2: NO prop default, but app code reads the memo-backed
  // prop during component init. On pristine svelte 5.56.3 this leaks
  // exactly like the defaulted variant — the init-time unconnected read
  // is the minting ingredient, not the default itself. As with Child,
  // the template must never read workspacePath.
  interface Props {
    workspacePath: string;
    title: string;
    onInit: (workspacePath: string) => void;
  }
  let { workspacePath, title, onInit }: Props = $props();
  // The init-time, non-reactive read is the point of this fixture.
  // svelte-ignore state_referenced_locally
  onInit(workspacePath);
</script>

<div>{title}</div>
