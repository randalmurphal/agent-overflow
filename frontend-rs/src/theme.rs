// Centralised colour + spacing tokens. Mirrors the subset of the Svelte
// app.css we actually use in the spike — full Tailwind theme parity is
// out of scope; we only need enough variety for the sidebar / timeline /
// status bar to look intentional rather than placeholder-grey.

use gpui::{Hsla, hsla};

#[derive(Clone, Copy)]
pub struct Theme {
    pub background: Hsla,
    pub surface: Hsla,
    pub surface_alt: Hsla,
    pub border: Hsla,
    pub text: Hsla,
    pub text_muted: Hsla,
    pub accent: Hsla,
    pub accent_text: Hsla,
    pub success: Hsla,
    pub warning: Hsla,
    pub danger: Hsla,
}

impl Theme {
    pub fn dark() -> Self {
        Self {
            background: hsla(240. / 360., 0.10, 0.07, 1.0),
            surface: hsla(240. / 360., 0.08, 0.10, 1.0),
            surface_alt: hsla(240. / 360., 0.10, 0.13, 1.0),
            border: hsla(240. / 360., 0.06, 0.20, 1.0),
            text: hsla(220. / 360., 0.10, 0.90, 1.0),
            text_muted: hsla(220. / 360., 0.08, 0.60, 1.0),
            accent: hsla(210. / 360., 0.80, 0.55, 1.0),
            accent_text: hsla(0., 0., 1., 1.0),
            success: hsla(140. / 360., 0.55, 0.50, 1.0),
            warning: hsla(38. / 360., 0.85, 0.55, 1.0),
            danger: hsla(0. / 360., 0.65, 0.55, 1.0),
        }
    }
}
