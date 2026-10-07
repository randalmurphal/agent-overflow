// Self-hosted fonts. Four weights of each family covers every surface the
// app uses today (body/medium/semibold/bold). Import this before the global
// stylesheet so the @font-face declarations beat any cascading font-family
// rules. The browser test project loads the same faces, so layout tests
// measure the text the app renders rather than the host's fallback font.
import '@fontsource/geist-sans/400.css';
import '@fontsource/geist-sans/500.css';
import '@fontsource/geist-sans/600.css';
import '@fontsource/geist-sans/700.css';
import '@fontsource/geist-mono/400.css';
import '@fontsource/geist-mono/500.css';
import '@fontsource/geist-mono/600.css';

/** The faces imported above, as `FontFaceSet.load` descriptors. */
export const SHIPPED_FONT_FACES: readonly string[] = [
  ...[400, 500, 600, 700].map((weight) => `${weight} 1em "Geist Sans"`),
  ...[400, 500, 600].map((weight) => `${weight} 1em "Geist Mono"`),
];
