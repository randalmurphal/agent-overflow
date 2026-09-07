// Bundle hashes identify bytes; only release versions establish upgrade order.
// Mirrors Android ReleaseVersion. Unknown versions never authorize an update.
const numeric = /^(0|[1-9]\d*)$/;
const semver = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;

function parse(version: string): { core: string[]; pre: string[] } | null {
  if (version.length > 256) return null;
  const match = semver.exec(version);
  if (!match || match[0] !== version) return null;
  const pre = match[4]?.split('.') ?? [];
  if (pre.some((part) => /^\d+$/.test(part) && !numeric.test(part))) return null;
  return { core: match.slice(1, 4), pre };
}

function numberOrder(a: string, b: string): number {
  return Math.sign(a.length - b.length) || (a > b ? 1 : a < b ? -1 : 0);
}

export function compareBundleVersions(a: string, b: string): number | null {
  const left = parse(a), right = parse(b);
  if (!left || !right) return null;
  for (let i = 0; i < 3; i++) {
    const order = numberOrder(left.core[i], right.core[i]);
    if (order) return order;
  }
  if (!left.pre.length || !right.pre.length) {
    return left.pre.length ? -1 : right.pre.length ? 1 : 0;
  }
  for (let i = 0; i < Math.min(left.pre.length, right.pre.length); i++) {
    const x = left.pre[i], y = right.pre[i];
    if (x === y) continue;
    const xn = numeric.test(x), yn = numeric.test(y);
    return xn && yn ? numberOrder(x, y) : xn !== yn ? (xn ? -1 : 1) : (x > y ? 1 : -1);
  }
  return Math.sign(left.pre.length - right.pre.length);
}
