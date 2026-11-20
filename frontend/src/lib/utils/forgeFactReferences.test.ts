import { describe, expect, it } from 'vitest';
import { lex, parseBlocks, type Extension, type ForgeFactReader, type Token } from '../markdown';
import { createDocumentContextCache, createParseBlocksCache, documentContextOf } from '../markdown';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from '../components/chat/markdown/chatMarkdownOptions';
import { forgeFactKeys } from './forgeReferenceExtension';
import { FACT_KEYS_PER_BODY, REVISION_FACT_KEY, labelIndexCounters, type ForgeFact, type ForgeFactState } from './forgeFacts';
import type { ForgeAttachmentSource } from './forgeAttachments';
import { CORPUS, findCandidateCombinations, generatedMarkdown } from '../../test/fixtures/markdownFindCorpus';

// References read the forge's answer (a fact) from their block's scope,
// and `forgeFactKeys` names every key a body's lex can read, from its raw
// text: the store asks for those before any body mounts.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};
const GH = 'https://github.com/acme/widget';
const GL = 'https://gitlab.com/group/widget';
const SHA = '0123456789abcdef0123456789abcdef01234567';

const extensionsFor = (source: ForgeAttachmentSource): Extension[] => chatMarkdownExtensions({ forge: chatMarkdownForgeOptions(source, true) });

/** A reader over `facts`, answering `rest` for every other key, recording each key read. */
function reader(facts: Record<string, ForgeFactState>, rest: ForgeFactState = undefined): ForgeFactReader & { read: Set<string> } {
  const read = new Set<string>();
  return {
    read,
    get: (key) => {
      read.add(key);
      return key in facts ? facts[key] : rest;
    },
    signature: (key) => JSON.stringify(key in facts ? facts[key] : rest) ?? '',
  };
}

/** `markdown` lexed as ChatMarkdown lexes a settled body, under `facts`. */
function lexUnder(markdown: string, source: ForgeAttachmentSource, facts: ForgeFactReader | null): Token[] {
  const extensions = extensionsFor(source);
  const cache = createParseBlocksCache();
  const blocks = parseBlocks(markdown, extensions, cache);
  const context = documentContextOf(cache, extensions, createDocumentContextCache(), source.pr.forge as 'github' | 'gitlab', facts);
  return blocks.flatMap((block, index) => lex(block, extensions, context.scope(index)) as unknown as Token[]);
}

type Shown = { type: string; text: string; href?: string; title?: string | null; style?: unknown };

function shown(tokens: readonly Token[], out: Shown[] = []): Shown[] {
  for (const token of tokens) {
    const any = token as Token & { href?: string; title?: string | null; tokens?: Token[]; color?: string | null; scope?: string | null; name?: string };
    if (token.type === 'link') out.push({ type: 'link', text: (any.tokens ?? []).map((child) => ('text' in child ? child.text : '')).join(''), href: any.href, title: any.title });
    else if (token.type === 'labelChip') out.push({ type: 'labelChip', text: `${any.scope ?? ''}|${any.name}`, href: any.href, style: any.color });
    else if (any.tokens) shown(any.tokens, out);
  }
  return out;
}

const found = (key: string, extra: Partial<ForgeFact> = {}): ForgeFact => ({ key, status: 'found', ...extra });
const absent = (key: string): ForgeFact => ({ key, status: 'absent' });
const unavailable = (key: string): ForgeFact => ({ key, status: 'unavailable' });

describe('references by their facts', () => {
  it('links a found GitHub #12 where the forge does, a pull request at /pull, with its title', () => {
    const facts = reader({ 'ref:acme/widget#12': found('ref:acme/widget#12', { url: `${GH}/pull/12`, kind: 'pull', title: 'Add widget' }) });
    expect(shown(lexUnder('See #12 now', GITHUB, facts))).toEqual([{ type: 'link', text: '#12', href: `${GH}/pull/12`, title: 'Add widget' }]);
  });

  it('keeps the shape for a found URL off the PR\'s forge', () => {
    const facts = reader({ 'ref:acme/widget#12': found('ref:acme/widget#12', { url: 'https://evil.example/12' }) });
    expect(shown(lexUnder('#12', GITHUB, facts))[0].href).toBe(`${GH}/issues/12`);
  });

  it.each([
    ['#12', 'ref:acme/widget#12', GITHUB],
    ['GH-12', 'ref:acme/widget#12', GITHUB],
    ['other/repo#7', 'ref:other/repo#7', GITHUB],
    ['@octo', 'user:octo', GITHUB],
    ['!3+', 'mr:group/widget!3', GITLAB],
    ['group/other#5', 'ref:group/other#5', GITLAB],
    ['@group/sub', 'user:group/sub', GITLAB],
  ] as const)('shows an absent %s as text as written', (markdown, key, source) => {
    const tokens = lexUnder(`a ${markdown} b`, source, reader({ [key]: absent(key) }));
    expect(shown(tokens)).toEqual([]);
    expect((tokens[0] as Tokens).tokens.map((token) => token.raw).join('')).toBe(`a ${markdown} b`);
  });

  it.each([['pending'], ['unavailable'], ['undefined']] as const)('links a %s reference by its shape', (state) => {
    const value: ForgeFactState = state === 'pending' ? 'pending' : state === 'unavailable' ? unavailable('ref:acme/widget#12') : undefined;
    expect(shown(lexUnder('#12 @octo', GITHUB, reader({}, value)))).toEqual([
      { type: 'link', text: '#12', href: `${GH}/issues/12`, title: null },
      { type: 'link', text: '@octo', href: 'https://github.com/octo', title: null },
    ]);
  });

  it('hovers a found commit\'s subject and links it by its shape', () => {
    const facts = reader({ [`commit:${SHA}`]: found(`commit:${SHA}`, { oid: SHA, title: 'Fix the thing' }) });
    expect(shown(lexUnder(SHA, GITLAB, facts))).toEqual([{ type: 'link', text: SHA.slice(0, 8), href: `${GL}/-/commit/${SHA}`, title: 'Fix the thing' }]);
  });
});

