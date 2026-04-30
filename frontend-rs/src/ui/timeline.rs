// Timeline: renders the list of items for the selected thread.
//
// Spike-level rendering matrix:
//   user message   → bordered card, primary text
//   assistant text → no border, primary text
//   tool call/result → muted card with tool name + summary
//   notification / system → small dim row with the summary as-is
//
// Payload bodies (full markdown messages, command output, diffs) are
// out of scope for the spike — Item.summary is what the Go side already
// puts in the row, and that's what we show. Loading payload bodies on
// demand mirrors the Svelte app's lazy-load pattern; bolt that on once
// the basic skeleton is alive.

use std::sync::Arc;

use gpui::{
    Context, IntoElement, ParentElement, Render, SharedString, Styled, Window, div, px,
};

use crate::app::{AppState, TimelineModel};
use crate::models::{Item, ItemLane};
use crate::theme::Theme;

pub struct Timeline {
    state: Arc<AppState>,
}

impl Timeline {
    pub fn new(state: Arc<AppState>, cx: &mut Context<Self>) -> Self {
        cx.observe(&state.timeline, |_, _, cx| cx.notify()).detach();
        cx.observe(&state.threads, |_, _, cx| cx.notify()).detach();
        cx.observe(&state.selection, |_, _, cx| cx.notify())
            .detach();
        Self { state }
    }
}

impl Render for Timeline {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let theme = self.state.theme;
        let timeline: &TimelineModel = &self.state.timeline.read(_cx);
        let selection_thread_id = self
            .state
            .selection
            .read(_cx)
            .thread_id
            .clone();
        let active_thread = selection_thread_id.as_deref().and_then(|id| {
            self.state
                .threads
                .read(_cx)
                .list
                .iter()
                .find(|t| t.id == id)
                .cloned()
        });

        let header = if let Some(thread) = &active_thread {
            div()
                .h(px(48.))
                .px_4()
                .flex()
                .items_center()
                .justify_between()
                .border_b_1()
                .border_color(theme.border)
                .child(
                    div()
                        .font_weight(gpui::FontWeight::SEMIBOLD)
                        .child(SharedString::from(thread.display_title().to_string())),
                )
                .child(
                    div()
                        .text_color(theme.text_muted)
                        .text_size(px(11.))
                        .child(if thread.model.is_empty() {
                            thread.provider.clone()
                        } else {
                            format!("{} · {}", thread.provider, thread.model)
                        }),
                )
        } else {
            div()
                .h(px(48.))
                .px_4()
                .flex()
                .items_center()
                .border_b_1()
                .border_color(theme.border)
                .text_color(theme.text_muted)
                .child("No thread selected")
        };

        let body = if let Some(err) = &timeline.error {
            div()
                .p_4()
                .text_color(theme.danger)
                .child(SharedString::from(format!("Failed to load timeline: {err}")))
                .into_any_element()
        } else if timeline.thread_id.is_none() {
            div()
                .p_4()
                .text_color(theme.text_muted)
                .child("Pick a thread on the left.")
                .into_any_element()
        } else if timeline.items.is_empty() {
            div()
                .p_4()
                .text_color(theme.text_muted)
                .child("No items yet — send a message to begin.")
                .into_any_element()
        } else {
            let mut list = div()
                .flex()
                .flex_col()
                .gap_2()
                .p_4()
                .overflow_hidden();
            for item in &timeline.items {
                list = list.child(render_item(theme, item));
            }
            list.into_any_element()
        };

        div()
            .flex()
            .flex_col()
            .size_full()
            .child(header)
            .child(div().flex_grow().min_h_0().child(body))
    }
}

fn render_item(theme: Theme, item: &Item) -> impl IntoElement {
    match item.lane() {
        ItemLane::User => render_user(theme, item).into_any_element(),
        ItemLane::Assistant => render_assistant(theme, item).into_any_element(),
        ItemLane::Tool => render_tool(theme, item).into_any_element(),
        ItemLane::System => render_system(theme, item).into_any_element(),
        ItemLane::Other => render_other(theme, item).into_any_element(),
    }
}

fn render_user(theme: Theme, item: &Item) -> impl IntoElement {
    div()
        .p_3()
        .border_1()
        .border_color(theme.border)
        .bg(theme.surface)
        .rounded_md()
        .child(
            div()
                .flex()
                .items_center()
                .gap_2()
                .text_color(theme.accent)
                .text_size(px(11.))
                .font_weight(gpui::FontWeight::SEMIBOLD)
                .child("you"),
        )
        .child(
            div()
                .mt_1()
                .text_color(theme.text)
                .child(SharedString::from(item.summary.clone())),
        )
}

fn render_assistant(theme: Theme, item: &Item) -> impl IntoElement {
    div()
        .px_3()
        .py_2()
        .child(
            div()
                .flex()
                .items_center()
                .gap_2()
                .text_color(theme.text_muted)
                .text_size(px(11.))
                .font_weight(gpui::FontWeight::SEMIBOLD)
                .child("assistant"),
        )
        .child(
            div()
                .mt_1()
                .text_color(theme.text)
                .child(SharedString::from(item.summary.clone())),
        )
}

fn render_tool(theme: Theme, item: &Item) -> impl IntoElement {
    let tool = item
        .tool_name
        .as_deref()
        .filter(|s| !s.is_empty())
        .unwrap_or(&item.kind);
    let status = item.status.clone();
    div()
        .p_3()
        .bg(theme.surface)
        .border_l_2()
        .border_color(theme.text_muted)
        .rounded_md()
        .child(
            div()
                .flex()
                .items_center()
                .gap_2()
                .text_color(theme.text_muted)
                .text_size(px(11.))
                .font_weight(gpui::FontWeight::SEMIBOLD)
                .child(SharedString::from(format!("tool · {tool}")))
                .child(if status.is_empty() {
                    div()
                } else {
                    div()
                        .text_size(px(10.))
                        .px_1()
                        .text_color(theme.warning)
                        .child(SharedString::from(status))
                }),
        )
        .child(
            div()
                .mt_1()
                .text_color(theme.text)
                .text_size(px(12.))
                .child(SharedString::from(item.summary.clone())),
        )
}

fn render_system(theme: Theme, item: &Item) -> impl IntoElement {
    div()
        .px_3()
        .py_1()
        .text_size(px(11.))
        .text_color(theme.text_muted)
        .child(SharedString::from(format!("• {}", item.summary)))
}

fn render_other(theme: Theme, item: &Item) -> impl IntoElement {
    div()
        .px_3()
        .py_1()
        .text_size(px(11.))
        .text_color(theme.text_muted)
        .child(SharedString::from(format!(
            "[{}] {}",
            item.kind, item.summary
        )))
}
