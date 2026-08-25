// One-shot Runtime.evaluate against the app page: probe evalq '<expr>'. Runtime.evaluate only — safe beside a running sampler.
import { connectPage, done, evaluate } from './lib/cdp.mjs';

const expr = process.argv[2];
if (!expr) {
  console.error("usage: probe evalq '<js expression>'");
  process.exit(2);
}

const page = await connectPage();
try {
  const result = await evaluate(page, `JSON.stringify((() => (${expr}))(), null, 1)`);
  console.log(result);
} finally {
  await done([page]);
}
