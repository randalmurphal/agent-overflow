// Source-map frame resolution for the probes. The production bundle is
// minified, so V8 profile frames land on bundle line:col; when the build
// ran with AO_SOURCEMAP=1 (vite emits 'hidden' maps beside the assets),
// this resolves them to original file:line plus the original identifier
// for frames the minifier anonymized. No dependencies: a source map v3
// is base64-VLQ segments, ~40 lines to decode.
//
// Maps are fetched over the app's own asset server (`<bundle url>.map`),
// so the probe needs no filesystem view of the repo. A missing map (404,
// build without AO_SOURCEMAP) resolves to null and callers fall back to
// the raw bundle coordinate.

const B64 = new Int8Array(128).fill(-1);
for (let i = 0; i < 64; i += 1) {
  B64['ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'.charCodeAt(i)] = i;
}

// mappings → per generated line, array of [genCol, srcIdx, srcLine, srcCol, nameIdx]
// (segments in generated-column order; nameIdx -1 when absent).
function decodeMappings(mappings) {
  const lines = [];
  let line = [];
  let genCol = 0;
  let src = 0;
  let srcLine = 0;
  let srcCol = 0;
  let name = 0;
  const n = mappings.length;
  let i = 0;
  while (i <= n) {
    const code = i === n ? 59 : mappings.charCodeAt(i);
    if (code === 59 /* ; */) {
      lines.push(line);
      line = [];
      genCol = 0;
      i += 1;
      continue;
    }
    if (code === 44 /* , */) {
      i += 1;
      continue;
    }
    const vals = [];
    while (i < n) {
      const c = mappings.charCodeAt(i);
      if (c === 59 || c === 44) break;
      let shift = 0;
      let value = 0;
      let digit;
      do {
        digit = B64[mappings.charCodeAt(i)];
        i += 1;
        value += (digit & 31) << shift;
        shift += 5;
      } while (digit & 32);
      vals.push(value & 1 ? -(value >>> 1) : value >>> 1);
    }
    genCol += vals[0];
    if (vals.length >= 4) {
      src += vals[1];
      srcLine += vals[2];
      srcCol += vals[3];
      if (vals.length >= 5) name += vals[4];
      line.push([genCol, src, srcLine, srcCol, vals.length >= 5 ? name : -1]);
    }
  }
  return lines;
}

function shortSource(path) {
  if (!path) return path;
  // rolldown emits sources as relative paths ("../../src/lib/…",
  // "../../vendor/…"); strip the climb so output matches repo paths.
  return path.replace(/^(\.\.\/)+/, '');
}

/**
 * Loads the hidden maps for every given bundle URL (silently skipping
 * bundles without one) and returns a synchronous
 * `resolve(url, line0, col0) -> { source, line, name } | null`.
 */
export async function createFrameResolver(urls) {
  const maps = new Map();
  await Promise.all(
    [...new Set(urls)].map(async (url) => {
      if (!/^https?:.*\.js$/.test(url)) return;
      try {
        const r = await fetch(url + '.map');
        if (!r.ok) return;
        const m = await r.json();
        maps.set(url, {
          sources: m.sources.map(shortSource),
          names: m.names,
          lines: decodeMappings(m.mappings),
        });
      } catch {
        // No map for this bundle — frames resolve raw.
      }
    }),
  );
  return function resolve(url, line0, col0) {
    const m = maps.get(url);
    if (!m) return null;
    const segs = m.lines[line0];
    if (!segs || segs.length === 0) return null;
    // Last segment with genCol <= col0.
    let lo = 0;
    let hi = segs.length - 1;
    if (segs[0][0] > col0) return null;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (segs[mid][0] <= col0) lo = mid;
      else hi = mid - 1;
    }
    const seg = segs[lo];
    return {
      source: m.sources[seg[1]],
      line: seg[2] + 1,
      name: seg[4] >= 0 ? m.names[seg[4]] : null,
    };
  };
}
