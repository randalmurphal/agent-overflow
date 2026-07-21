#!/usr/bin/env python3
"""Slice the Hack Nerd Font faces into unicode-range loading units.

Hack Nerd Font is one monolithic file per face: ~1,600 text/symbol
glyphs plus ~10,300 Nerd icon glyphs (BMP PUA + plane-15 PUA-A
material icons). A loaded @font-face pins its whole decoded sfnt in
renderer memory (~2.7 MB per face measured 2026-07-21), so a terminal
session that renders plain ASCII pays for the full icon set three
faces over.

This script partitions each face's cmap into slices and emits one
woff2 + @font-face block (with unicode-range) per slice. The union of
the slices is exactly the original cmap — no glyph is dropped, and a
codepoint outside every range falls through to the next family in the
stack exactly like a cmap miss does today. The browser then fetches
only the slices whose ranges intersect rendered text, so decoded
memory tracks what a session actually displays.

Slice boundaries (chosen from measured coverage, Nerd Fonts v3.4.0):

  text       cp < U+2500                 latin/greek/cyrillic/punct/math
  symbols    U+2500..U+DFFF              box drawing, blocks, braille --
                                         spinner + TUI territory
  powerline  U+E0A0..U+E0D7              prompt separators; tiny, carved
                                         out so a starship/oh-my-posh
                                         prompt doesn't pull every icon
  icons      U+E000..U+F8FF minus        BMP PUA Nerd icons
             powerline
  icons2     cp > U+F8FF                 plane-15 material design icons
                                         (the majority of the file)

Usage:
  slice_fonts.py <input-dir>

<input-dir> must hold HackNerdFont-{Regular,Bold,Italic,BoldItalic}
as .woff2 or .ttf (the upstream tar's TTFs work directly). Sliced
woff2 files are written next to this script; the @font-face CSS is
written to src/lib/utils/hack-nerd.css. Requires fonttools + brotli
(see SOURCE.md for the venv one-liner).

The script verifies its own output: exact cmap partition per face and
glyph-outline + advance-width fidelity for every sliced codepoint, and
fails loudly on any mismatch. Coverage variance BETWEEN faces (upstream
Bold maps a few codepoints Regular doesn't) is reported, not fatal —
per-face unicode-ranges reproduce the monolithic files' cmap-miss
fallback exactly.
"""

from __future__ import annotations

import sys
from pathlib import Path

from fontTools import subset
from fontTools.ttLib import TTFont

FACES = {
    "Regular": ("normal", 400),
    "Bold": ("normal", 700),
    "Italic": ("italic", 400),
    "BoldItalic": ("italic", 700),
}

POWERLINE = (0xE0A0, 0xE0D7)


def bucket_of(cp: int) -> str:
    if cp < 0x2500:
        return "text"
    if cp < 0xE000:
        return "symbols"
    if POWERLINE[0] <= cp <= POWERLINE[1]:
        return "powerline"
    if cp <= 0xF8FF:
        return "icons"
    return "icons2"


SLICE_ORDER = ["text", "symbols", "powerline", "icons", "icons2"]


def to_ranges(cps: list[int]) -> list[tuple[int, int]]:
    ranges: list[tuple[int, int]] = []
    for cp in cps:
        if ranges and cp == ranges[-1][1] + 1:
            ranges[-1] = (ranges[-1][0], cp)
        else:
            ranges.append((cp, cp))
    return ranges


def css_range(ranges: list[tuple[int, int]]) -> str:
    parts = [
        f"U+{a:X}" if a == b else f"U+{a:X}-{b:X}"
        for a, b in ranges
    ]
    return ", ".join(parts)


def glyph_signature(font: TTFont, cp: int):
    """(advance, lsb, outline bytes) for the glyph mapped at cp."""
    name = font.getBestCmap()[cp]
    glyf = font["glyf"]
    glyph = glyf[name]
    # expand() then compile() normalizes both fonts to the same byte
    # form regardless of how the file stored them.
    glyph.expand(glyf)
    return font["hmtx"][name], glyph.compile(glyf)


