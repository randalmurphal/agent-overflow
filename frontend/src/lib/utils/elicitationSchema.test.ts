import { describe, expect, it } from 'vitest';
import { parseElicitationSchema, type ElicitationField } from './elicitationSchema';

describe('parseElicitationSchema', () => {
  // ---- Happy path primitives ----

  it('parses a simple string property', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: { name: { type: 'string', title: 'Your name' } },
    });
    expect(fields).toHaveLength(1);
    expect(fields[0]).toMatchObject({
      kind: 'string',
      name: 'name',
      title: 'Your name',
      required: false,
    });
  });

  it('parses a string property with every optional attribute', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        email: {
          type: 'string',
          title: 'Email',
          description: 'Where we send the receipt',
          format: 'email',
          minLength: 3,
          maxLength: 120,
          default: 'a@b.c',
        },
      },
      required: ['email'],
    });
    expect(fields[0]).toEqual<ElicitationField>({
      kind: 'string',
      name: 'email',
      title: 'Email',
      description: 'Where we send the receipt',
      format: 'email',
      minLength: 3,
      maxLength: 120,
      default: 'a@b.c',
      required: true,
    });
  });

  it('accepts every valid string format hint', () => {
    for (const format of ['email', 'uri', 'date', 'date-time'] as const) {
      const fields = parseElicitationSchema({
        type: 'object',
        properties: { f: { type: 'string', format } },
      });
      expect(fields[0]).toMatchObject({ kind: 'string', format });
    }
  });

  it('ignores unknown string formats', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: { f: { type: 'string', format: 'ssn' } },
    });
    expect(fields[0]).toMatchObject({ kind: 'string' });
    expect((fields[0] as { format?: string }).format).toBeUndefined();
  });

  it('parses a number property with bounds and default', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        count: { type: 'number', title: 'Count', minimum: 0, maximum: 100, default: 42 },
      },
    });
    expect(fields[0]).toEqual<ElicitationField>({
      kind: 'number',
      integer: false,
      name: 'count',
      title: 'Count',
      minimum: 0,
      maximum: 100,
      default: 42,
      required: false,
    });
  });

  it('distinguishes integer from number', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        age: { type: 'integer', title: 'Age' },
      },
    });
    expect(fields[0]).toMatchObject({ kind: 'number', integer: true });
  });

  it('parses a boolean property with a default', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        optIn: { type: 'boolean', title: 'Opt in', default: true },
      },
    });
    expect(fields[0]).toEqual<ElicitationField>({
      kind: 'boolean',
      name: 'optIn',
      title: 'Opt in',
      default: true,
      required: false,
    });
  });

  // ---- Enum variants ----

  it('parses an untitled single-select enum', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        color: { type: 'string', enum: ['red', 'green', 'blue'], default: 'red' },
      },
    });
    expect(fields[0]).toEqual<ElicitationField>({
      kind: 'select',
      name: 'color',
      title: 'color',
      options: [
        { value: 'red', label: 'red' },
        { value: 'green', label: 'green' },
        { value: 'blue', label: 'blue' },
      ],
      default: 'red',
      required: false,
    });
  });

  it('parses a legacy enum with enumNames labels', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        region: {
          type: 'string',
          enum: ['us-east', 'us-west'],
          enumNames: ['United States (East)', 'United States (West)'],
        },
      },
    });
    expect(fields[0]).toMatchObject({
      kind: 'select',
      options: [
        { value: 'us-east', label: 'United States (East)' },
        { value: 'us-west', label: 'United States (West)' },
      ],
    });
  });

  it('parses a titled single-select via oneOf', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        role: {
          type: 'string',
          oneOf: [
            { const: 'admin', title: 'Administrator' },
            { const: 'user', title: 'User' },
          ],
        },
      },
    });
    expect(fields[0]).toMatchObject({
      kind: 'select',
      options: [
        { value: 'admin', label: 'Administrator' },
        { value: 'user', label: 'User' },
      ],
    });
  });

  it('prefers oneOf when both oneOf and enum are present', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        x: {
          type: 'string',
          enum: ['a', 'b'],
          oneOf: [{ const: 'x', title: 'X-Ray' }],
        },
      },
    });
    expect(fields[0]).toMatchObject({
      kind: 'select',
      options: [{ value: 'x', label: 'X-Ray' }],
    });
  });

  it('falls back from malformed oneOf to enum', () => {
    // oneOf exists but has no usable entries → parser should keep going and
    // use `enum` as the source of truth.
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        x: {
          type: 'string',
          oneOf: [{ title: 'missing const' }, { const: 42 }], // 42 is not a string
          enum: ['fallback'],
        },
      },
    });
    expect(fields[0]).toMatchObject({
      kind: 'select',
      options: [{ value: 'fallback', label: 'fallback' }],
    });
  });

  it('parses an untitled multi-select (array of string enum)', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        tags: {
          type: 'array',
          items: { type: 'string', enum: ['red', 'green', 'blue'] },
          minItems: 1,
          maxItems: 2,
          default: ['red'],
        },
      },
    });
    expect(fields[0]).toEqual<ElicitationField>({
      kind: 'multi-select',
      name: 'tags',
      title: 'tags',
      options: [
        { value: 'red', label: 'red' },
        { value: 'green', label: 'green' },
        { value: 'blue', label: 'blue' },
      ],
      minItems: 1,
      maxItems: 2,
      default: ['red'],
      required: false,
    });
  });

  it('parses a titled multi-select (array of oneOf)', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        perms: {
          type: 'array',
          items: {
            type: 'string',
            oneOf: [
              { const: 'read', title: 'Read' },
              { const: 'write', title: 'Write' },
            ],
          },
        },
      },
    });
    expect(fields[0]).toMatchObject({
      kind: 'multi-select',
      options: [
        { value: 'read', label: 'Read' },
        { value: 'write', label: 'Write' },
      ],
    });
  });

  // ---- Required tracking ----

  it('marks fields listed in `required` as required', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        a: { type: 'string' },
        b: { type: 'number' },
        c: { type: 'boolean' },
      },
      required: ['a', 'c'],
    });
    expect(fields.find((f) => f.name === 'a')?.required).toBe(true);
    expect(fields.find((f) => f.name === 'b')?.required).toBe(false);
    expect(fields.find((f) => f.name === 'c')?.required).toBe(true);
  });

  it('preserves property order', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        z: { type: 'string' },
        a: { type: 'string' },
        m: { type: 'string' },
      },
    });
    expect(fields.map((f) => f.name)).toEqual(['z', 'a', 'm']);
  });

  // ---- Title fallback ----

  it('falls back to the property name when title is missing', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: { someKey: { type: 'string' } },
    });
    expect(fields[0].title).toBe('someKey');
  });

  it('falls back to the property name when title is empty string', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: { a: { type: 'string', title: '' } },
    });
    expect(fields[0].title).toBe('a');
  });

  // ---- Input coercion (string payload) ----

  it('accepts the schema as a JSON string', () => {
    const fields = parseElicitationSchema(
      JSON.stringify({ type: 'object', properties: { f: { type: 'string' } } }),
    );
    expect(fields).toHaveLength(1);
  });

  it('returns [] for a JSON string that does not parse', () => {
    expect(parseElicitationSchema('not json')).toEqual([]);
  });

  it('returns [] for a JSON string whose root is not an object', () => {
    expect(parseElicitationSchema('42')).toEqual([]);
    expect(parseElicitationSchema('"hello"')).toEqual([]);
    expect(parseElicitationSchema('[]')).toEqual([]);
  });

  // ---- Adversarial / defensive cases ----

  it('returns [] for null, undefined, and non-object inputs', () => {
    expect(parseElicitationSchema(null)).toEqual([]);
    expect(parseElicitationSchema(undefined)).toEqual([]);
    expect(parseElicitationSchema(42)).toEqual([]);
    expect(parseElicitationSchema([])).toEqual([]);
  });

  it('returns [] when root `type` is not "object"', () => {
    expect(parseElicitationSchema({ type: 'string', properties: {} })).toEqual([]);
    expect(parseElicitationSchema({ type: null, properties: {} })).toEqual([]);
    expect(parseElicitationSchema({ properties: {} })).toEqual([]);
  });

  it('returns [] when properties is missing or not a plain object', () => {
    expect(parseElicitationSchema({ type: 'object' })).toEqual([]);
    expect(parseElicitationSchema({ type: 'object', properties: null })).toEqual([]);
    expect(parseElicitationSchema({ type: 'object', properties: 'nope' })).toEqual([]);
    expect(parseElicitationSchema({ type: 'object', properties: [] })).toEqual([]);
  });

  it('skips properties that are not objects', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        ok: { type: 'string' },
        nope: 'stringy-value',
        nopeAgain: null,
        nopeArr: [],
      },
    });
    expect(fields.map((f) => f.name)).toEqual(['ok']);
  });

  it('drops properties with an unknown `type`', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        good: { type: 'string' },
        weird: { type: 'null' },
        worse: { type: 42 },
      },
    });
    expect(fields.map((f) => f.name)).toEqual(['good']);
  });

  it('drops an array property whose items lack an enum constraint', () => {
    // Plain arrays (no enum) aren't representable by this UI.
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        tags: { type: 'array', items: { type: 'string' } },
      },
    });
    expect(fields).toEqual([]);
  });

  it('drops array fields with a missing or non-object items', () => {
    expect(
      parseElicitationSchema({ type: 'object', properties: { a: { type: 'array' } } }),
    ).toEqual([]);
    expect(
      parseElicitationSchema({
        type: 'object',
        properties: { a: { type: 'array', items: 'nope' } },
      }),
    ).toEqual([]);
  });

  it('ignores non-finite numbers in bounds and defaults', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        a: { type: 'number', minimum: NaN, maximum: Infinity, default: -Infinity },
      },
    });
    const field = fields[0] as Extract<ElicitationField, { kind: 'number' }>;
    expect(field.minimum).toBeUndefined();
    expect(field.maximum).toBeUndefined();
    expect(field.default).toBeUndefined();
  });

  it('treats a non-boolean default on a boolean field as absent', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: { x: { type: 'boolean', default: 'yes' } },
    });
    expect(fields[0]).toMatchObject({ kind: 'boolean' });
    expect((fields[0] as { default?: boolean }).default).toBeUndefined();
  });

  it('ignores enum entries that are not strings', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        x: { type: 'string', enum: ['ok', 42, null, { nested: 'no' }, 'fine'] },
      },
    });
    expect(fields[0]).toMatchObject({
      kind: 'select',
      options: [
        { value: 'ok', label: 'ok' },
        { value: 'fine', label: 'fine' },
      ],
    });
  });

  it('ignores `required` entries that are not strings', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: { a: { type: 'string' } },
      required: ['a', 42, null, { name: 'a' }],
    });
    expect(fields[0].required).toBe(true);
  });

  // ---- Unicode / large payload survival ----

  it('preserves unicode in titles and descriptions', () => {
    const fields = parseElicitationSchema({
      type: 'object',
      properties: {
        name: { type: 'string', title: '日本語', description: 'öäü' },
      },
    });
    expect(fields[0].title).toBe('日本語');
    expect(fields[0].description).toBe('öäü');
  });

  it('handles a large number of properties without dropping any', () => {
    const properties: Record<string, unknown> = {};
    for (let i = 0; i < 1000; i++) {
      properties[`p${i}`] = { type: 'string' };
    }
    const fields = parseElicitationSchema({ type: 'object', properties });
    expect(fields).toHaveLength(1000);
  });

  it('does not throw on a deeply nested (but malformed) input', () => {
    expect(() =>
      parseElicitationSchema({
        type: 'object',
        properties: {
          a: { type: 'object', properties: { b: { type: 'string' } } },
        },
      }),
    ).not.toThrow();
    // Nested objects aren't a supported primitive, so they drop out.
    expect(
      parseElicitationSchema({
        type: 'object',
        properties: { a: { type: 'object' } },
      }),
    ).toEqual([]);
  });
});
