import { describe, expect, it } from 'vitest';
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
    expect(settled('{"a":1}}}')).toBe('```json\n{\n  "a": 1\n}\n```}}');
  });
});
