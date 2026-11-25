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
| Engine, seed, reset, clock, IDs | `engine.go`, `fixture.go`, `jira_seed.go`, `jira_fixture.go`, `linear_seed.go`, `linear_fixture.go` |
| Request pipeline: offline, credential, route, fault, handler, log | `serve.go`, `txn.go`, `faults.go`, `log.go`, `scrub.go`, `auth.go` |
| Shared REST route matching | `routes.go` |
| Jira routes and handlers | `jira_routes.go` (the whole table), `jira*.go`; JQL in `jql.go` |
| Tempo | `tempo.go`, `tempo_worklogs.go`, `tempo_fixture.go` |
| Linear GraphQL executor, its pre-pass and filters | `linear.go`, `linear_exec.go`, `linear_check.go`, `linear_filter.go`, `linear_conn.go` |
| Linear tables: fields, comparators, sorts, roots, mutations, inputs | `linear_types.go`, `linear_objects.go`, `linear_issue*.go`, `linear_roots.go`, `linear_writes.go`, `linear_records.go` |
| Linear points per request, fitted to the measurements | `linear_cost.go` |

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
  clause outside the grammar in `jql.go`, a Linear document or variables
  the schema refuses (`linearschema.DocumentProblems`,
  `VariableProblems`), and any Linear field, argument, directive,
  fragment, filter key, comparator, sort or input member the executor's
  tables lack. The pre-pass (`checkOperation`) checks the last against
  the same tables the resolvers read, before anything runs, so the
  answer does not depend on what the fixture holds. It parses each
  filter once (`parseFilter`) into the conditions the connection
  evaluates; every comparator is defined per operand type in
  `operandKinds`, and an operand form it does not read (a relative
  date duration, for one) is unhandled. Tests fail on any entry of
  `Engine.Unhandled`.
- The Linear executor answers each response key once, its fields
  collected from the fragments spread (`collectFields`) with their
  sub-selections merged, and an error on a non-null field nulls the
  nearest parent that may be null, or the whole data.
- A panic in tracker code is the fake's defect: the pipeline recovers
  it, rolls back, answers unhandled naming it and keeps serving.
- A well-formed request for something the fixture lacks answers the
  tracker's own not-found or refusal and changes nothing. Every handler
  runs in a transaction the pipeline owns (`txn.go`): an unhandled
  answer, a refusal (400 or more), a panic and a Linear document whose
  mutation fails with an error change nothing. A route that does not set
  `reads` is a write and saves its whole tenant first, so a read costs
  more on a larger tenant unless it sets `reads`. A refused `Seed` puts
  back the ID counter and the clock as well as the tenants.
- A null Linear input member clears a field the schema lets be null and
  is refused for one the schema declares non-null (an issue's state,
  team, title, priority, sort orders, labels); an empty ID counts as
  null.
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
  Later changes add one entry per update with every changed value. An
  entry's nested entities (`fromAssignee`, `addedLabels`, ...) answer
  null, or leave a label out, for an ID the workspace lacks.
- Each paging style has one windowing helper over every record a read
  matched: Jira's `offsetWindow` (`offsetPage`, `listPage`) and
  `tokenWindow`, Tempo's `page`, Linear's `connection`.
- No log entry or unhandled answer repeats a credential the engine holds
  or the request presented: `logEntry` (`log.go`, `scrub.go`) is the only
  way an entry is made. Seeded credentials are at least 8 characters so
  the replacement does not hit ordinary text; a shorter value a request
  presents is replaced wherever it appears.

## Tenants and faults

- Jira tenants are sites, one origin each; a site's `Members` sign in
  with their own tokens and act as themselves. Tempo and Linear are one
  origin each; the bearer token or API key selects the tenant, so seeding
  refuses two tenants sharing one. `SetToken` rotates a credential.
- A fault matches by origin and route name (`Request.Route`: `"GET
  rest/api/3/issue/{issue}"`, `"GET 4/worklogs/{id}"`, or a Linear
  operation name). Faults apply after the credential and route checks.
- `Reset`, `Close` and a `Seed` that replaces a tenant end the world
  through `endWorld`: every held request ends with its connection
  closed and nothing applied, including one a release already let go,
  since a request carries the engine generation it arrived in and is
  dropped when that is gone. A hold still armed after a replacing
  `Seed` holds later requests.
- `Servers` owns one serve goroutine per origin and counts the requests
  being answered; `Servers.Close` waits for both. A held request waits on
  its hold, its client's context or `Reset`/`Close`.

## Adding a route

1. Jira or Tempo: one row in the route table (`jira_routes.go` or
   `tempo.go`) with its method, pattern and accepted query parameters,
   `reads: true` when it changes nothing (the method does not say:
   `POST search/jql` reads), and one handler. Linear: the executor
   serves any schema-valid operation; add a row for each field the
   operation reads to its type's table, a mutation to `mutations` and
   its input members to `linearInputs`. `TestLinearTablesMatchTheSchema`
   checks every row against the schema.
2. Note the evidence for each behavior beside it.
3. A write adds a refused request to `refusals` (`txn_test.go`), a route
   that sets `reads` or a Linear query root field an answered request to
   `readRows` (`reads_test.go`), a paged read a row to `pagedRows`
   (`paging_test.go`); each test's walk fails for a route or connection
   with none.
4. Drive the client method through the conformance test in its package
   (`internal/tracker/{jira,tempo,linear}/fake_*_test.go`), whose
   reflection walk fails for a Client method with no case. Linear's also
   fails for a registered operation that never ran.
5. When a provider reads it, extend the provider scenario
   (`internal/tracker/{jira,linear}provider/fake_scenario_test.go`).

Run `make go-test GO_TEST_PKGS='./internal/harness/trackerfake
./internal/tracker/jira ./internal/tracker/tempo ./internal/tracker/linear
./internal/tracker/linear/linearschema ./internal/tracker/jiraprovider
./internal/tracker/linearprovider' GO_TEST_FLAGS='-race'` after a change
here.
