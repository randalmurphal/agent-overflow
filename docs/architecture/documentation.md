# Documentation maintenance

Use this policy when adding, changing or removing guides, documentation,
comments or docstrings. Their purpose is to help a future reader make a
correct change with the least necessary reading.

## Choose one home

| Information | Home |
|---|---|
| Essential project-wide engineering constraint | Root `AGENTS.md` |
| Area responsibility, unique constraint or task-specific reading route | Nearest useful area `AGENTS.md` |
| Mechanism involving several files or packages | Focused document in `docs/architecture/` |
| API inputs, outputs, errors, ownership or concurrency contract | Comment on the API or owning type |
| Local ordering, lifetime or external-library constraint | Comment beside the relevant code |
| Product choice that code cannot explain | Owning spec or `docs/decisions.md` |
| External behavior and evidence needed to interpret it | `docs/references/` |
| Commands, defaults, schemas or generated shapes | Their executable/configuration source; link to it |
| Completed work, review discussion or incident chronology | Commit or review history; omit from maintained instructions |

Search for an existing explanation before adding one. Correct or extend its
owner, then link from other places that need it. Do not copy a rule into
parents, children, docs and comments. A short routing summary can identify
what a reference covers without repeating its contract.

## What belongs in a guide

An `AGENTS.md` is repeatedly loaded as instructions. Keep only material
that helps select the right code, find the relevant contract or preserve
an essential constraint while changing that area.

For each proposed entry, check:

1. Does it change an implementation, validation or navigation decision?
2. Is it needed throughout this scope, or only for a narrower task?
3. Is this information already clear from code, tooling or inherited rules?
4. Is the claim current, and is there an authoritative implementation,
   test, external reference or explicit product decision behind it?

Delete entries that supply no useful decision. Move narrower material to
its owner. Verify stale claims against current code; do not retain a rule
merely because a previous guide called it permanent.

A guide usually needs a brief purpose, a small responsibility map, the
constraints unique to its area and links labeled by task. Small packages
may need no guide at all. Do not create one for every directory or package.
Do not fill template sections that have nothing useful to say.

Read a proposed guide together with its ancestors. Remove repeated rules
and avoid instructions to read whole documentation trees. A link should
state when it applies, such as "Changing session renewal: read ...".
Cross-area contracts have one document linked from each participating area.

## What belongs beside code

Explain information a maintainer cannot reliably infer from the body:
resource ownership, valid inputs, observable failures, thread safety,
ordering requirements, or constraints imposed by an external API.

Do not narrate assignments, loops or obvious branches. Do not write a
paragraph defending an avoidable workaround. Prefer a clearer name, type
or API when that makes the explanation unnecessary. Explain a necessary
constraint in terms of current behavior, not the bug or review that found it.
Tests demonstrate behavior; comments need not list every regression test.
Keep useful public API documentation even when it describes what an API does.

## Style and organization

- Use short, direct sentences and ordinary engineering terms. Name the
  actual mechanism: validation, permissions, ownership, cancellation,
  retries, persistence or concurrency. Avoid dramatic, adversarial or
  military metaphors when a precise engineering term exists.
- Do not use em dashes in authored prose. Preserve exact API names,
  protocol fields, commands and quoted external material when necessary.
- State the requirement and enough reason to apply it correctly. Avoid
  slogans, repeated emphasis, exhaustive symbol lists and speculative advice.
- Keep headings specific to the reader's task. Split a document when its
  sections serve independent tasks; do not create a new file per small fact.
- Keep work logs, completion reports, review history and incident narratives
  out of guides and current-reference docs. Retain necessary current
  constraints or reproducible external evidence, not the chronology.
- A new test, cache, map or bug fix does not require a new guide entry.
  Add prose only if important information remains unavailable to the reader.
- An enforced rule may still need a short explanation for design decisions.
  Do not duplicate a test's case table, type definition or generated schema.

## Review a documentation change

Check affected code, tests and existing references before deciding what to
keep, correct, relocate or delete. Moving a large guide wholesale into a
document does not make its content useful. Remove duplication and obsolete
claims before relocation.

When code changes, search its symbols and behavior descriptions in guides,
docs and nearby comments. Update or remove claims the change invalidates.
Check incoming links when moving or deleting a file or heading, including
references in code comments. Update the appropriate index in `docs/README.md`
or a topic index. New files need a discoverable route with a clear reading
condition.

Retained guides use a relative `CLAUDE.md -> AGENTS.md` symlink. Remove the
symlink if the guide is removed, and repair incoming references. Do not
maintain separate instruction copies for different agents.

Before finishing, verify affected links and symlinks. For changed guides,
check their ancestors for repeated instructions. Confirm that each removed
critical constraint still has a clear owner. Review tone and usefulness manually;
word counts and phrase searches can locate problems but cannot judge quality.
