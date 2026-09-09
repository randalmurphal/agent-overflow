# Provider structured-output schemas

This package validates the strict JSON Schema subset accepted by provider CLIs.
It is provider-agnostic and operates on JSON bytes so schema generators and the
mock provider can share it.

`Validate` enforces the union required by both providers: draft-07, the
supported keyword vocabulary, a declared type on every node, closed objects,
and every object property listed in `required`. Express optional properties by
keeping them required and widening their type with `null`.

`ValidateClaude` omits only Codex's closed-object and complete-required rules.
Use it solely for schemas that can only reach Claude. Shared and workflow
schemas always use `Validate`.

Add a validation rule only after reproducing a hard rejection against the real
CLI and recording the contract in the package documentation and tests. The mock
provider must reject the same invalid schemas.
