# Account auto-switch

Status: approved in conversation 2026-11-13; implemented with this spec.

## Goal

A person with several saved accounts for one provider should not have to
watch usage meters. When the selected account cannot serve the models that
are running, Agent Overflow selects another saved account that can, and can
resume threads a usage limit stopped.

The selected account stays provider-wide: every thread of a provider, and the
provider's own CLI outside the app, uses it. Auto-switch changes which account
is selected. It does not give threads separate accounts and it does not change
how a selection reaches a session: a Claude session uses the new login on its
next request, a Codex session at its next send once its turn and background
work have finished.

## Terms

- **Pool**: every saved account of one provider except those whose
  **Auto-switch** checkbox is cleared on their card. Auto-switch acts for a
  provider only while the selected account is in the pool and the pool has
  another usable account. An account that needs a sign-in is not usable.
- **Provider-wide limit**: a limit that applies to every model (Claude
  `session` and `weekly_all`, Codex `codex`). Every other limit is **scoped**.
- **Applies**: a provider-wide limit applies to every model. A scoped limit
  applies to a model when its bucket names the model's family (Claude
  `weekly_scoped:fable` and `claude-fable-5-2`; Codex "GPT-5.3-Codex-Spark"
  and `gpt-5.3-codex-spark`). A scoped limit that names no known family
  applies to no model.
- **State of an account for a model**, from the limits that apply:
  - *exhausted*: a limit is at 100% or more, or the account holds a refusal
    mark for the model;
  - *past threshold*: a limit is at or above the threshold setting;
  - *clear*: otherwise.
  A limit whose reset time has passed counts as 0%.
- **Demand**: the models of the provider's threads with a turn in flight,
  plus the model of a thread that is about to send.
- **Score** of an account for a demand: the number of demanded models it is
  exhausted for, then the number it is past threshold for. Lower is better.

## Choosing an account

The **pool order** comes from the "Use accounts in order of" setting:

- **Soonest weekly reset** (default): accounts ordered by the reset time of
  their longest provider-wide window, soonest first. Quota that resets unused
  is lost, so this order drains it first. An account whose window has not
  started, or has already reset, has nothing to lose and sorts last. Ties
  keep the order of the cards.
- **Card order**: the order of the cards in Settings.

A switch is **forced** when another usable pool account has a strictly better
score than the selected one. The target is the best-scoring account, the
earliest in the pool order among equals. It follows that:

- an account past the threshold is left only for an account that is clear;
  when every account is past the threshold, the selected one is used until
  the provider refuses it;
- a scoped limit matters only while a thread of that model is running or
  sending. An account with no Fable usage left still serves Opus threads;
- when no account can serve every demanded model, the account that serves
  the most of them is selected, and no switch happens between accounts that
  serve equally many.

An **order switch** moves to a pool account earlier in the pool order when it
and the selected account are both clear for the demand. It happens only
before a send, only when no other turn of the provider is in flight, and only
when the selection is not pinned. A selection is pinned when a person or an
outside login made it; a forced switch unpins it. This is what returns work
to an account after its window resets without moving running turns for it.

## When it decides

1. **Before a send** from a thread with no turn of its own in flight: a
   forced or an order switch, with that thread's model in the demand. This
   runs on the send path from cached readings and makes no network request.
2. **When a reading for the selected account arrives**, for a provider whose
   running sessions follow a switch (Claude): forced only. Codex is left out
   because a switch cannot reach a running Codex turn; its next send decides.
