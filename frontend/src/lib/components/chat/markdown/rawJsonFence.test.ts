import { describe, expect, it, vi } from 'vitest';
import { createProvenAppend } from '../../../markdown';
import { RawJsonFenceFormatter, isRawJsonSource } from './rawJsonFence';

// A workflow-envelope-shaped document on ONE line: nested objects and
// arrays, empty containers, strings holding every delimiter the printer
// reacts to (braces, brackets, commas, colons, escaped quotes, backslashes,
// backticks, underscores) and the markdown characters that caused the
// original restyle flash.
const ENVELOPE =
  '{"phase":"review","status":"ok","summary":"Use `make check` and _never_ `git reset`","files":[{"path":"a/b_c.ts","notes":[]},{"path":"d{}.go","notes":["has \\"quotes\\" and \\\\ back\\\\slashes","{not:json}","[1,2,3]"]}],"counts":{"errors":0,"warnings":12,"ratio":-1.5},"flags":[true,false,null],"empty":{},"nested":{"deep":{"deeper":{"deepest":["x"]}}}}';

const ENVELOPE_PRETTY = [
  '{',
  '  "phase": "review",',
  '  "status": "ok",',
  '  "summary": "Use `make check` and _never_ `git reset`",',
  '  "files": [',
  '    {',
  '      "path": "a/b_c.ts",',
  '      "notes": []',
  '    },',
  '    {',
  '      "path": "d{}.go",',
  '      "notes": [',
  '        "has \\"quotes\\" and \\\\ back\\\\slashes",',
  '        "{not:json}",',
  '        "[1,2,3]"',
  '      ]',
  '    }',
  '  ],',
  '  "counts": {',
  '    "errors": 0,',
  '    "warnings": 12,',
  '    "ratio": -1.5',
  '  },',
  '  "flags": [',
  '    true,',
  '    false,',
  '    null',
  '  ],',
  '  "empty": {},',
  '  "nested": {',
  '    "deep": {',
  '      "deeper": {',
  '        "deepest": [',
  '          "x"',
  '        ]',
  '      }',
  '    }',
  '  }',
  '}',
].join('\n');

function randomJsonRoots(seed: number, count: number): unknown[] {
  let state = seed >>> 0;
  const random = (): number => {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0;
    return state;
  };
  const scalar = (): null | boolean | number | string => {
    switch (random() % 4) {
      case 0: return null;
      case 1: return (random() & 1) === 1;
      case 2: return ((random() % 2_000_001) - 1_000_000) / 100;
      default: return [
        'plain', 'quote " slash \\', 'line\nfeed', 'café 漢字 😀',
        '{}[],:`_*', '', 'tabs\tand\rreturns',
      ][random() % 7];
    }
  };
  const value = (depth: number): unknown => {
    if (depth === 0 || random() % 3 === 0) return scalar();
    if ((random() & 1) === 0) {
      return Array.from({ length: random() % 5 }, () => value(depth - 1));
    }
    const object: Record<string, unknown> = {};
    const keys = ['alpha', 'b"eta', 'slash\\key', 'unicode-漢', 'empty'];
    for (let index = 0, length = random() % 5; index < length; index++) {
      object[`${keys[random() % keys.length]}-${index}`] = value(depth - 1);
    }
    return object;
  };

  const roots: unknown[] = [];
  for (let index = 0; index < count; index++) {
    // Object roots are always classified. Array roots start with a string,
    // object, array, or are empty, matching the intentional citation guard.
    if ((random() & 1) === 0) {
      roots.push({ root: value(4), index });
    } else {
      const first = [String(index), { index }, [index]][random() % 3];
      roots.push(random() % 5 === 0
        ? []
        : [first, ...Array.from({ length: random() % 5 }, () => value(3))]);
    }
  }
  return roots;
}

function settled(source: string): string {
  return new RawJsonFenceFormatter().render(source, false);
}

