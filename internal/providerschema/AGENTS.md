# internal/providerschema/

The strict-mode rules both provider CLIs enforce on a structured-output
schema, in one place. Claude Code validates `--json-schema` at spawn and
the Codex app-server validates `outputSchema` per turn; either rejection
fails the whole phase before any work happens, so a schema that breaks a
rule here is a hard outage, not a degraded run.

Stdlib-only and provider-agnostic: it reads JSON, not `def.JSONSchema`,
so schema *generation* (`internal/workflow/def`) and schema *consumption*
(the mock provider) can both depend on it without depending on each other.

## Surface

| Symbol | Purpose |
|---|---|
| `Validate(schema []byte) []Violation` | Every rule the schema breaks. Empty result means both CLIs accept it. |
| `ValidateClaude(schema []byte) []Violation` | Only the rules Claude's `--json-schema` validator enforces. For a schema that is Claude's ALONE, see below. |
| `Violation` | `Path` + `Message`; implements `error`. |
| `Draft07` | The only `$schema` value verified to pass both CLIs. |

## `ValidateClaude` is the narrow door, and when it is legitimate

`Validate` is the union. Two of its rules (`additionalProperties: false`,
a `required` naming every property) are **codex-400 rejections only**;
Claude accepts an open object and a partial `required`, and this package
has always recorded that tolerance.

`ValidateClaude` drops exactly those two and keeps the rest (the `$schema`
draft, the strict-mode vocabulary, a declared `type` on every node). It
exists because two schemas in this repo are Claude's alone and can never be
re-targeted: `internal/commitmsg` and `internal/threadtitle` each keep a
separate Claude constant beside their Codex one, and the Claude
commit-message schema deliberately requires only `subject` so an empty body
stays legal. Judging those against the union reports a working invocation
as broken.

Anything sent to BOTH providers, every generated workflow envelope schema and
the workflow digest included, still goes through `Validate`. Reaching for
`ValidateClaude` to quiet a violation on a shared schema re-opens the exact
class of defect the union exists to catch.

## The rules, and the failure each one prevents

Verified against Claude Code 2.1.219 and codex-cli 0.145.0. Every rule is
an observed hard failure, not a precaution:

| Rule | Observed rejection |
|---|---|
| No draft 2020-12 `$schema` | claude: `--json-schema is not a valid JSON Schema: no schema with key or ref "https://json-schema.org/draft/2020-12/schema"` |
| Keywords limited to the shared vocabulary | claude: `strict mode: unknown keyword: "multiline"` |
| Every object sets `additionalProperties: false` | codex 400 `invalid_json_schema`: `'additionalProperties' is required to be supplied and to be false` |
| Every object's `required` lists every property | codex 400 `invalid_json_schema`: `'required' is required to be supplied and to be an array including every key in properties` |
| Every schema node declares `type` | Both reject an untyped node. |

The rule set is the **union** of both CLIs' demands, so passing here means
either provider accepts the schema. Claude tolerates open objects and
partial `required`, and Codex tolerates the 2020-12 URI. Neither tolerance is
relied on, because a phase can be re-targeted at the other provider at any
time.

Optionality has one representation: a property stays in `required` and its
type widens to `["T","null"]`. Strict mode has no other way to express it,
and `def.ValidateEnvelope` reads that null back as "absent".

## Extending

Add a rule only with a reproduction against a real CLI, and record the
verbatim rejection in the table above and in the package doc comment. Do
not add rules from documentation or inference. The two CLIs disagree in
both directions, and guessing tightens the union for no reason.

## Consumers

- `internal/workflow/def` (test-only) pins `EnvelopeSchema` output to the
  rules. The non-test package stays free of this dependency.
- `cmd/ao-mockprovider` rejects an illegal schema the way a real CLI
  does, in both Claude (`--json-schema`) and Codex (`outputSchema`) modes,
  and on the one-shot text-generation paths (`claude -p` uses
  `ValidateClaude`, `codex exec --output-schema` uses `Validate`).
  Without this the mock accepts anything and a workflow suite passes green
  while every real provider run fails at spawn, which is exactly how the
  original five schema defects survived a full green harness.