describe('GitLab labels, milestones, snippets and epics', () => {
  const LABELS = 'labels:group/widget';
  const label = (title: string, extra: Partial<ForgeFact> = {}) => found(`label:group/widget~${title}`, { color: '#d9534f', textColor: '#ffffff', url: `${GL}/-/issues?label_name=${encodeURIComponent(title)}`, ...extra });
  const list = (titles: string[], complete = true): ForgeFact =>
    found(LABELS, { complete, labels: titles.map((title) => ({ title, color: '#d9534f', textColor: '#ffffff' })) });

  it('shows a label in the list as a chip in its colors, linking to its issues', () => {
    const facts = reader({ [LABELS]: list(['bug']) });
    expect(shown(lexUnder('a ~bug b', GITLAB, facts))).toEqual([{ type: 'labelChip', text: '|bug', href: `${GL}/-/issues?label_name=bug`, style: '#d9534f' }]);
    expect([...facts.read]).toEqual([LABELS]);
  });

  it('leaves a label a complete list lacks as text, matching titles exactly', () => {
    const facts = reader({ [LABELS]: list(['bug', 'Docs', 'two words']) });
    expect(shown(lexUnder('~bug ~bugs ~docs ~Docs ~"two words" ~"two"', GITLAB, facts)).map((each) => each.text)).toEqual(['|bug', '|Docs', '|two words']);
    expect([...facts.read]).toEqual([LABELS]);
  });

  it('decides a label a list cut short lacks by its own fact, asked as a read', () => {
    const facts = reader({
      [LABELS]: list(['bug'], false),
      'label:group/widget~late': label('late', { color: '#00ff00' }),
      'label:group/widget~gone': absent('label:group/widget~gone'),
      'label:group/widget~asked': 'pending',
    });
    expect(shown(lexUnder('~bug ~late ~gone ~asked', GITLAB, facts))).toEqual([
      { type: 'labelChip', text: '|bug', href: `${GL}/-/issues?label_name=bug`, style: '#d9534f' },
      { type: 'labelChip', text: '|late', href: `${GL}/-/issues?label_name=late`, style: '#00ff00' },
      { type: 'labelChip', text: '|asked', href: `${GL}/-/issues?label_name=asked`, style: null },
    ]);
    expect([...facts.read].sort()).toEqual([LABELS, 'label:group/widget~asked', 'label:group/widget~gone', 'label:group/widget~late'].sort());
  });

  it('builds a list\'s title index once, however many lexes read it', () => {
    const facts = reader({ [LABELS]: list(Array.from({ length: 1000 }, (_, i) => `label${i}`)) });
    const builds = labelIndexCounters.builds;
    for (let i = 0; i < 20; i++) lexUnder(`~label${i} and ~label${999 - i}\n\nmore ~label${i * 3}`, GITLAB, facts);
    expect(labelIndexCounters.builds).toBe(builds + 1);
  });

  it('splits a scoped label at its last `::`, quoted or not', () => {
    const facts = reader({}, 'pending');
    expect(shown(lexUnder('~type::bug and ~"a b::c d" and ~"multi word"', GITLAB, facts))).toEqual([
      { type: 'labelChip', text: 'type|bug', href: `${GL}/-/issues?label_name=type%3A%3Abug`, style: null },
      { type: 'labelChip', text: 'a b|c d', href: `${GL}/-/issues?label_name=a%20b%3A%3Ac%20d`, style: null },
      { type: 'labelChip', text: '|multi word', href: `${GL}/-/issues?label_name=multi%20word`, style: null },
    ]);
  });

  it.each([['absent'], ['unavailable'], ['undefined']] as const)('leaves a label as text when its list is %s', (state) => {
    const value: ForgeFactState = state === 'absent' ? absent(LABELS) : state === 'unavailable' ? unavailable(LABELS) : undefined;
    const facts = reader({}, value);
    expect(shown(lexUnder('a ~bug b', GITLAB, facts))).toEqual([]);
    // Nothing is asked per title.
    expect([...facts.read]).toEqual([LABELS]);
  });

  it('leaves a tilde that strikes to the strikethrough', () => {
    const tokens = lexUnder('a ~bug~ b', GITLAB, reader({}, 'pending'));
    expect(shown(tokens)).toEqual([]);
    expect(JSON.stringify(tokens)).toContain('"type":"del"');
  });

  it('links a found or pending milestone and epic and a found snippet, and leaves the rest as text', () => {
    const facts = reader({
      'milestone:group/widget%v1.0': found('milestone:group/widget%v1.0', { url: `${GL}/-/milestones/2`, title: 'v1.0' }),
      'milestone:group/widget%v2': 'pending',
      'snippet:group/widget$5': found('snippet:group/widget$5', { url: `${GL}/-/snippets/5`, title: 'Snip' }),
      // Asked: `$7` is a price far more often than a snippet.
      'snippet:group/widget$7': 'pending',
      'epic:group&9': 'pending',
    });
    expect(shown(lexUnder('%v1.0 %v2 $5 $7 &9 %"multi word" $6', GITLAB, facts))).toEqual([
      { type: 'link', text: '%v1.0', href: `${GL}/-/milestones/2`, title: 'v1.0' },
      { type: 'link', text: '%v2', href: `${GL}/-/milestones?search_title=v2`, title: null },
      { type: 'link', text: '$5', href: `${GL}/-/snippets/5`, title: 'Snip' },
      { type: 'link', text: '&9', href: 'https://gitlab.com/groups/group/-/epics/9', title: null },
    ]);
  });

  it('leaves a bare label that starts with a digit as text while it is asked, and chips it once found', () => {
    const source = 'takes ~5 minutes, ~10ms or ~2x, but ~"5 things" and ~v2';
    expect(shown(lexUnder(source, GITLAB, reader({}, 'pending'))).map((each) => each.text)).toEqual(['|5 things', '|v2']);
    const facts = reader({ [LABELS]: list(['5', '5 things', 'v2']) }, 'pending');
    expect(shown(lexUnder(source, GITLAB, facts)).map((each) => each.text)).toEqual(['|5', '|5 things', '|v2']);
    // A list cut short asks the rest per title, as prose while asked.
    const short = reader({ [LABELS]: list(['v2'], false) }, 'pending');
    expect(shown(lexUnder(source, GITLAB, short)).map((each) => each.text)).toEqual(['|5 things', '|v2']);
  });

  it('reads none of them on GitHub', () => {
    const facts = reader({}, 'pending');
    expect(shown(lexUnder('~bug %v1 $5 &9', GITHUB, facts))).toEqual([]);
    expect(forgeFactKeys('~bug %v1 $5 &9', GITHUB)).toEqual([]);
  });
});

