// Bottom status bar: connection state + a counter or two. Keeps the
// transport status visible without owning a chunk of the main layout.

use std::sync::Arc;

use gpui::{
    Context, IntoElement, ParentElement, Render, SharedString, Styled, Window, div, px,
};

use crate::app::AppState;
use crate::transport::TransportStatus;

pub struct StatusBar {
    state: Arc<AppState>,
}

impl StatusBar {
    pub fn new(state: Arc<AppState>, cx: &mut Context<Self>) -> Self {
        cx.observe(&state.status, |_, _, cx| cx.notify()).detach();
        cx.observe(&state.threads, |_, _, cx| cx.notify()).detach();
        cx.observe(&state.timeline, |_, _, cx| cx.notify()).detach();
        Self { state }
    }
}

impl Render for StatusBar {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let theme = self.state.theme;
        let status = self.state.status.read(_cx).transport;
        let threads_count = self.state.threads.read(_cx).list.len();
        let item_count = self.state.timeline.read(_cx).items.len();

        let (label, color) = match status {
            TransportStatus::Connected => ("connected", theme.success),
            TransportStatus::Connecting => ("connecting…", theme.warning),
            TransportStatus::Reconnecting => ("reconnecting…", theme.warning),
            TransportStatus::Disconnected => ("disconnected", theme.danger),
        };

        div()
            .flex()
            .items_center()
            .gap_4()
            .w_full()
            .text_size(px(11.))
            .text_color(theme.text_muted)
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .child(div().w(px(8.)).h(px(8.)).rounded_full().bg(color))
                    .child(SharedString::from(label.to_string())),
            )
            .child(
                div().child(SharedString::from(format!("{threads_count} threads"))),
            )
            .child(div().child(SharedString::from(format!("{item_count} items"))))
    }
}
