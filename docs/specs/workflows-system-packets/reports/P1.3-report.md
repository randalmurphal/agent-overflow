# Packet P1.3 Report

Codex session id: `019f5478-bdb3-75c0-b1c4-4ee5139ae19a`

## Files touched

- `cmd/ao/main.go` — adds the thin `ao` binary entry point.
- `internal/aocli/doc.go` — documents the importable offline CLI package.
- `internal/aocli/run.go` — implements command routing, config-root discovery, workflow validation/listing, output formats, and exit semantics.
- `internal/aocli/run_test.go` — adds direct, table-driven CLI tests over temporary config trees.
- `internal/aocli/AGENTS.md` — records the new package boundary and conventions.
- `internal/aocli/CLAUDE.md` — symlinks the package guide for the alternate agent entry point.
- `internal/AGENTS.md` — adds the required package-layout row for `internal/aocli`.
- `PACKET-REPORT.md` — records packet scope, assumptions, command surface, and gate evidence.

## ASSUMPTIONS

- `--config-root` names the app config directory itself, so workflows live at `<override>/workflows/` and `<override>/projects/<slug>/workflows/`. Without the override, discovery mirrors the app's `os.UserConfigDir()` → `os.UserHomeDir()` fallback and appends `agent-overflow`.
- `internal/workflow/profile` is absent on this base. Validation therefore passes nil bindings to `def.Validate` and reports `unchecked` in human and JSON output, as the packet directs.
- An absent shared or project workflow directory represents an empty scope and is skipped. A present but unreadable or non-directory source is an operational error.
- Project slugs use the existing persisted slug shape: 1–64 lowercase ASCII letters/digits separated by single hyphens. The CLI validates that shape before joining the slug into a config path.
- For direct-path validation, `ResolvedWorkflow.Path` supplies the definition-directory context used by prompt validation. The assigned shared scope is provenance only and does not alter `def.Validate` behavior.
- Human list output uses quoted name/path fields so embedded whitespace cannot corrupt its one-workflow-per-line form.

## Implemented command and flag surface

Root routing:

```text
ao [--config-root <path>] workflow validate [options] <path>
ao [--config-root <path>] workflow validate [options] --id <id>
ao [--config-root <path>] workflow list [options]
```

`--config-root` is accepted either before `workflow` or on either leaf command. `--project <slug>` adds project scope for ID validation/listing; without it, only shared scope is resolved. `--json` selects typed JSON output.

Validation exit codes are `0` for valid, `1` for validation findings, and `2` for usage/operational errors. List returns `0` for success, including zero workflows, and `2` for usage/operational errors.

### `ao workflow validate --help`

```text
Usage: ao workflow validate [options] <path>
       ao workflow validate [options] --id <id>

Options:
  --config-root <path>  override the Agent Overflow config root
  --id <id>             resolve and validate a workflow by id
  --json                write the typed validation result as JSON
  --project <slug>      include workflows for the project slug
```

### `ao workflow list --help`

```text
Usage: ao workflow list [options]

Options:
  --config-root <path>  override the Agent Overflow config root
  --json                write the resolved workflow list as JSON
  --project <slug>      include workflows for the project slug
```

## M4 interface note (not built)

M4 should add a separate online client boundary that accepts endpoint/token credentials and optional run context, then owns loopback HTTP+WebSocket calls and scoped-token errors. The offline `internal/aocli` resolver/presentation package should not acquire transport knowledge. No client interface, token discovery, RPC, network code, or `ao run` plumbing was added in this packet.

## Gate outputs

### Baseline — `make go-build`

Exit code: `0`

Last output:

```text
(no output)
```

### Baseline — `make go-test`

Exit code: `0`

Last 20 lines:

```text
ok  	agent-overflow/internal/sysstat	0.004s
ok  	agent-overflow/internal/terminal	0.875s
ok  	agent-overflow/internal/testutil	0.044s
ok  	agent-overflow/internal/textgen	1.011s
ok  	agent-overflow/internal/threadmode	0.005s
ok  	agent-overflow/internal/threadtitle	0.005s
ok  	agent-overflow/internal/transport	0.754s
?   	agent-overflow/internal/transport/methodgen	[no test files]
ok  	agent-overflow/internal/triage	19.978s
ok  	agent-overflow/internal/uikeys	0.042s
ok  	agent-overflow/internal/uitrace	0.093s
ok  	agent-overflow/internal/uiwindow	0.038s
ok  	agent-overflow/internal/usagecost	0.005s
ok  	agent-overflow/internal/usermessage	0.005s
ok  	agent-overflow/internal/windowgeom	0.105s
ok  	agent-overflow/internal/workflow/def	0.062s
ok  	agent-overflow/internal/workspacefiles	0.235s
ok  	agent-overflow/internal/workspacepath	0.005s
ok  	agent-overflow/internal/wsldistro	0.090s
ok  	agent-overflow/internal/wsllauncher	0.117s
```

