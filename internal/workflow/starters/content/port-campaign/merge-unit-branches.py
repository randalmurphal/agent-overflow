#!/usr/bin/env python3
"""Merge a wave's lane branches into the campaign branch, skipping what conflicts.

Reference implementation of the `merge-unit-branches` command the `port-campaign`
starter binds as its fan-out join. It is CONTENT: the engine never runs it
implicitly and nothing in Agent Overflow merges anything on its own. Copy it,
edit it, replace it - what it demonstrates is the shape a merge join has to have.

Bind it in the project profile:

    commands:
      merge-unit-branches: ["python3", "<abs path to this file>", "{{units}}"]

`{{units}}` interpolates to one JSON argv element: the array of unit results the
join consolidates, each entry carrying at least `id` and `status`, and — for a
unit that produced a branch — `branch` and `worktree`.

Two properties are the whole point, and both come from one live incident where a
merge stopped at its first conflict and reported success:

1.  **Skip and continue, never stop at the first conflict.** A lane that will not
    merge is aborted back out (`git merge --abort`) and recorded; the lanes
    behind it are still merged. Stopping meant ~1900 lines of approved work
    silently never landed, because a later repair only ever looked at the one
    conflict that was reported.
2.  **Account for every unit.** The join declares `accounts_for_units: true`, so
    the engine post-validates this envelope: each unit id must appear exactly
    once across `merged` and `blocked`. Dropping a lane to make the lists tidy is
    refused rather than believed, which is what makes "the wave is merged" a
    statement that can be trusted.

    A unit a human DROPPED (`dropped` status) is one of those units. The engine
    shows dropped lanes to the join precisely so it can say what it did not
    receive, so this script accounts for one in `blocked` with a reason naming
    the drop. It does not ask for a human, because a human is who dropped it.

    The set the engine holds the join to is read from these same entries by one
    rule: a JSON object carrying a non-blank string `id`. An entry that fails
    that rule is not a unit the join is accountable for, and naming it in either
    list is refused (a blank id accounts for nothing; an unknown id is not one of
    this fan-out's units). Such an entry is therefore reported on stderr and left
    out of both lists — the one case where leaving something out is the honest
    answer — and it still makes this script exit non-zero, because a units list
    the engine could not compose is not a wave anybody should call merged.

Exit status is the gate's signal, not a failure. It answers exactly one
question: **does a human still have to land something?** A clean merge and a
deliberately dropped lane answer no; a conflict and an unusable entry answer yes.
The tool driver turns it into `passed`, and the phase's gate routes a wave that
answered yes to the phase that lands it by hand.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from typing import NamedTuple

# One merged lane is one commit, so the message is the only place a reader later
# learns which unit a merge came from.
MERGE_MESSAGE = "Merge lane {unit} into the campaign branch"
# Enough of git's own complaint to act on, without pasting a whole conflict
# report into an envelope that is size-capped.
MAX_REASON_CHARS = 800


def git(*args: str) -> subprocess.CompletedProcess[str]:
    """Run one git command in the campaign worktree (this process's cwd)."""
    return subprocess.run(
        ["git", *args], capture_output=True, text=True, check=False
    )


def truncate(text: str) -> str:
    collapsed = " ".join(text.split())
    if len(collapsed) <= MAX_REASON_CHARS:
        return collapsed
    return collapsed[:MAX_REASON_CHARS] + "…"


class NotMerged(NamedTuple):
    """Why a lane is not in the campaign branch, and whether anybody must act.

    Both halves are needed because the two are not the same question. Every
    unmerged lane has to be ACCOUNTED for — that is the contract — but only some
    of them are work still owed: a lane a human dropped is already decided, and
    routing the wave to a hand-landing phase for it would ask somebody to undo
    their own decision.
    """

    reason: str
    needs_human: bool


def accountable_id(entry: object) -> str | None:
    """The unit id this entry can be accounted for under, or None.

    It mirrors the engine's own read of the `units` binding
    (`def.UnitIDsFromResults`) exactly: an object with a non-blank string `id`.
    The two must agree — the engine holds this join to the ids IT could read, so
    an entry that fails here is one the engine is not expecting either, and
    naming it in `merged` or `blocked` would be refused rather than credited.
    """
    if not isinstance(entry, dict):
        return None
    unit_id = entry.get("id")
    if not isinstance(unit_id, str) or not unit_id.strip():
        return None
    return unit_id


def merge_one(unit: dict) -> NotMerged | None:
    """Merge one lane. Returns None on success, or why it is not merged.

    Every early return is a REASON rather than an exception: a unit this script
    cannot merge is a unit the join reports, and a crash here would produce no
    envelope at all — which is the one outcome that loses the whole wave.
    """
    status = unit.get("status")
    if status == "dropped":
        # Not a failure and not this script's call. A drop is the human's
        # "proceed without it", so the lane is accounted for and nobody is asked
        # to land it.
        return NotMerged("the unit was dropped, so the wave proceeds without it "
                         "and there is nothing to land", needs_human=False)
    if status != "done":
        return NotMerged(f"the lane did not finish: its unit rested {status!r}", needs_human=True)
    branch = (unit.get("branch") or "").strip()
    if not branch:
        return NotMerged("the lane produced no branch to merge", needs_human=True)
    if git("rev-parse", "--verify", "--quiet", branch + "^{commit}").returncode != 0:
        return NotMerged(f"branch {branch!r} does not exist in this repository", needs_human=True)

    ahead = git("rev-list", "--count", "HEAD.." + branch)
    if ahead.returncode == 0 and ahead.stdout.strip() == "0":
        # Not a failure and not a merge: the lane committed nothing the campaign
        # branch does not already have. It still has to be accounted for, and
        # calling that "merged" is the honest answer — its work is present.
        return None

    merged = git(
        "merge", "--no-ff", "-m", MERGE_MESSAGE.format(unit=unit.get("id", "?")), branch
    )
    if merged.returncode == 0:
        return None
    # Back the working tree out before touching the next lane. Without this the
    # campaign branch sits mid-merge and every later merge fails for a reason
    # that has nothing to do with the lane it names.
    aborted = git("merge", "--abort")
    reason = truncate(merged.stdout + "\n" + merged.stderr) or "git merge failed"
    if aborted.returncode != 0:
        reason += " (and `git merge --abort` failed; the worktree needs a human)"
    return NotMerged(reason, needs_human=True)


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(f"usage: {argv[0]} '<units JSON>'", file=sys.stderr)
        return 2
    try:
        units = json.loads(argv[1])
    except json.JSONDecodeError as error:
        print(f"units argument is not JSON: {error}", file=sys.stderr)
        return 2
    if not isinstance(units, list):
        print("units argument is not a JSON array", file=sys.stderr)
        return 2

    merged: list[str] = []
    blocked: list[dict[str, str]] = []
    needs_human = False
    for entry in units:
        unit_id = accountable_id(entry)
        if unit_id is None:
            # Left out of both lists on purpose: the engine could not read an id
            # here either, so it is holding this join to a set that does not
            # include this entry, and naming it would be refused rather than
            # credited. It is still loud, and it still fails the gate.
            needs_human = True
            print(f"unusable unit entry, accountable to nothing: {entry!r}", file=sys.stderr)
            continue
        outcome = merge_one(entry)
        if outcome is None:
            merged.append(unit_id)
            print(f"merged {unit_id}")
            continue
        blocked.append({"unit": unit_id, "reason": outcome.reason})
        needs_human = needs_human or outcome.needs_human
        print(f"blocked {unit_id}: {outcome.reason}")

    envelope = {
        "status": "done",
        "outputs": {"merged": merged, "blocked": blocked},
        "question": None,
        "reason": None,
        "narrative": None,
        "memory": None,
    }
    # The envelope is what the phase's gate reads; the process output above is
    # only the human-readable tail the runner files as the narrative.
    envelope_path = os.environ.get("AO_ENVELOPE")
    if not envelope_path:
        print("AO_ENVELOPE is not set; refusing to report a merge nothing recorded", file=sys.stderr)
        return 2
    with open(envelope_path, "w", encoding="utf-8") as handle:
        json.dump(envelope, handle, indent=2)
        handle.write("\n")
    # Not `1 if blocked`: a wave whose only unmerged lane was dropped on purpose
    # has nothing left for a human, and routing it to the hand-landing phase
    # would ask somebody to reverse their own decision.
    return 1 if needs_human else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
