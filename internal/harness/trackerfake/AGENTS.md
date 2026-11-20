# Fake trackers

`trackerfake` is a stateful fake of Jira Cloud (platform REST and the
Agile and Software APIs), Tempo and Linear. One `Engine` holds every
tenant; `Handler` serves one origin; `Serve` puts the origins on
127.0.0.1 listeners with `http.Server`s it owns. It imports no tracker
client and links no `testing` package
(`linearschema`'s `TestNoTestingInNonTestCode` checks), so the harness
process can run it. The
client contract it answers is
[tracker clients](../../../docs/architecture/tracker-clients.md).

## Files

| Concern | Files |
|---|---|
| Engine, seed, reset, clock, IDs | `engine.go`, `fixture.go` |
| Request pipeline: offline, credential, route, fault, handler, log | `serve.go`, `faults.go`, `log.go`, `auth.go` |
| Shared REST route matching | `routes.go` |
| Jira routes and handlers | `jira_routes.go` (the whole table), `jira*.go`; JQL in `jql.go` |
| Tempo | `tempo.go`, `tempo_worklogs.go`, `tempo_fixture.go` |
| Linear GraphQL executor | `linear.go`, `linear_exec.go`, `linear_conn.go` |
| Linear objects, reads and writes | `linear_objects.go`, `linear_issue*.go`, `linear_roots.go`, `linear_writes.go`, `linear_records.go` |

## Answer rules

- Behavior follows the vendor's documentation or a measurement. A
  comment beside the code says which (`documented`, `measured`); one the
  fake had to choose says `unverified`. Never shape an answer to make a
  client test pass: when a client and the fake disagree, find which one
  the documentation or a measurement supports, and report the other.
- A request the fake does not implement is unhandled: 501 in the
  tracker's error shape, recorded with `Unhandled` and a `Detail` naming
  what is missing. That covers a route, a method, a query parameter not
  in the route's list, a JSON body member (bodies decode strictly), a JQL
  clause outside the grammar in `jql.go`, a Linear field, argument or
  filter key the executor does not read, and a Linear document or
  variables the schema refuses (`linearschema.DocumentProblems`,
  `VariableProblems`). Tests fail on any entry of `Engine.Unhandled`.
- A well-formed request for something the fixture lacks answers the
  tracker's own not-found or refusal and changes nothing. A write
  applies all or nothing.
- Every write is stamped with the engine clock (`Options.Now`, never
  moving back) and records what the tracker records: Jira changelog
  items and `updated`, Tempo `updatedAt`, Linear `updatedAt`, history and
  state timestamps.
- Jira changelog shapes follow the measured ones in
  [tracker API measurements](../../../docs/references/tracker-api-measurements.md#jira-cloud)
  (`jira_changes.go`, `jira_records.go`, `jira_moves.go`; tested in
  `jira_changes_test.go`). Link and parent items carry no `fieldId`.
- Linear history: changes within `linearFold` of an issue's creation fold
  into the creation and add no entry (measured: folded at 2m30s,
  recorded at 3m).
  Later changes add one entry per update with every changed value.
- No answer repeats a credential: the log drops `Authorization` and
  replaces every known or presented credential with `<redacted>`, and an
  unhandled answer's reason is scrubbed the same way. Credentials are at
  least 8 characters so the replacement never hits ordinary text.

## Tenants and faults

- Jira tenants are sites, one origin each. Tempo and Linear are one
  origin each; the bearer token or API key selects the tenant, so seeding
  refuses two tenants sharing one. `SetToken` rotates a credential.
- A fault matches by origin and route name (`Request.Route`: `"GET
  rest/api/3/issue/{issue}"`, `"GET 4/worklogs/{id}"`, or a Linear
  operation name). Faults apply after the credential and route checks.
  `Reset` and `Close` drop held requests with nothing applied.
- `Servers` owns one serve goroutine per origin and counts the requests
  being answered; `Servers.Close` waits for both. A held request waits on
  its hold, its client's context or `Reset`/`Close`.

## Adding a route

1. Jira or Tempo: one row in the route table (`jira_routes.go` or
   `tempo.go`) with its method, pattern and accepted query parameters,
   and one handler. Linear: the executor serves any schema-valid
   operation; add the fields, arguments or mutation the operation reads
   to the objects in `linear_*.go`.
2. Note the evidence for each behavior beside it.
3. Drive the client method through the conformance test in its package
   (`internal/tracker/{jira,tempo,linear}/fake_*_test.go`), whose
   reflection walk fails for a Client method with no case. Linear's also
   fails for a registered operation that never ran.
4. When a provider reads it, extend the provider scenario
   (`internal/tracker/{jira,linear}provider/fake_scenario_test.go`).

Run `make go-test GO_TEST_PKGS='./internal/harness/trackerfake
./internal/tracker/jira ./internal/tracker/tempo ./internal/tracker/linear
./internal/tracker/jiraprovider ./internal/tracker/linearprovider'
GO_TEST_FLAGS='-race'` after a change here.