def slice_face(src: Path, out_dir: Path) -> dict[str, list[tuple[int, int]]]:
    """Write one woff2 per slice for the face at src.

    Returns {slice name: codepoint ranges} for CSS emission.
    """
    face = src.stem.split("-")[1]
    original = TTFont(src)
    cmap = original.getBestCmap()
    slices: dict[str, list[int]] = {name: [] for name in SLICE_ORDER}
    for cp in sorted(cmap):
        slices[bucket_of(cp)].append(cp)

    covered: set[int] = set()
    ranges: dict[str, list[tuple[int, int]]] = {}
    for name, cps in slices.items():
        if not cps:
            raise SystemExit(f"{src.name}: slice {name} is empty — "
                             "boundaries no longer match the font")
        opts = subset.Options()
        opts.flavor = "woff2"
        opts.layout_features = ["*"]
        subsetter = subset.Subsetter(opts)
        subsetter.populate(unicodes=cps)
        sliced = TTFont(src)
        subsetter.subset(sliced)

        out = out_dir / f"HackNerdFont-{face}.{name}.woff2"
        sliced.save(out)

        # Verify from the written file, not the in-memory object. The
        # written cmap may carry a few EXTRA entries beyond the slice:
        # glyph closure (shared outlines, GSUB alternates) can retain a
        # glyph whose codepoint lives in another slice, and the
        # subsetter keeps cmap entries for every retained glyph. Those
        # extras are inert — unicode-range is what gates face
        # selection, and each extra's range entry lives in its home
        # slice — so require only that the extra maps to the same glyph
        # the original font maps it to.
        written = TTFont(out)
        wc = written.getBestCmap()
        if missing := set(cps) - set(wc):
            raise SystemExit(f"{out.name}: cmap lost {sorted(missing)[:5]}")
        for extra in set(wc) - set(cps):
            if wc[extra] != cmap.get(extra):
                raise SystemExit(f"{out.name}: extra cmap entry U+{extra:04X} "
                                 f"remapped: {wc[extra]} != {cmap.get(extra)}")
        if overlap := covered & set(cps):
            raise SystemExit(f"{out.name}: overlaps other slices: {sorted(overlap)[:5]}")
        covered |= set(cps)
        for cp in cps:
            if glyph_signature(original, cp) != glyph_signature(written, cp):
                raise SystemExit(f"{out.name}: glyph U+{cp:04X} not identical to original")
        ranges[name] = to_ranges(cps)
        print(f"  {out.name:42} {out.stat().st_size / 1024:7.1f} KB  "
              f"{len(cps):5} cps, {len(ranges[name]):3} ranges")

    if covered != set(cmap):
        missing = sorted(set(cmap) - covered)
        raise SystemExit(f"{src.name}: {len(missing)} codepoints uncovered: {missing[:5]}")
    return ranges


CSS_HEADER = """\
/* GENERATED by ../../assets/fonts/hack-nerd/slice_fonts.py — edit
 * that script, not this file. Lazy-loaded via
 * `import('./hack-nerd.css')` in fonts.ts.
 *
 * Each face is sliced into unicode-range loading units so the browser
 * fetches (and pays decoded-sfnt memory for) only the glyph ranges a
 * session actually renders — plain terminal output loads the text
 * slice, not the ~10k Nerd icon glyphs. The slices partition the full
 * Nerd Font cmap: nothing is dropped, and uncovered codepoints fall
 * through the family stack exactly as a cmap miss always has.
 *
 * Hack ships only weights 400 + 700; 500/600 fall back to the nearest
 * match. See ../../assets/fonts/hack-nerd/SOURCE.md for upstream
 * version + license. */
"""


def emit_css(all_ranges: dict[str, dict[str, list[tuple[int, int]]]], css_path: Path) -> None:
    blocks = [CSS_HEADER]
    for face, (style, weight) in FACES.items():
        for name in SLICE_ORDER:
            blocks.append(
                "@font-face {\n"
                "  font-family: 'Hack Nerd Font';\n"
                f"  font-style: {style};\n"
                f"  font-weight: {weight};\n"
                "  font-display: swap;\n"
                f"  src: url('../../assets/fonts/hack-nerd/HackNerdFont-{face}.{name}.woff2') format('woff2');\n"
                f"  unicode-range: {css_range(all_ranges[face][name])};\n"
                "}\n"
            )
    css_path.write_text("\n".join(blocks))
    print(f"  {css_path} written")


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    in_dir = Path(sys.argv[1])
    out_dir = Path(__file__).resolve().parent
    css_path = out_dir.parents[2] / "lib" / "utils" / "hack-nerd.css"
    if not css_path.parent.is_dir():
        raise SystemExit(f"css target dir missing: {css_path.parent}")

    all_ranges: dict[str, dict[str, list[tuple[int, int]]]] = {}
    for face in FACES:
        src = next(
            (p for ext in (".woff2", ".ttf") if (p := in_dir / f"HackNerdFont-{face}{ext}").exists()),
            None,
        )
        if src is None:
            raise SystemExit(f"missing HackNerdFont-{face}.woff2/.ttf in {in_dir}")
        print(f"{src.name}:")
        all_ranges[face] = slice_face(src, out_dir)

    # Upstream faces vary slightly in coverage (Bold carries a few
    # more text glyphs than Regular, italics fewer symbols). That's
    # fine: each face's unicode-range mirrors ITS OWN cmap, so the
    # candidate set for any codepoint equals the faces that can
    # actually render it — the same outcome the monolithic files
    # reach via per-face cmap-miss fallback. Report the variance so a
    # future upstream bump can see it move.
    baseline = all_ranges["Regular"]
    for face, ranges in all_ranges.items():
        for name in SLICE_ORDER:
            a = sum(b - x + 1 for x, b in baseline[name])
            f = sum(b - x + 1 for x, b in ranges[name])
            if f != a:
                print(f"  note: {face}.{name} covers {f} cps vs Regular's {a}")

    emit_css(all_ranges, css_path)


if __name__ == "__main__":
    main()
