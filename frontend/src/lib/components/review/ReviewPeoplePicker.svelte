<script lang="ts">
  import Check from '@lucide/svelte/icons/check';
  import Search from '@lucide/svelte/icons/search';
  import Users from '@lucide/svelte/icons/users';
  import Icon from '../primitives/Icon.svelte';
  import Popover from '../primitives/Popover.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewManageError from './ReviewManageError.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PRPickerOption, PRPickerView } from '../../stores/prManagePicker.svelte';
  import { labelColor } from '../../utils/prManage';
  import { randomId } from '../../utils/randomId';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  // The Details rows' searchable multi-select: a search field over the
  // forge's candidates (members who can be asked to review or be
  // assigned; the project's labels), each checked or not. It is a
  // dialog, not a menu: typing goes to the field, the arrows move
  // through the options and Enter checks one (a held key's repeat does not). Closing it, by
  // Escape, Tab, a click outside or its trigger, applies the change as
  // one write; the store writes nothing when nothing changed.

  interface Props {
    picker: PRPickerView;
    /** The Edit that opened it. */
    anchor: HTMLElement | undefined;
    /** What the field searches, for its placeholder and label. */
    noun: string;
  }

  let { picker, anchor, noun }: Props = $props();

  let input: HTMLInputElement | undefined = $state(undefined);
  let active = $state(0);
  const options = $derived(picker.options);
  const activeIndex = $derived(Math.min(active, Math.max(0, options.length - 1)));

  // The popover shows once it is placed, a frame after it mounts; a
  // hidden field cannot take focus.
  $effect(() => {
    const field = input;
    if (!field) return;
    const frame = requestAnimationFrame(() => field.focus({ preventScroll: true }));
    return () => cancelAnimationFrame(frame);
  });

  function close(reason?: PopoverCloseReason): void {
    const trigger = anchor;
    void picker.close();
    restorePickerFocus(reason, { triggerEl: trigger });
  }

  function toggle(option: PRPickerOption | undefined): void {
    if (option) picker.toggle(option.key);
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.isComposing) return;
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        active = Math.min(activeIndex + 1, options.length - 1);
        break;
      case 'ArrowUp':
        event.preventDefault();
        active = Math.max(activeIndex - 1, 0);
        break;
      case 'Enter':
        event.preventDefault();
        if (!event.repeat) toggle(options[activeIndex]);
        break;
    }
  }

  function optionName(option: PRPickerOption): string {
    if (option.label) return option.label.name;
    return option.person?.name || option.person?.login || '';
  }

  const listId = `review-picker-${randomId()}`;
</script>

<Popover {anchor} open={picker.row !== null} onClose={close} placement="bottom-end" role="dialog" ariaLabel="Edit {noun}" claimTab>
  <div
    class="flex w-72 max-w-[calc(100vw-1rem)] flex-col rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 shadow-menu"
    data-testid="review-picker"
    data-row={picker.row}
  >
    <div class="flex items-center gap-1.5 border-b border-border-subtle px-2.5">
      <Icon icon={Search} size={13} class="shrink-0 text-fg-subtle" />
      <input
        bind:this={input}
        type="text"
        class="h-8 min-w-0 flex-1 bg-transparent text-xs text-fg outline-none placeholder:text-fg-subtle compact:h-10"
        placeholder="Search {noun}"
        aria-label="Search {noun}"
        aria-controls={listId}
        aria-activedescendant={options[activeIndex] ? `${listId}-${activeIndex}` : undefined}
        role="combobox"
        aria-expanded="true"
        maxlength={100}
        data-testid="review-picker-search"
        value={picker.query}
        oninput={(event) => { active = 0; picker.setQuery(event.currentTarget.value); }}
        onkeydown={onKeydown}
      />
      <span class="inline-flex size-4 shrink-0 items-center justify-center text-fg-subtle" aria-hidden="true">
        {#if picker.loading}<SteppedSpinner size={12} />{/if}
      </span>
    </div>
    <div class="max-h-64 overflow-y-auto py-1" id={listId} role="listbox" aria-multiselectable="true" aria-label={noun} data-testid="review-picker-options">
      {#each options as option, index (option.key)}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <div
          id="{listId}-{index}"
          role="option"
          tabindex="-1"
          aria-selected={option.checked}
          aria-disabled={option.locked ? 'true' : undefined}
          title={option.locked ? 'Their review is in, so the request cannot be withdrawn here.' : undefined}
          class="flex h-7 cursor-pointer items-center gap-2 px-2.5 text-xs compact:h-10 {index === activeIndex ? 'bg-surface-2/60' : ''} {option.locked ? 'cursor-default opacity-60' : 'hover:bg-surface-2/40'}"
          data-testid="review-picker-option"
          data-key={option.key}
          data-name={option.label?.name}
          data-checked={option.checked ? 'true' : 'false'}
          onmousedown={(event) => event.preventDefault()}
          onclick={() => { active = index; toggle(option); }}
        >
          <span class="inline-flex size-3.5 shrink-0 items-center justify-center rounded-[3px] border {option.checked ? 'border-accent bg-accent text-surface-0' : 'border-border'}">
            {#if option.checked}<Icon icon={Check} size={10} strokeWidth={3} />{/if}
          </span>
          {#if option.label}
            {@const color = labelColor(option.label)}
            <span class="size-2.5 shrink-0 rounded-full border border-border-subtle {color ? '' : 'bg-surface-2'}" style:background-color={color}></span>
            <span class="min-w-0 flex-1 truncate text-fg">{option.label.name}</span>
          {:else if option.person}
            {#if option.person.team}
              <span class="inline-flex size-4 shrink-0 items-center justify-center text-fg-muted"><Icon icon={Users} size={12} /></span>
            {:else}
              <ReviewAvatar login={option.person.login} name={option.person.name} size={16} />
            {/if}
            <span class="min-w-0 flex-1 truncate text-fg">{optionName(option)}</span>
            {#if option.person.name}
              <span class="max-w-[40%] shrink-0 truncate text-fg-subtle">{option.person.login}</span>
            {/if}
          {/if}
        </div>
      {:else}
        {#if !picker.loading && !picker.error}
          <div class="px-2.5 py-1.5 text-xs text-fg-subtle" data-testid="review-picker-empty">No matches</div>
        {/if}
      {/each}
    </div>
    {#if picker.error}
      <div class="px-2 pb-2">
        <ReviewManageError error={picker.error} testid="review-picker-error" />
      </div>
    {:else if picker.more}
      <div class="border-t border-border-subtle px-2.5 py-1.5 text-[0.6875rem] text-fg-subtle" data-testid="review-picker-more">Type to narrow</div>
    {/if}
  </div>
</Popover>
