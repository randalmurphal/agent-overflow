# Hack Nerd Font

These woff2 files are converted from the upstream `Hack.tar.xz` shipped
with the Nerd Fonts project. The unpatched Hack typeface is by Source
Foundry; the Nerd Fonts project patches it with extra icon glyphs in
the Private Use Area.

## Source

- Project: https://github.com/ryanoasis/nerd-fonts
- Release: v3.4.0
- Archive: https://github.com/ryanoasis/nerd-fonts/releases/download/v3.4.0/Hack.tar.xz
- Variant: `HackNerdFont-*` (proportional-icon, mono Latin) — *not*
  `HackNerdFontMono-*` (single-width icons) and *not*
  `HackNerdFontPropo-*` (fully proportional Latin).

## Conversion

Converted from TTF to woff2 with `fontTools` 4.62.1 + `brotli` 1.2.0:

```python
from fontTools.ttLib import TTFont
font = TTFont('HackNerdFont-Regular.ttf')
font.flavor = 'woff2'
font.save('HackNerdFont-Regular.woff2')
```

No subsetting — the whole point of Nerd Font is the icon glyph set, so
the unicode-range stays at default and every glyph survives the round
trip.

## License

`LICENSE.md` in this directory is the upstream license bundle.
The Hack typeface itself is MIT (Source Foundry); the DejaVu glyphs
incorporated into the Nerd Fonts patcher are public domain;
Bitstream Vera glyphs are under the Bitstream Vera License.
All compatible with bundling and redistribution.

## Refreshing

To pull a newer Nerd Fonts release:

1. Download the matching `Hack.tar.xz` from the Nerd Fonts releases page.
2. Extract `HackNerdFont-{Regular,Bold,Italic,BoldItalic}.ttf`.
3. Convert each to woff2 with the snippet above.
4. Replace the woff2 files in this directory and update the version
   pin recorded above.
5. Re-run `pnpm run build` and confirm the lazy chunk still
   contains all four files.
