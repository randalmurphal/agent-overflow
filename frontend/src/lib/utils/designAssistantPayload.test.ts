import { describe, expect, it } from 'vitest';
import {
  controlsKey,
  parseDesignAssistantPayloads,
} from './designAssistantPayload';

describe('parseDesignAssistantPayloads', () => {
  it('returns [] for empty / fence-free text', () => {
    expect(parseDesignAssistantPayloads('')).toEqual([]);
    expect(parseDesignAssistantPayloads('hello world')).toEqual([]);
    expect(parseDesignAssistantPayloads('```js\n{}\n```')).toEqual([]);
  });

  it('extracts a clarification_request block', () => {
    const text = [
      'Before I start, pick from each:',
      '',
      '```aoflow-design',
      JSON.stringify({
        kind: 'clarification_request',
        requestId: 'req-1',
        intro: 'Pick a family',
        questions: [
          {
            id: 'family',
            prompt: 'Aesthetic family',
            choices: [
              { id: 'editorial', label: 'Editorial minimal' },
              { id: 'terminal', label: 'Terminal-core' },
            ],
          },
        ],
      }),
      '```',
    ].join('\n');

    const got = parseDesignAssistantPayloads(text);
    expect(got).toHaveLength(1);
    expect(got[0].kind).toBe('clarification_request');
    if (got[0].kind !== 'clarification_request') throw new Error('kind narrow');
    expect(got[0].payload.requestId).toBe('req-1');
    expect(got[0].payload.intro).toBe('Pick a family');
    expect(got[0].payload.questions).toHaveLength(1);
    expect(got[0].payload.questions[0].choices).toHaveLength(2);
  });

  it('extracts an expose_controls block', () => {
    const text =
      '```aoflow-design\n' +
      JSON.stringify({
        kind: 'expose_controls',
        controls: [
          { id: 'density', label: 'Density', min: 0.6, max: 1.4, step: 0.05, value: 1.0 },
        ],
      }) +
      '\n```';

    const got = parseDesignAssistantPayloads(text);
    expect(got).toHaveLength(1);
    if (got[0].kind !== 'expose_controls') throw new Error('kind narrow');
    expect(got[0].payload.controls).toHaveLength(1);
    expect(got[0].payload.controls[0].id).toBe('density');
    expect(got[0].payload.controls[0].step).toBe(0.05);
  });

  it('extracts multiple blocks in document order', () => {
    const text = [
      '```aoflow-design',
      JSON.stringify({
        kind: 'clarification_request',
        requestId: 'q1',
        questions: [
          {
            id: 'a',
            prompt: 'A?',
            choices: [{ id: 'x', label: 'x' }],
          },
        ],
      }),
      '```',
      'middle text',
      '```aoflow-design',
      JSON.stringify({
        kind: 'expose_controls',
        controls: [{ id: 'k', label: 'K', min: 0, max: 1, value: 0.5 }],
      }),
      '```',
    ].join('\n');

    const got = parseDesignAssistantPayloads(text);
    expect(got).toHaveLength(2);
    expect(got[0].kind).toBe('clarification_request');
    expect(got[1].kind).toBe('expose_controls');
  });

  it('drops blocks with invalid JSON without throwing', () => {
    const text = '```aoflow-design\n{not json}\n```';
    expect(parseDesignAssistantPayloads(text)).toEqual([]);
  });

  it('drops blocks with unknown kind', () => {
    const text =
      '```aoflow-design\n' +
      JSON.stringify({ kind: 'mystery_payload', value: 1 }) +
      '\n```';
    expect(parseDesignAssistantPayloads(text)).toEqual([]);
  });

  it('drops a clarification_request missing required fields', () => {
    const cases = [
      // missing questions
      { kind: 'clarification_request', requestId: 'r' },
      // empty questions
      { kind: 'clarification_request', requestId: 'r', questions: [] },
      // question without choices
      {
        kind: 'clarification_request',
        requestId: 'r',
        questions: [{ id: 'q', prompt: 'p', choices: [] }],
      },
      // choice without id
      {
        kind: 'clarification_request',
        requestId: 'r',
        questions: [
          { id: 'q', prompt: 'p', choices: [{ label: 'has no id' }] },
        ],
      },
    ];
    for (const c of cases) {
      const text = '```aoflow-design\n' + JSON.stringify(c) + '\n```';
      expect(parseDesignAssistantPayloads(text)).toEqual([]);
    }
  });

  it('synthesizes a stable requestId when the agent omits one', () => {
    const make = () =>
      '```aoflow-design\n' +
      JSON.stringify({
        kind: 'clarification_request',
        questions: [
          {
            id: 'q1',
            prompt: 'p',
            choices: [{ id: 'a', label: 'A' }],
          },
        ],
      }) +
      '\n```';

    const first = parseDesignAssistantPayloads(make());
    const second = parseDesignAssistantPayloads(make());
    if (first[0].kind !== 'clarification_request') throw new Error('kind');
    if (second[0].kind !== 'clarification_request') throw new Error('kind');
    expect(first[0].payload.requestId).toMatch(/^synth-/);
    expect(first[0].payload.requestId).toBe(second[0].payload.requestId);
  });

  it('drops an expose_controls block with min >= max', () => {
    const text =
      '```aoflow-design\n' +
      JSON.stringify({
        kind: 'expose_controls',
        controls: [{ id: 'k', label: 'K', min: 1, max: 1, value: 1 }],
      }) +
      '\n```';
    expect(parseDesignAssistantPayloads(text)).toEqual([]);
  });

  it('drops an expose_controls block with non-finite value', () => {
    const text =
      '```aoflow-design\n' +
      JSON.stringify({
        kind: 'expose_controls',
        controls: [{ id: 'k', label: 'K', min: 0, max: 1, value: null }],
      }) +
      '\n```';
    expect(parseDesignAssistantPayloads(text)).toEqual([]);
  });

  it('handles unclosed fence (streaming) gracefully', () => {
    // No closing ``` — parser stops at the open fence and returns
    // whatever it had collected before. With no prior block, returns [].
    const text = '```aoflow-design\n{partial';
    expect(parseDesignAssistantPayloads(text)).toEqual([]);
  });

  it('preserves earlier blocks when a later fence is unclosed', () => {
    const text = [
      '```aoflow-design',
      JSON.stringify({
        kind: 'expose_controls',
        controls: [{ id: 'k', label: 'K', min: 0, max: 1, value: 0.5 }],
      }),
      '```',
      'streaming partial:',
      '```aoflow-design',
      '{ "kind": "clarification_request", "questions"',
    ].join('\n');

    const got = parseDesignAssistantPayloads(text);
    expect(got).toHaveLength(1);
    expect(got[0].kind).toBe('expose_controls');
  });
});

describe('controlsKey', () => {
  it('produces equal keys for identical control sets', () => {
    const a = [{ id: 'k', label: 'K', min: 0, max: 1, step: 0.1, value: 0.5 }];
    const b = [{ id: 'k', label: 'K', min: 0, max: 1, step: 0.1, value: 0.5 }];
    expect(controlsKey(a)).toBe(controlsKey(b));
  });

  it('differs when value changes', () => {
    const a = [{ id: 'k', label: 'K', min: 0, max: 1, value: 0.5 }];
    const b = [{ id: 'k', label: 'K', min: 0, max: 1, value: 0.6 }];
    expect(controlsKey(a)).not.toBe(controlsKey(b));
  });
});
