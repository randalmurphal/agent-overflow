// Sidebar: shows the threads list, sectioned by project. Click to
// select. Subscribes to `AppState.threads` and `AppState.selection` so
// the active row stays highlighted as state changes elsewhere.
//
// Spike-level scope:
//   - No virtualisation (we render the actual list straight out).
//     The Svelte sidebar uses real virtualisation; revisit if a real
//     workload puts thousands of threads in a single project.
//   - No filtering / search yet.
//   - No archived view yet — we just hide archived rows.

use std::sync::Arc;

use gpui::{
    Context, InteractiveElement, IntoElement, ParentElement, Render, SharedString, Styled,
    Window, div, px,
};

use crate::app::{AppState, ThreadsModel, select_thread};
use crate::models::Thread;
use crate::theme::Theme;

pub struct Sidebar {
    state: Arc<AppState>,
}

impl Sidebar {
    pub fn new(state: Arc<AppState>, cx: &mut Context<Self>) -> Self {
        // Re-render on threads or selection changes.
        cx.observe(&state.threads, |_, _, cx| cx.notify()).detach();
        cx.observe(&state.selection, |_, _, cx| cx.notify())
            .detach();
        Self { state }
    }
}

impl Render for Sidebar {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let theme = self.state.theme;
        let model: &ThreadsModel = self.state.threads.read(_cx);
        let selected_id: Option<String> = self
            .state
            .selection
            .read(_cx)
            .thread_id
            .clone();

        let header = div()
            .h(px(40.))
            .px_3()
            .flex()
            .items_center()
            .justify_between()
            .border_b_1()
            .border_color(theme.border)
            .child(div().font_weight(gpui::FontWeight::SEMIBOLD).child("Threads"))
            .child(
                div()
                    .text_color(theme.text_muted)
                    .text_size(px(11.))
                    .child(format!("{}", model.list.iter().filter(|t| !t.archived).count())),
            );

        let mut list = div().flex().flex_col().py_1().w_full();

        if let Some(err) = &model.error {
            list = list.child(error_row(theme, err));
        } else if model.list.is_empty() {
            list = list.child(
                div()
                    .px_3()
                    .py_4()
                    .text_color(theme.text_muted)
                    .child("No threads. Connect to a project to start one."),
            );
        } else {
            for thread in model.list.iter().filter(|t| !t.archived) {
                let is_selected = selected_id.as_deref() == Some(thread.id.as_str());
                list = list.child(thread_row(theme, thread, is_selected, self.state.clone()));
            }
        }

        div().flex().flex_col().w_full().h_full().child(header).child(list)
    }
}

fn thread_row(
    theme: Theme,
    thread: &Thread,
    selected: bool,
    state: Arc<AppState>,
) -> impl IntoElement {
    let id = thread.id.clone();
    let title: SharedString = thread.display_title().to_string().into();
    let subtitle = if thread.provider.is_empty() {
        String::new()
    } else if thread.model.is_empty() {
        thread.provider.clone()
    } else {
        format!("{} · {}", thread.provider, thread.model)
    };
    let unread = thread.is_unread();

    let bg = if selected {
        theme.surface_alt
    } else {
        theme.surface
    };
    let border_color = if selected {
        theme.accent
    } else {
        gpui::transparent_black()
    };

    div()
        .px_3()
        .py_2()
        .bg(bg)
        .border_l_2()
        .border_color(border_color)
        .hover(|s| s.bg(theme.surface_alt))
        .cursor_pointer()
        .flex()
        .flex_col()
        .gap_0p5()
        .on_mouse_down(gpui::MouseButton::Left, {
            let state = state.clone();
            let id = id.clone();
            move |_event, _window, _cx| {
                select_thread(&state, id.clone());
            }
        })
        .child(
            div()
                .flex()
                .items_center()
                .justify_between()
                .child(
                    div()
                        .font_weight(if unread {
                            gpui::FontWeight::SEMIBOLD
                        } else {
                            gpui::FontWeight::MEDIUM
                        })
                        .text_color(theme.text)
                        .child(title),
                )
                .child(if thread.has_actionable_proposed_plan {
                    div()
                        .text_color(theme.warning)
                        .text_size(px(10.))
                        .child("plan")
                        .into_any_element()
                } else if thread.has_incomplete_turn {
                    div()
                        .text_color(theme.danger)
                        .text_size(px(10.))
                        .child("interrupted")
                        .into_any_element()
                } else if unread {
                    div()
                        .w(px(8.))
                        .h(px(8.))
                        .rounded_full()
                        .bg(theme.accent)
                        .into_any_element()
                } else {
                    div().into_any_element()
                }),
        )
        .child(
            div()
                .text_size(px(11.))
                .text_color(theme.text_muted)
                .child(subtitle),
        )
}

fn error_row(theme: Theme, message: &str) -> impl IntoElement {
    div()
        .px_3()
        .py_2()
        .bg(theme.surface_alt)
        .text_color(theme.danger)
        .text_size(px(11.))
        .child(format!("error: {message}"))
}
