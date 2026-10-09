import { describe, expect, it } from 'vitest';
import {
  AUTHOR_TONE_COUNT,
  authorDisplayName,
  authorInitials,
  authorTone,
  isBotLogin,
} from './reviewIdentity';

describe('authorDisplayName', () => {
  it('shows the display name when the forge knows one', () => {
    expect(authorDisplayName({ authorLogin: 'adoe', authorName: 'Alice Doe' })).toBe('Alice Doe');
  });

  it('falls back to the login for a missing or blank name', () => {
    expect(authorDisplayName({ authorLogin: 'octocat' })).toBe('octocat');
    expect(authorDisplayName({ authorLogin: 'octocat', authorName: '' })).toBe('octocat');
    // A whitespace-only name would render as an empty bold label.
    expect(authorDisplayName({ authorLogin: 'octocat', authorName: '   ' })).toBe('octocat');
  });
});

describe('authorInitials', () => {
  it('takes the first letters of the first two words of the display name', () => {
    expect(authorInitials({ authorLogin: 'adoe', authorName: 'Alice Doe' })).toBe('AD');
    expect(authorInitials({ authorLogin: 'x', authorName: 'alice beth doe' })).toBe('AB');
  });

  it('takes the first two characters of a single-word name, uppercased', () => {
    expect(authorInitials({ authorLogin: 'octocat' })).toBe('OC');
    // `[bot]` is not a word separator, so the bot login is one word.
    expect(authorInitials({ authorLogin: 'coderabbitai[bot]' })).toBe('CO');
  });

  it('splits login words on hyphens, underscores and dots', () => {
    expect(authorInitials({ authorLogin: 'coderabbit-api-dev' })).toBe('CA');
    expect(authorInitials({ authorLogin: 'jane_smith' })).toBe('JS');
    expect(authorInitials({ authorLogin: 'john.q.public' })).toBe('JQ');
    // Leading separators do not produce an empty first word.
    expect(authorInitials({ authorLogin: '-max-power' })).toBe('MP');
  });

  it('prefers the display name over the login', () => {
    expect(authorInitials({ authorLogin: 'zz-top', authorName: 'Alice Doe' })).toBe('AD');
  });

  it('keeps a non-BMP first character whole', () => {
    // Array.from splits by code point, so a surrogate pair is not halved.
    expect(authorInitials({ authorLogin: 'x', authorName: '😀 smile' })).toBe('😀S');
  });
});

describe('authorTone', () => {
  it('is a stable index within the tone table', () => {
    const logins = ['alice', 'bob', 'octocat', 'coderabbitai[bot]', '', 'a-very-long-login-name-1234567890'];
    for (const login of logins) {
      const tone = authorTone(login);
      expect(Number.isInteger(tone)).toBe(true);
      expect(tone).toBeGreaterThanOrEqual(0);
      expect(tone).toBeLessThan(AUTHOR_TONE_COUNT);
      // One person keeps one color across renders and PRs.
      expect(authorTone(login)).toBe(tone);
    }
  });

  it('spreads distinct logins over more than one tone', () => {
    const tones = new Set(Array.from({ length: 40 }, (_, index) => authorTone(`user${index}`)));
    expect(tones.size).toBeGreaterThan(1);
  });
});

describe('isBotLogin', () => {
  it('recognizes GitHub app logins, GitLab bot tokens and -bot names', () => {
    expect(isBotLogin('coderabbitai[bot]')).toBe(true);
    expect(isBotLogin('dependabot[bot]')).toBe(true);
    expect(isBotLogin('my-bot')).toBe(true);
    expect(isBotLogin('bot-runner')).toBe(true);
    expect(isBotLogin('group_42_bot_abc123')).toBe(true);
    expect(isBotLogin('Release-BOT')).toBe(true);
  });

  it('does not flag logins that merely contain the letters', () => {
    expect(isBotLogin('robotics')).toBe(false);
    expect(isBotLogin('abbott')).toBe(false);
    expect(isBotLogin('alice')).toBe(false);
  });
});
