import { describe, expect, it } from 'vitest';
import { statusDotClass, statusDotLabel } from './threadRowStatus';

describe('statusDotClass', () => {
  it('running → warning dot with pulse', () => {
    expect(statusDotClass('running')).toBe('bg-warning animate-pulse');
  });

  it('pending-approval → accent dot', () => {
    expect(statusDotClass('pending-approval')).toBe('bg-accent');
  });

  it('error → error dot', () => {
    expect(statusDotClass('error')).toBe('bg-error');
  });

  it('idle → empty class (dot hidden)', () => {
    expect(statusDotClass('idle')).toBe('');
  });
});

describe('statusDotLabel', () => {
  it('running → "Running"', () => {
    expect(statusDotLabel('running')).toBe('Running');
  });

  it('pending-approval → "Pending approval"', () => {
    expect(statusDotLabel('pending-approval')).toBe('Pending approval');
  });

  it('error → "Error"', () => {
    expect(statusDotLabel('error')).toBe('Error');
  });

  it('idle → empty string', () => {
    expect(statusDotLabel('idle')).toBe('');
  });
});