### Final — `make go-build`

Exit code: `0`

Last output:

```text
(no output)
```

### Final — `make go-test`

Exit code: `0`

Last 20 lines:

```text
ok  	agent-overflow/internal/sysstat	(cached)
ok  	agent-overflow/internal/terminal	(cached)
ok  	agent-overflow/internal/testutil	(cached)
ok  	agent-overflow/internal/textgen	(cached)
ok  	agent-overflow/internal/threadmode	(cached)
ok  	agent-overflow/internal/threadtitle	(cached)
ok  	agent-overflow/internal/transport	(cached)
?   	agent-overflow/internal/transport/methodgen	[no test files]
ok  	agent-overflow/internal/triage	(cached)
ok  	agent-overflow/internal/uikeys	(cached)
ok  	agent-overflow/internal/uitrace	(cached)
ok  	agent-overflow/internal/uiwindow	(cached)
ok  	agent-overflow/internal/usagecost	(cached)
ok  	agent-overflow/internal/usermessage	(cached)
ok  	agent-overflow/internal/windowgeom	(cached)
ok  	agent-overflow/internal/workflow/def	(cached)
ok  	agent-overflow/internal/workspacefiles	(cached)
ok  	agent-overflow/internal/workspacepath	(cached)
ok  	agent-overflow/internal/wsldistro	(cached)
ok  	agent-overflow/internal/wsllauncher	(cached)
```

## Additional verification

### `go test ./internal/aocli ./cmd/ao`

Exit code: `0`

```text
ok  	agent-overflow/internal/aocli	0.006s
?   	agent-overflow/cmd/ao	[no test files]
```

### `go vet ./internal/aocli ./cmd/ao`

Exit code: `0`

```text
(no output)
```

### `cd frontend && pnpm run check`

Exit code: `0`

```text
> agent-overflow-frontend@0.0.9 check /home/rmurphy/repos/ao-lanes/p13/frontend
> svelte-check --tsconfig ./tsconfig.json

1783829200514 START "/home/rmurphy/repos/ao-lanes/p13/frontend"
1783829200517 COMPLETED 1548 FILES 0 ERRORS 0 WARNINGS 0 FILES_WITH_PROBLEMS
```

### `cd frontend && pnpm run build`

Exit code: `0`

Last 20 lines:

```text
dist/assets/jsx-CeeDbO4M.js                                   177.85 kB │ gzip:  16.77 kB
dist/assets/typescript-Cb6HvS8q.js                            181.14 kB │ gzip:  16.29 kB
dist/assets/chunk-WYO6CB5R-Djp-upTk.js                        230.18 kB │ gzip:  36.12 kB
dist/assets/katex-CTs2aUt1.js                                 256.70 kB │ gzip:  76.92 kB
dist/assets/markdown-vendor-B6FJe0KM.js                       387.18 kB │ gzip: 121.39 kB
dist/assets/cytoscape.esm-BkZ_dLQL.js                         435.40 kB │ gzip: 137.92 kB
dist/assets/terminal-vendor-DBbG48j_.js                       465.92 kB │ gzip: 121.55 kB
dist/assets/cpp-DLTJnIL_.js                                   637.55 kB │ gzip:  50.49 kB
dist/assets/chunk-KEIR6QF5-CXN5l0_R.js                        662.57 kB │ gzip: 143.20 kB
dist/assets/index-CKVgym_q.js                               1,090.56 kB │ gzip: 319.93 kB

[plugin builtin:vite-reporter]
(!) Some chunks are larger than 500 kB after minification. Consider:
- Using dynamic import() to code-split the application
- Use build.rolldownOptions.output.codeSplitting to improve chunking: https://rolldown.rs/reference/OutputOptions.codeSplitting
- Adjust chunk size limit for this warning via build.chunkSizeWarningLimit.
✓ built in 2.80s
```

## Post-task review

Six read-only review lenses found two unique issues; both were fixed. The list path now uses `def.Resolve` as the sole winner-selection authority with an O(1) shared-ID annotation, and validation JSON normalizes empty findings to `[]` with exact-shape tests. No architectural or behavioral items remain for discussion.
