// Root view: holds Arc<AppState>, lays out sidebar + main pane.

use std::sync::Arc;

use gpui::{
    Context, IntoElement, ParentElement, Render, Styled, Window, div, prelude::*, px,
};

use crate::app::AppState;
use crate::ui::{sidebar::Sidebar, status_bar::StatusBar, timeline::Timeline};

pub struct Root {
    state: Arc<AppState>,
    sidebar: gpui::Entity<Sidebar>,
    timeline: gpui::Entity<Timeline>,
    status_bar: gpui::Entity<StatusBar>,
}

impl Root {
    pub fn new(state: Arc<AppState>, cx: &mut Context<Self>) -> Self {
        let sidebar = cx.new(|cx| Sidebar::new(state.clone(), cx));
        let timeline = cx.new(|cx| Timeline::new(state.clone(), cx));
        let status_bar = cx.new(|cx| StatusBar::new(state.clone(), cx));

        Self {
            state,
            sidebar,
            timeline,
            status_bar,
        }
    }
}

impl Render for Root {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let theme = self.state.theme;
        div()
            .size_full()
            .flex()
            .flex_col()
            .bg(theme.background)
            .text_color(theme.text)
            .font_family("ui-sans-serif")
            .text_size(px(13.))
            .child(
                div()
                    .flex()
                    .flex_row()
                    .flex_grow()
                    .min_h_0()
                    .child(
                        div()
                            .w(px(280.))
                            .min_w(px(220.))
                            .flex_shrink_0()
                            .border_r_1()
                            .border_color(theme.border)
                            .bg(theme.surface)
                            .child(self.sidebar.clone()),
                    )
                    .child(
                        div()
                            .flex_grow()
                            .flex()
                            .flex_col()
                            .min_w_0()
                            .child(self.timeline.clone()),
                    ),
            )
            .child(
                div()
                    .h(px(28.))
                    .border_t_1()
                    .border_color(theme.border)
                    .bg(theme.surface)
                    .px_3()
                    .flex()
                    .items_center()
                    .child(self.status_bar.clone()),
            )
    }
}