describe('isRawJsonSource', () => {
  it('accepts the shapes a schema answer starts with', () => {
    for (const s of ['{"a":1}', '{}', '[{"a":1}]', '["a"]', '[[1]]', '[]', '  \n{"a":1}', '{', '[', '{ \n "a": 1 }']) {
      expect(isRawJsonSource(s), s).toBe(true);
    }
  });

  it('rejects prose, fenced code, and things that merely start with a brace', () => {
    for (const s of ['', '   ', 'hello', '```json\n{"a":1}\n```', '{a: 1}', '[1, 2]', '{{template}}', '- item', '{ foo']) {
      expect(isRawJsonSource(s), s).toBe(false);
    }
  });
});

describe('RawJsonFenceFormatter', () => {
  it('returns a non-JSON source unchanged, same identity', () => {
    const f = new RawJsonFenceFormatter();
    const prose = 'Just **prose** with `code`.';
    expect(f.render(prose, true)).toBe(prose);
    expect(f.render(prose, false)).toBe(prose);
  });

  it('pretty-prints a single-line document into a closed json fence', () => {
    expect(settled(ENVELOPE)).toBe('```json\n' + ENVELOPE_PRETTY + '\n```');
  });

  it('round-trips: the formatted text parses back to the same value', () => {
    const body = settled(ENVELOPE).slice('```json\n'.length, -'\n```'.length);
    expect(JSON.parse(body)).toEqual(JSON.parse(ENVELOPE));
  });

  it('normalizes already-pretty input to the same output', () => {
    expect(settled(JSON.stringify(JSON.parse(ENVELOPE), null, 4))).toBe(settled(ENVELOPE));
    expect(settled('\n\n' + ENVELOPE + '\n')).toBe('```json\n' + ENVELOPE_PRETTY + '\n```\n');
  });

  it('keeps empty containers and scalars-only roots inline', () => {
    expect(settled('{}')).toBe('```json\n{}\n```');
    expect(settled('[]')).toBe('```json\n[]\n```');
    expect(settled('[[],{}]')).toBe('```json\n[\n  [],\n  {}\n]\n```');
  });

  it('leaves an open fence while streaming and closes it once settled', () => {
    const f = new RawJsonFenceFormatter();
    expect(f.render('{"a":', true)).toBe('```json\n{\n  "a": ');
    expect(f.render('{"a":1', true)).toBe('```json\n{\n  "a": 1');
    expect(f.render('{"a":1', false)).toBe('```json\n{\n  "a": 1\n```');
  });

  it('closes the fence when the root closes and passes trailing text through as markdown', () => {
    const f = new RawJsonFenceFormatter();
    expect(f.render('{"a":1}', true)).toBe('```json\n{\n  "a": 1\n}\n```');
    expect(f.render('{"a":1}\n\nAll **done**.', true)).toBe(
      '```json\n{\n  "a": 1\n}\n```\n\nAll **done**.',
    );
    // Settling adds nothing: the fence is already closed.
    expect(f.render('{"a":1}\n\nAll **done**.', false)).toBe(
      '```json\n{\n  "a": 1\n}\n```\n\nAll **done**.',
    );

    const inlineTail = new RawJsonFenceFormatter();
    expect(inlineTail.render('{"a":1} All **done**.', true)).toBe(
      '```json\n{\n  "a": 1\n}\n```\n All **done**.',
    );
  });

  it('is prefix-stable over every prefix of the document, fresh and incremental', () => {
    const full = settled(ENVELOPE);
    const incremental = new RawJsonFenceFormatter();
    for (let n = 1; n <= ENVELOPE.length; n += 1) {
      const prefix = ENVELOPE.slice(0, n);
      const fresh = new RawJsonFenceFormatter().render(prefix, true);
      expect(full.startsWith(fresh), `fresh prefix ${n}`).toBe(true);
      expect(incremental.render(prefix, true), `incremental prefix ${n}`).toBe(fresh);
    }
    expect(incremental.render(ENVELOPE, false)).toBe(full);
  });

  it('consumes proven append deltas without scanning the growing source', () => {
    const expected = settled(ENVELOPE);
    const incremental = new RawJsonFenceFormatter();
    const startsWith = vi.spyOn(String.prototype, 'startsWith')
      .mockImplementation(() => {
        throw new Error('append proof fell back to a prefix scan');
      });
    let source = '';
    try {
      for (const delta of ENVELOPE) {
        const append = createProvenAppend(source, delta);
        source = append.next;
        incremental.render(source, true, append);
      }
      expect(incremental.sourceIsRawJson).toBe(true);
      expect(incremental.render(source, false)).toBe(expected);
    } finally {
      startsWith.mockRestore();
    }
  });

  it('rejects stale and fabricated append proofs and rebuilds from the source', () => {
    const formatter = new RawJsonFenceFormatter();
    const initial = createProvenAppend('', '{"a"');
    expect(formatter.render(initial.next, true, initial)).toBe(
      '```json\n{\n  "a"',
    );

    const startsWith = vi.spyOn(String.prototype, 'startsWith');
    try {
      const stale = createProvenAppend('{"other"', ':1}');
      const next = '{"a":1';
      expect(formatter.render(next, true, stale)).toBe(
        '```json\n{\n  "a": 1',
      );
      expect(startsWith).toHaveBeenCalled();

      startsWith.mockClear();
      const fabricated = {
        previous: next,
        delta: '}',
        next: `${next}}`,
      } as unknown as ReturnType<typeof createProvenAppend>;
      expect(formatter.render(fabricated.next, true, fabricated)).toBe(
        '```json\n{\n  "a": 1\n}\n```',
      );
      expect(startsWith).toHaveBeenCalled();
    } finally {
      startsWith.mockRestore();
    }
  });

  it('does not carry append output lineage across an authoritative rewrite', () => {
    const formatter = new RawJsonFenceFormatter();
    const first = createProvenAppend('', '{"a":1}');
    formatter.render(first.next, true, first);
    expect(formatter.outputAppend?.next).toBe('```json\n{\n  "a": 1\n}\n```');

    const rewritten = '{"replacement":[true,false]}';
    expect(formatter.render(rewritten, true)).toBe(
      '```json\n{\n  "replacement": [\n    true,\n    false\n  ]\n}\n```',
    );
    expect(formatter.outputAppend).toBeUndefined();

    const append = createProvenAppend(rewritten, '\n\nDone.');
    expect(formatter.render(append.next, true, append)).toBe(
      '```json\n{\n  "replacement": [\n    true,\n    false\n  ]\n}\n```\n\nDone.',
    );
    expect(formatter.outputAppend?.delta).toBe('\n\nDone.');
  });

  it('classifies prose once and can reject a tentative JSON opener by delta', () => {
    const prose = new RawJsonFenceFormatter();
    const firstProse = createProvenAppend('', 'J');
    expect(prose.render(firstProse.next, true, firstProse)).toBe('J');
    expect(prose.outputAppend?.delta).toBe('J');
    const restOfProse = createProvenAppend('J', 'ust prose');
    expect(prose.render(restOfProse.next, true, restOfProse)).toBe('Just prose');
    expect(prose.outputAppend?.delta).toBe('ust prose');
    expect(prose.sourceIsRawJson).toBe(false);

    const tentative = new RawJsonFenceFormatter();
    const tentativeOpen = createProvenAppend('', '{');
    expect(tentative.render(tentativeOpen.next, true, tentativeOpen)).toBe('```json\n{');
    expect(tentative.sourceIsRawJson).toBe(true);
    const tentativeReject = createProvenAppend('{', 'a');
    expect(tentative.render(tentativeReject.next, true, tentativeReject)).toBe('{a');
    expect(tentative.sourceIsRawJson).toBe(false);
    expect(tentative.outputAppend).toBeUndefined();
  });

  it('emits the exact formatted suffix for every proven JSON append', () => {
    const formatter = new RawJsonFenceFormatter();
    const source = '{"a":[1,2],"b":{}}';
    let input = '';
    let output = '';
    for (const delta of source) {
      const sourceAppend = createProvenAppend(input, delta);
      input = sourceAppend.next;
      const rendered = formatter.render(input, true, sourceAppend);
      const append = formatter.outputAppend;
      expect(append?.previous).toBe(output);
      expect(append?.next).toBe(rendered);
      output += append?.delta ?? '';
      expect(output).toBe(rendered);
    }
  });

  it('withholds append proof when an open JSON fence changes streaming mode', () => {
    const formatter = new RawJsonFenceFormatter();
    let sourceAppend = createProvenAppend('', '{');
    let source = sourceAppend.next;
    let rendered = formatter.render(source, false, sourceAppend);
    expect(formatter.outputAppend?.next).toBe(rendered);
    expect(rendered.endsWith('\n```')).toBe(true);

    sourceAppend = createProvenAppend(source, '"a"');
    source = sourceAppend.next;
    rendered = formatter.render(source, false, sourceAppend);
    expect(formatter.outputAppend).toBeUndefined();
    expect(rendered.endsWith('\n```')).toBe(true);

    rendered = formatter.render(source, true);
    expect(formatter.outputAppend).toBeUndefined();
    expect(rendered.endsWith('\n```')).toBe(false);

    const previous = rendered;
    sourceAppend = createProvenAppend(source, ':1');
    source = sourceAppend.next;
    rendered = formatter.render(source, true, sourceAppend);
    expect(formatter.outputAppend?.previous).toBe(previous);
    expect(formatter.outputAppend?.next).toBe(rendered);

    rendered = formatter.render(source, false);
    expect(formatter.outputAppend).toBeUndefined();
    expect(rendered.endsWith('\n```')).toBe(true);
  });

  it('round-trips randomized nested JSON across irregular proven appends', () => {
    for (const [rootIndex, root] of randomJsonRoots(0x5eedc0de, 96).entries()) {
      const source = JSON.stringify(root);
      const final = settled(source);
      const formatter = new RawJsonFenceFormatter();
      let input = '';
      let offset = 0;
      let chunkState = (rootIndex + 1) * 0x9e3779b1;
      while (offset < source.length) {
        chunkState = (Math.imul(chunkState, 1_103_515_245) + 12_345) >>> 0;
        const end = Math.min(source.length, offset + 1 + (chunkState % 17));
        const delta = source.slice(offset, end);
        const append = createProvenAppend(input, delta);
        input = append.next;
        const streaming = formatter.render(input, true, append);
        expect(formatter.outputAppend?.next).toBe(streaming);
        expect(final.startsWith(streaming), `root ${rootIndex}, byte ${end}`).toBe(true);
        offset = end;
      }
      const rendered = formatter.render(input, false);
      expect(rendered).toBe(final);
      const body = rendered.slice('```json\n'.length, -'\n```'.length);
      expect(JSON.parse(body)).toEqual(root);
    }
  });

  it('is prefix-stable across trailing prose too', () => {
    const source = ENVELOPE + '\n\nNote: `x_y` _and_ more.';
    const full = new RawJsonFenceFormatter().render(source, false);
    const incremental = new RawJsonFenceFormatter();
    for (let n = ENVELOPE.length - 5; n <= source.length; n += 1) {
      const out = incremental.render(source.slice(0, n), true);
      expect(full.startsWith(out), `prefix ${n}`).toBe(true);
    }
  });

  it('restarts from scratch when the source is not an extension of the previous one', () => {
    const f = new RawJsonFenceFormatter();
    f.render('{"a":1', true);
    expect(f.render('{"b":2}', false)).toBe(settled('{"b":2}'));
    expect(f.render('{"b"', true)).toBe('```json\n{\n  "b"');
  });

  it('recovers after a row flips between JSON and prose', () => {
    const f = new RawJsonFenceFormatter();
    f.render('{"a":1}', false);
    expect(f.render('plain', false)).toBe('plain');
    expect(f.render('{"a":1}', false)).toBe(settled('{"a":1}'));
  });

  it('never throws on malformed input and always closes the fence', () => {
    for (const s of ['{"a":}', '{"a":1}}}', '[{"a":1', '{"a":"\\', '{"a",,,}', '{"a":1]']) {
      const out = settled(s);
      expect(out.startsWith('```json\n'), s).toBe(true);
      expect(out.includes('\n```'), s).toBe(true);
    }
    // Extra closers after the root are trailing text, not JSON: they
    // pass through after the closed fence like any other tail.
    expect(settled('{"a":1}}}')).toBe('```json\n{\n  "a": 1\n}\n```\n}}');
  });
});
