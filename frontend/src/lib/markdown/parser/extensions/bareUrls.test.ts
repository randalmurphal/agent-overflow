import { describe, expect, it, vi } from 'vitest';
import { backpedalUrl } from '../engine/destination';
import { inline } from '../engine/rules';
import { markedGithubBareUrls, markedGitlabBareUrls } from './bareUrls';

const githubEnd = (run: string): string => markedGithubBareUrls.bareUrls!.end(run);
const gitlabEnd = (run: string): string => markedGitlabBareUrls.bareUrls!.end(run);

/** GFM's rule as written plainly: `_backpedal` until it changes nothing. */
const backpedalReference = (run: string): string => {
	let previous;
	do {
		previous = run;
		run = inline.gfm._backpedal.exec(run)?.[0] ?? '';
	} while (previous !== run);
	return run;
};

/** GitLab's rule as written plainly: backpedal, then drop one unbalanced closer, until neither changes the URL. */
const gitlabReference = (run: string): string => {
	const count = (text: string, char: string) => text.split(char).length - 1;
	for (;;) {
		const url = backpedalReference(run);
		const last = url.at(-1) ?? '';
		const opener = last === ']' ? '[' : last === '}' ? '{' : undefined;
		if (opener === undefined || count(url, last) <= count(url, opener)) return url;
		run = url.slice(0, -1);
	}
};

/** Seeded bare URL runs: the same runs on every run of the suite. */
const seededRuns = (seed: number, count: number): string[] => {
	const next = () => (seed = (seed * 1103515245 + 12345) % 2 ** 31);
	const pieces = ['a', 'b', '1', '(', ')', '[', ']', '{', '}', '.', ',', ';', ':', '&', '&amp;', '!', '?', '_', '~', "'", '"', '*', '/'];
	const runs: string[] = [];
	for (let round = 0; round < count; round++) {
		let run = next() % 4 === 0 ? 'www.example.org/' : 'https://example.org/';
		const length = next() % 14;
		for (let i = 0; i < length; i++) run += pieces[next() % pieces.length];
		runs.push(run);
	}
	return runs;
};

/** The characters the regexes `run` executes read: the length of each input. */
const scanned = (run: () => void): number => {
	const exec = vi.spyOn(RegExp.prototype, 'exec');
	try {
		run();
		return exec.mock.calls.reduce((total, [input]) => total + String(input).length, 0);
	} finally {
		exec.mockRestore();
	}
};

describe('bare URL ends', () => {
	it('end as the plain rules end, for runs of brackets, braces, entities and punctuation', () => {
		for (const run of seededRuns(12345, 5000)) expect(gitlabEnd(run), JSON.stringify(run)).toBe(gitlabReference(run));
		for (const run of seededRuns(777, 5000)) {
			// Agent chat's rule is the engine's; GitHub's ends a URL by it too.
			expect(backpedalUrl(run), JSON.stringify(run)).toBe(backpedalReference(run));
			expect(githubEnd(run), JSON.stringify(run)).toBe(backpedalReference(run));
		}
	});

	it.each([
		['a GitLab', 'many unbalanced closers', gitlabEnd, 'https://example.org/path' + ']'.repeat(4000), 'https://example.org/path'],
		['a GitLab', 'alternating punctuation and closers', gitlabEnd, 'https://example.org/p' + '.]'.repeat(4000), 'https://example.org/p'],
		['a GitHub', 'trailing punctuation', githubEnd, 'https://example.org/p' + '.'.repeat(4000), 'https://example.org/p'],
		['an agent chat', 'trailing punctuation', backpedalUrl, 'https://example.org/p' + '.,'.repeat(4000), 'https://example.org/p']
	])('ends %s URL with %s in linear work', (_surface, _shape, end, run, url) => {
		let result = '';
		// Each regex reads the run once at most, not once per character trimmed.
		expect(scanned(() => (result = end(run)))).toBeLessThanOrEqual(3 * run.length);
		expect(result).toBe(url);
	});
});