type Tokens = { tokens: Token[] };

describe('forgeFactKeys', () => {
  it('names each reference\'s canonical key', () => {
    expect(forgeFactKeys(`Fixes #12, GH-3 and other/repo#7 by @octo in ${SHA}; @acme/team and a@b.example`, GITHUB)).toEqual([
      'ref:acme/widget#12', 'ref:acme/widget#3', 'ref:other/repo#7', 'user:octo', `commit:${SHA}`,
    ]);
    expect(forgeFactKeys('!3 widget#4 group/sub/p!5 ~bug ~"a b" %v1.0. $7 &8 @ana.', GITLAB)).toEqual([
      'mr:group/widget!3', 'ref:group/widget#4', 'mr:group/sub/p!5', 'labels:group/widget',
      'milestone:group/widget%v1.0', 'snippet:group/widget$7', 'epic:group&8', 'user:ana',
    ]);
  });

  it('names the label list once for a body of any number of labels, and none for one without', () => {
    const many = Array.from({ length: 500 }, (_, i) => `~l${i}`).join(' ');
    expect(forgeFactKeys(`${many} ~"a b" ~k::v`, GITLAB)).toEqual(['labels:group/widget']);
    expect(forgeFactKeys('a ~ b, ~. and x~y', GITLAB)).toEqual([]);
  });

  it('asks for at most FACT_KEYS_PER_BODY keys per body', () => {
    const body = Array.from({ length: 1000 }, (_, i) => `#${i + 1}`).join(' ');
    expect(forgeFactKeys(body, GITHUB)).toHaveLength(FACT_KEYS_PER_BODY);
  });

  it('names no key for a PR with no forge address', () => {
    expect(forgeFactKeys('#1', { ...GITHUB, pr: { ...GITHUB.pr, host: '' }, webBase: '' })).toEqual([]);
  });

  // Every key a lex reads is one the scan named, on both forges, whether
  // the facts are unknown, pending or found (a chip or link takes text a
  // later rule would otherwise read).
  const BODIES = [
    ...CORPUS,
    ...findCandidateCombinations(),
    ...Array.from({ length: 40 }, (_, seed) => generatedMarkdown(seed)),
    'See #12, !12+, acme/widget#3, @octo., a@b.example, x...y, group/widget>, [a](docs/a.md) and [[Home]].',
    `_owner/repo#1_ __widget#2_ {-group/x!3-} *@ana* _@bob_ ~~#4~~ <b>#5</b> \`#6\` [#7](#x) ${SHA}...${SHA.slice(0, 9)} widget@abc1234`,
    '~bug ~"two words" ~type::bug %v1 %"m s" $5 &6 group&7 ~bug~ ~~x~~ (~bug) _~bug_ {+~bug+}',
    '| #1 | @a |\n| --- | --- |\n| ~b | %c |\n\n> #2 @d\n\n- [ ] #3\n\n# Heading #4\n\n<details><summary>#5</summary>\n\n@e ~f\n\n</details>',
    'GH-1 _GH-2_ xGH-3 @octo/team @Octo-Cat @-x 0123abcd ab12cd3 deadbeef',
  ];
  // References packed against every delimiter, container and code edge a
  // lex can start or end inside, seeded.
  const PIECES = ['#1', '!2', '@a_b', '@c-d', '~bug', '~"x y"', '~k::v', '%m1', '$5', '&6', 'widget#7', 'group/x!8', 'acme/widget#9', SHA.slice(0, 9), 'GH-3',
    '_', '__', '*', '~', '~~', '{-', '-}', '{+', '+}', '`', '[', '](x)', '<b>', '</b>', '\\', '&amp;', ' ', ' ', '\n', '|', '.', '-', ':', '@', '#'];
  let seed = 11;
  const next = () => ((seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648);
  const FUZZ = Array.from({ length: 800 }, () => Array.from({ length: 3 + Math.floor(next() * 8) }, () => PIECES[Math.floor(next() * PIECES.length)]).join(''));
  BODIES.push(...FUZZ);

  // A found label list here is complete: a list cut short reads a title
  // it lacks per title, a key no scan names by design (the store asks for
  // a key a block reads, "The scan is a prefetch").
  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('names every key a lex reads on %s', (_name, source) => {
    const labels = ['bug', 'two words', 'type::bug', 'b', 'f', 'k::v', 'x y'].map((title) => ({ title, color: '#d9534f' }));
    for (const rest of [undefined, 'pending' as const, found('any', { url: `${source.webBase}`, complete: true, labels })]) {
      for (const body of BODIES) {
        const facts = reader({}, rest);
        lexUnder(body, source, facts);
        const keys = new Set(forgeFactKeys(body, source));
        // The frame's revision is the store's own answer, never asked for.
        expect([...facts.read].filter((key) => key !== REVISION_FACT_KEY && !keys.has(key)), body).toEqual([]);
      }
    }
  });

  it('scans in time linear in the body', () => {
    const unit = `Fixes #12 by @octo in ${SHA}, ~bug and %v1, see owner/repo#3 and _a_b/c#4 here. `;
    const time = (repeat: number) => {
      const body = unit.repeat(repeat);
      const samples: number[] = [];
      for (let run = 0; run < 7; run++) {
        const start = performance.now();
        forgeFactKeys(body, GITLAB);
        samples.push(performance.now() - start);
      }
      return samples.sort((a, b) => a - b)[3];
    };
    time(500);
    // Sixteen times the text: linear is about 16x, quadratic about 256x.
    expect(time(8000) / Math.max(time(500), 0.05)).toBeLessThan(48);
  });

  it('scans a long run of underscores before a sigil in linear time', () => {
    // The median of five, so one descheduled run under a loaded worker
    // does not read as superlinear.
    const time = (length: number) => {
      const body = `${'_'.repeat(length)}x#1 `.repeat(4);
      const samples: number[] = [];
      for (let run = 0; run < 5; run++) {
        const start = performance.now();
        forgeFactKeys(body, GITHUB);
        samples.push(performance.now() - start);
      }
      return samples.sort((a, b) => a - b)[2];
    };
    time(20_000);
    expect(time(320_000) / Math.max(time(20_000), 0.05)).toBeLessThan(48);
  });
});
