/**
 * How each forge autolinks a bare URL, as the engine's `bareUrls` option.
 * Both leave GFM's trailing punctuation and an unbalanced `)` out. GitHub
 * links http, https and `www.` only, so `ftp://…` is text. GitLab links
 * every scheme (an `ftp://` URL by the engine's rule, the others by
 * forgeReferenceExtension.ts through `Tokenizer.urlEnd`) and ends a URL
 * before an unbalanced `]` or `}`. Agent chat keeps the engine's rule.
 */
import { backpedalEnd, backpedalUrl, type BareUrlRule } from '../engine/destination';
import type { Extension } from '../lexer';

const count = (text: string, char: string): number => text.split(char).length - 1;

/**
 * The URL in `run`, ending before each unbalanced `]` or `}` at its end,
 * backpedalled again after each. The closers' balances are counted once and
 * kept as the end moves back, and backpedalling reads from the end, so each
 * character is read a bounded number of times.
 */
const gitlabEnd = (run: string): string => {
	const url = backpedalUrl(run);
	let end = url.length;
	let square = count(url, ']') - count(url, '[');
	let curly = count(url, '}') - count(url, '{');
	const trim = (to: number) => {
		for (; end > to; end--) {
			const char = url[end - 1];
			if (char === ']') square--;
			else if (char === '[') square++;
			else if (char === '}') curly--;
			else if (char === '{') curly++;
		}
	};
	for (;;) {
		const last = url[end - 1];
		if (last === ']' ? square <= 0 : last === '}' ? curly <= 0 : true) return url.slice(0, end);
		trim(end - 1);
		trim(backpedalEnd(url, end));
	}
};

const GITHUB_BARE_URLS: BareUrlRule = { links: (scheme) => scheme === 'http' || scheme === 'https', end: backpedalUrl };
const GITLAB_BARE_URLS: BareUrlRule = { links: () => true, end: gitlabEnd };

export const markedGithubBareUrls: Extension = { name: 'githubBareUrls', level: 'inline', bareUrls: GITHUB_BARE_URLS };
export const markedGitlabBareUrls: Extension = { name: 'gitlabBareUrls', level: 'inline', bareUrls: GITLAB_BARE_URLS };
