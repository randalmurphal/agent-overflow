// Normalizes the MCP elicitation form schema that arrives from Codex into a
// flat list of fields the UI can render. The input shape taxonomy is defined
// upstream at /Users/randy/repos/codex-source/codex-rs/app-server-protocol/
// schema/typescript/v2/McpElicitation*.ts. The output here is a single
// discriminated union so the component doesn't need to know about the many
// schema variants (titled vs untitled enums, legacy enumNames, single vs
// multi-select, etc).
//
// Anything we can't make sense of — bogus `type`, non-object root, mixed
// variants — falls back to an empty field list so the dialog still renders
// (with just Accept / Decline / Cancel) rather than erroring the whole
// the composer approval panel.

export type ElicitationFormatHint = 'email' | 'uri' | 'date' | 'date-time';

export interface ElicitationOption {
  value: string;
  label: string;
}

interface FieldBase {
  /** The property key on the object schema; used as the response content key. */
  name: string;
  /** Human-readable label. Falls back to `name` when no `title` is provided. */
  title: string;
  description?: string;
  required: boolean;
}

export type ElicitationField =
  | (FieldBase & {
      kind: 'string';
      format?: ElicitationFormatHint;
      minLength?: number;
      maxLength?: number;
      default?: string;
    })
  | (FieldBase & {
      kind: 'number';
      integer: boolean;
      minimum?: number;
      maximum?: number;
      default?: number;
    })
  | (FieldBase & {
      kind: 'boolean';
      default?: boolean;
    })
  | (FieldBase & {
      kind: 'select';
      options: ElicitationOption[];
      default?: string;
    })
  | (FieldBase & {
      kind: 'multi-select';
      options: ElicitationOption[];
      minItems?: number;
      maxItems?: number;
      default?: string[];
    });

/**
 * Parse the raw `requestedSchema` that arrived from the backend into a flat
 * list of fields. Accepts either a JSON string, a RawMessage-like
 * byte-escaped string, or a pre-parsed object, so callers don't have to
 * pre-normalize.
 */
export function parseElicitationSchema(schema: unknown): ElicitationField[] {
  const root = coerceObject(schema);
  if (!root) return [];

  if (root['type'] !== 'object') return [];

  const properties = root['properties'];
  if (!isPlainObject(properties)) return [];

  const required = asStringArray(root['required']) ?? [];
  const requiredSet = new Set(required);

  const fields: ElicitationField[] = [];
  for (const [name, rawProp] of Object.entries(properties)) {
    if (isReservedObjectKey(name)) continue;
    if (!isPlainObject(rawProp)) continue;
    const field = normalizeProperty(name, rawProp, requiredSet.has(name));
    if (field) fields.push(field);
  }
  return fields;
}

function isReservedObjectKey(name: string): boolean {
  return name === '__proto__' || name === 'prototype' || name === 'constructor';
}

function coerceObject(input: unknown): Record<string, unknown> | null {
  if (input == null) return null;
  if (typeof input === 'string') {
    try {
      const parsed = JSON.parse(input);
      return isPlainObject(parsed) ? parsed : null;
    } catch {
      return null;
    }
  }
  return isPlainObject(input) ? input : null;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function asStringArray(v: unknown): string[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out: string[] = [];
  for (const entry of v) {
    if (typeof entry === 'string') out.push(entry);
  }
  return out;
}

function asNumber(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function asBoolean(v: unknown): boolean | undefined {
  return typeof v === 'boolean' ? v : undefined;
}

function titleOrName(raw: Record<string, unknown>, name: string): string {
  const t = asString(raw['title']);
  return t && t.length > 0 ? t : name;
}

function normalizeProperty(
  name: string,
  raw: Record<string, unknown>,
  isRequired: boolean,
): ElicitationField | null {
  const base: FieldBase = {
    name,
    title: titleOrName(raw, name),
    description: asString(raw['description']),
    required: isRequired,
  };

  const type = raw['type'];

  if (type === 'boolean') {
    return { ...base, kind: 'boolean', default: asBoolean(raw['default']) };
  }

  if (type === 'number' || type === 'integer') {
    return {
      ...base,
      kind: 'number',
      integer: type === 'integer',
      minimum: asNumber(raw['minimum']),
      maximum: asNumber(raw['maximum']),
      default: asNumber(raw['default']),
    };
  }

  if (type === 'string') {
    // Three enum flavors live under `type: "string"`: untitled (`enum`),
    // titled (`oneOf`), legacy (`enum` + `enumNames`). If any of those
    // discriminators is present, render as a select.
    const options = extractStringSelectOptions(raw);
    if (options) {
      return {
        ...base,
        kind: 'select',
        options,
        default: asString(raw['default']),
      };
    }
    const format = asString(raw['format']);
    const formatHint: ElicitationFormatHint | undefined =
      format === 'email' || format === 'uri' || format === 'date' || format === 'date-time'
        ? format
        : undefined;
    return {
      ...base,
      kind: 'string',
      format: formatHint,
      minLength: asNumber(raw['minLength']),
      maxLength: asNumber(raw['maxLength']),
      default: asString(raw['default']),
    };
  }

  if (type === 'array') {
    return normalizeArrayProperty(base, raw);
  }

  return null;
}

function extractStringSelectOptions(raw: Record<string, unknown>): ElicitationOption[] | null {
  // Titled single-select: `oneOf: [{const, title}]`
  const oneOf = raw['oneOf'];
  if (Array.isArray(oneOf) && oneOf.length > 0) {
    const opts: ElicitationOption[] = [];
    for (const entry of oneOf) {
      if (!isPlainObject(entry)) continue;
      const value = asString(entry['const']);
      if (value === undefined) continue;
      const label = asString(entry['title']) ?? value;
      opts.push({ value, label });
    }
    if (opts.length > 0) return opts;
  }

  // Untitled and legacy single-select: `enum: string[]` (+ optional `enumNames`).
  const enumValues = asStringArray(raw['enum']);
  if (enumValues && enumValues.length > 0) {
    const enumNames = asStringArray(raw['enumNames']);
    return enumValues.map((value, i) => ({
      value,
      label: enumNames?.[i] ?? value,
    }));
  }

  return null;
}

function normalizeArrayProperty(
  base: FieldBase,
  raw: Record<string, unknown>,
): ElicitationField | null {
  const items = raw['items'];
  if (!isPlainObject(items)) return null;

  const options = extractStringSelectOptions(items);
  if (!options) {
    // Arrays without an enum constraint aren't renderable in this UI —
    // the Codex schema guarantees multi-select items are enums, so bail.
    return null;
  }

  return {
    ...base,
    kind: 'multi-select',
    options,
    minItems: asNumber(raw['minItems']),
    maxItems: asNumber(raw['maxItems']),
    default: asStringArray(raw['default']),
  };
}
