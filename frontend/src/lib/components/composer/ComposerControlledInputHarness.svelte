<script lang="ts">
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ComposerInputSurface from './ComposerInputSurface.svelte';
  import type { ComposerInputValueInfo } from './composerInputSurface';

  let { pane, draft }: { pane: ThreadPane; draft: ComposerDraftStore } = $props();

  function handleInput(value: string, { appliedToDraft }: ComposerInputValueInfo): void {
    if (!appliedToDraft) draft.setContent(value);
  }
</script>

<div data-testid="composer-root">
  <ComposerInputSurface
    {pane}
    {draft}
    value={draft.content}
    disabled={false}
    placeholder="Message"
    oninput={handleInput}
    onSubmitEnter={() => {}}
  />
</div>
