// Empty stand-in for CSS imports during vitest runs. happy-dom doesn't
// load styles and components that `import 'foo.css'` (e.g. KaTeX, which
// svelte-streamdown's Math element pulls in) would otherwise crash the
// test loader with `Unknown file extension ".css"`. Aliased in
// `vitest.config.ts` so any `*.css` import resolves here.
export {};
