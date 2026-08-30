# Functional flows

Functional flows drive a fresh harness instance through a fresh Playwright
browser. They use semantic locators and real Playwright input. They do not
attach to an existing browser, use CDP, execute page code, or capture pixels.

Run a JSON scenario from `e2e/`:

```sh
pnpm harness:flow --spec /absolute/path/to/flow.json --report /tmp/flow.json
```

The checked-in smoke document can be used to verify the entrypoint itself:

```sh
pnpm harness:flow --spec fixtures/functional-flow-cli-test.json
```

`--report` defaults to `functional-flow-report.json` in the current directory.
Use `pnpm harness:flow --help` for the complete command-line form. The
default run owns a new temporary data root and removes it after cleanup. If
`--data-dir` is supplied, it must name an empty directory. That directory is
caller-owned and is left in place for inspection after the run.
The command returns zero for a passing flow and one for a flow or setup
failure. It always writes a versioned report for failures after argument
parsing, including the owned harness identity and the last semantic
observations. Invalid arguments return two. `--headed` is available for local
inspection and still creates a new browser context.

The scenario document has `v: 1`, an `id`, and `actions`. It may also include
`assertions` and bounded `monitors`. Typed extension invocations are supported
by library and test callers that register extensions. The standalone
`pnpm harness:flow` entrypoint has no extension loader and rejects them.
Targets are
semantic objects using one of `testId`, `role`, `label`, `text`, or
`placeholder`. Unknown fields and unsupported versions fail before the
harness starts. See `e2e/src/functional-flow.ts` for the complete TypeScript
types and `e2e/tests/functional-flow.spec.ts` for executable examples.
