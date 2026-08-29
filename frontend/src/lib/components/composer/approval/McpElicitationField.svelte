<script lang="ts">
  import type { ElicitationField } from '../../../utils/elicitationSchema';
  import { uniqueEachKeys } from '../../../utils/uniqueEachKeys';

  interface Props {
    requestId: string;
    field: ElicitationField;
    value: unknown;
    error: string | undefined;
    onChange: (name: string, value: unknown) => void;
    onToggleOption: (field: ElicitationField, value: string) => void;
  }

  let { requestId, field, value, error, onChange, onToggleOption }: Props = $props();

  // Enum/oneOf values come from the MCP SERVER's schema; nothing upstream
  // promises they are distinct. Keyed straight into the `{#each}` blocks
  // below a repeat throws `each_key_duplicate`, which aborts the update
  // flush and freezes the pane (utils/uniqueEachKeys.ts).
  const optionKeys = $derived(
    uniqueEachKeys(
      field.kind === 'select' || field.kind === 'multi-select' ? field.options : [],
      (option) => option.value,
    ),
  );

  function stringInputType(format: string | undefined): string {
    if (format === 'email') return 'email';
    if (format === 'uri') return 'url';
    if (format === 'date') return 'date';
    if (format === 'date-time') return 'datetime-local';
    return 'text';
  }
</script>

<div>
  <label for="el-{requestId}-{field.name}" class="text-xs font-medium text-text-primary">
    {field.title}
    {#if field.required}<span aria-label="required" class="text-error">*</span>{/if}
  </label>
  {#if field.description}
    <p class="text-[0.625rem] text-text-secondary/80">{field.description}</p>
  {/if}

  {#if field.kind === 'string'}
    <input
      id="el-{requestId}-{field.name}"
      type={stringInputType(field.format)}
      value={(value as string) ?? ''}
      minlength={field.minLength}
      maxlength={field.maxLength}
      data-testid="el-input-{field.name}"
      class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
      oninput={(e) => onChange(field.name, (e.target as HTMLInputElement).value)}
    />
  {:else if field.kind === 'number'}
    <input
      id="el-{requestId}-{field.name}"
      type="number"
      value={(value as number | undefined) ?? ''}
      step={field.integer ? 1 : 'any'}
      min={field.minimum}
      max={field.maximum}
      data-testid="el-input-{field.name}"
      class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
      oninput={(e) => {
        const raw = (e.target as HTMLInputElement).value;
        if (raw === '') {
          onChange(field.name, undefined);
          return;
        }
        const n = field.integer ? parseInt(raw, 10) : parseFloat(raw);
        onChange(field.name, Number.isFinite(n) ? n : undefined);
      }}
    />
  {:else if field.kind === 'boolean'}
    <div class="mt-1 flex items-center gap-2">
      <input
        id="el-{requestId}-{field.name}"
        type="checkbox"
        checked={(value as boolean | undefined) === true}
        data-testid="el-input-{field.name}"
        onchange={(e) => onChange(field.name, (e.target as HTMLInputElement).checked)}
      />
      <label for="el-{requestId}-{field.name}" class="text-xs text-text-primary">
        Enabled
      </label>
    </div>
  {:else if field.kind === 'select'}
    <select
      id="el-{requestId}-{field.name}"
      value={(value as string | undefined) ?? ''}
      data-testid="el-input-{field.name}"
      class="mt-1 w-full text-xs rounded border border-border bg-surface-1 px-2 py-1.5 text-text-primary"
      onchange={(e) => onChange(field.name, (e.target as HTMLSelectElement).value)}
    >
      <option value="">Select…</option>
      {#each field.options as opt, optIndex (optionKeys[optIndex] ?? optIndex)}
        <option value={opt.value}>{opt.label}</option>
      {/each}
    </select>
  {:else if field.kind === 'multi-select'}
    <div class="mt-1 space-y-1" data-testid="el-input-{field.name}">
      {#each field.options as opt, optIndex (optionKeys[optIndex] ?? optIndex)}
        {@const selected = ((value as string[] | undefined) ?? []).includes(opt.value)}
        <label class="flex items-center gap-2 text-xs text-text-primary">
          <input
            type="checkbox"
            checked={selected}
            data-testid="el-option-{field.name}-{opt.value}"
            onchange={() => onToggleOption(field, opt.value)}
          />
          {opt.label}
        </label>
      {/each}
    </div>
  {/if}

  {#if error}
    <p class="mt-1 text-[0.625rem] text-error" data-testid="el-error-{field.name}">
      {error}
    </p>
  {/if}
</div>