3. **When a turn is refused for a usage limit**: the account the session was
   using gets a [refusal mark](#refusal-marks), a fresh reading is requested,
   and a forced decision runs with the refused thread's model in the demand.

Off the send path, a target whose reading is older than five minutes is read
again before the switch; a failed read does not block it. After every switch
the new account's usage is read. If that reading shows it cannot serve the
demand, the next decision moves on. A stale reading therefore costs at most
one switch per account: the reading taken after the switch, or a refusal
mark, records what it missed.

A target that turns out to need a sign-in is skipped. A switch that fails for
any other reason is reported and the send continues on the selected account.

## Refusal marks

Readings can lag a refusal, and a provider can enforce a limit the reading
does not name. A typed usage-limit refusal therefore marks the session's
account as exhausted for the refused thread's model family. The mark lasts
until the reset time of the most used limit that applies to the model, or of
the most used limit of any kind when none applies, or one hour when the
account has no reading. When that limit is provider-wide and past the
threshold the mark covers every model. Marks are kept in memory.

A provider does not say which account refused a request, and a request in
flight at a switch can still be refused on the previous account. For two
minutes after a switch, a refusal of a turn that was running when the
selection moved can therefore belong to any account the turn used, and one
turn can be refused more than once (a subagent, then its parent). Such a
refusal marks no account: the accounts are read again and the decision uses
those readings. A send to the thread starts a turn on the selected account,
and that turn's refusals mark it.

A subagent's refusal is attributed to its thread's model.

## Continue after a limit

Setting **Continue threads after a usage limit**, off by default. While it is
on, a thread whose turn was refused for a usage limit is sent

> Continue. The usage limit that stopped this thread no longer applies.

once the selected account is not exhausted for the thread's model: right
after a switch, or after a reset. A reset counts once a reading taken after
the reset time confirms it, or five minutes after the reset time when no
reading can be taken. A check runs every minute while a thread is waiting,
and it also switches to a pool account whose limit has reset. The check uses
the model the thread would run on now, and it runs once more when the message
is queued: if the selected account, its reading or the thread's model changed
meanwhile so that the account is exhausted for it, nothing is queued and the
thread keeps waiting.

The message is queued like any app-written message, shows an "Agent Overflow"
origin, and keeps the composer draft. A thread stops waiting when any message
is sent to it, when it is archived or deleted, when its provider has no
selected account, or when the setting is turned off. Turning it off ends
every wait that began before, also when it is turned on again at once.
Waiting threads are kept
in memory: a restart forgets them, as a restart never dispatches queued
messages on its own.

When no reading names a reset time, the wait is the refusal mark's one-hour
guess. A refusal on a guess within two minutes of a continue sent after a
guessed wait does not wait again: the thread gets a notice asking for a
message instead. Any other refusal waits again.

Workflow-owned and hidden threads never receive it. A thread another agent
spawned does, which is why the setting warns that it restarts delegated work.
A workflow run parked on a usage limit stays parked until it is resumed
([D75](workflows-system-decisions.md)); running workflow threads count in the
demand like any other thread.

## What the person sees

- A toast on every switch the app makes, naming both accounts and the limit
  that caused it, and a warning toast when a switch fails.
- A timeline notice in the thread whose send or refusal caused the switch,
  and in every thread with a turn in flight when a reading caused it.
- In a refused thread that no account can serve: a notice with the earliest
  reset among the accounts that could serve it, and whether the thread will
  continue then.
- Every switch in the account audit log.

## Settings

| Setting | Values | Default |
|---|---|---|
| `accountAutoSwitchStrategy` | `soonest-reset`, `account-order` | `soonest-reset` |
| `accountAutoSwitchThreshold` | 50 to 100 percent | 95 |
| `accountAutoContinue` | boolean | off |

They apply to every provider and are user tier. The setting labels are "Use
accounts in order of" (`account-order` is "Card order"), "Switch at" and
"Continue threads after a usage limit". The per-account **Auto-switch**
checkbox is on by default, so a saved or newly added account is in the pool
until it is cleared. It is stored in `provider-accounts.json`
beside the cards and is forgotten with the account: an account added again
starts in the pool.

## Usage readings

Auto-switch reads the same account-scoped cache the meters read. The poll
runs every minute while a provider has a turn in flight or has completed one
since the last poll, and after every switch, manual or automatic. A reading
that the endpoint throttles stops readings for that account until the backoff
ends; refusal marks cover that gap.

## Not built

- Separate accounts per thread, or a proxy that picks an account per request.
- A change to how a Codex session adopts a selection: a message sent into a
  Codex turn that is still running on the previous account is refused until
  that turn ends, as after a manual switch.
- Waiting threads that survive a restart.
- Automatic resume of a parked workflow run.
