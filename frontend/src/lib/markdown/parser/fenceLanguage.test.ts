import { describe, expect, it } from 'vitest';
import { fenceLanguage } from './fenceLanguage';

describe('fenceLanguage', () => {
  it.each([
    [undefined, ''],
    ['', ''],
    ['ruby', 'ruby'],
    ['ruby title="example.rb" {1,3}', 'ruby'],
    ['mermaid\ttitle', 'mermaid'],
    ['json:table extra', 'json:table'],
  ])('reads %j as %j', (info, language) => {
    expect(fenceLanguage(info)).toBe(language);
  });
});
