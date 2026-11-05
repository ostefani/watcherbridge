# Contributing to WatcherBridge

Thanks for your interest.

## What WatcherBridge is

WatcherBridge receives network-change events from SubnetLens Pro watch mode, groups related events per host, and turns each group into a short, structured briefing (what changed, why it matters, how urgent it is, one check to run) using a configurable AI model. It can sit beside an existing event consumer (tap mode) or forward events through itself (proxy mode), and it can publish briefings back as annotations. Original events are never modified, and briefings link back to the events they describe.

Design principles that contributions must preserve:

- **Local first.** Event history and identity mappings stay on the operator's machine. Only a small, redacted summary is ever sent to a model provider.
- **Redaction by default.** IP addresses, MAC addresses, hostnames, and subnets are replaced with local tokens before anything leaves the machine; a bundle that still contains a raw address is never sent.
- **One owner for urgency.** A deterministic local baseline sets the priority floor. The model may suggest a one-step raise (capped), never a silent override.
- **Fail soft.** If the model or a downstream consumer is unavailable, events are still stored and a deterministic fallback briefing is produced.

## Getting started

Requirements:

- Go 1.26 or newer, matching `go.mod`.
- A C compiler (Xcode Command Line Tools on macOS, gcc on Linux). The SQLite driver (`mattn/go-sqlite3`) uses cgo, so builds need `CGO_ENABLED=1` (the default when a C compiler is present).

Build from the repository root:

```sh
CGO_ENABLED=1 go build -o watcherbridge ./cmd/watcherbridge
```

The dashboard assets are embedded in the executable. The repository includes
built assets, so Node.js is needed only when changing the viewer. For UI work,
use the version pinned in `ui/.nvmrc`, run `make ui-check`, and include the
regenerated `internal/viewer/web/dist` files with the source changes. Rebuild
and restart the executable after updating those assets; refreshing the browser
alone does not update a running binary.

Run the Go checks from the repository root with `CGO_ENABLED=1` and a C
compiler available:

```sh
go test $(go list ./... | grep -v '/node_modules/')
go vet $(go list ./... | grep -v '/node_modules/')
```

For anything touching concurrency or lifecycle code, also run:

```sh
go test -race $(go list ./... | grep -v '/node_modules/')
```

The `grep -v` filters out Go code shipped inside `ui/node_modules`
(third-party JavaScript dependencies); it is never part of the build.
Formatting is checked over tracked files only:

```sh
test -z "$(gofmt -l $(git ls-files '*.go'))"
```

The application includes signed-report ingestion, retained reported context,
priority resolution, optional model/investigator explanations, a local review
dashboard, operator decisions, and status/API surfaces. Network discovery,
host matching, and live device identity remain the scanner's responsibilities.

For UI changes, also walk the applicable checks in
[the manual UI checklist](docs/manual-ui-results.md). Record the tested revision
and actual results; automated render checks do not establish visual or
interactive correctness. `make ui-drift` verifies that the embedded assets
match a rebuild once the intended generated changes are committed.

## Architecture in brief

- `internal/store` — embedded SQLite (WAL mode) behind a single-writer goroutine. It stores raw events with their original bytes, per-host groups with a durable lifecycle (open → closed → processing → done/failed), briefings, and the local pseudonym map. Schema changes are ordered migrations keyed by `PRAGMA user_version`; timestamps are UTC Unix nanoseconds stored as integers.
- `internal/forwarder` — a per-event gate that prevents duplicate in-flight forwards when the same event is delivered twice.
- `internal/server` — authenticated report/heartbeat admission and tap/proxy delivery boundaries.
- `internal/briefing` — deterministic priority rules, bounded model/investigator work, and fallback briefings. `internal/provider` and `internal/redactor` own provider adapters and redacted context handling.
- `internal/overview` and `internal/incidentsummary` — review overview computation and summaries over retained incident evidence.
- `internal/viewer` and `ui/` — local APIs, Server-Sent Events, and the Svelte dashboard. Reported facts, model explanations, and operator review state remain distinct.
- `cmd/watcherbridge` — process setup and the status CLI.

Keep files organized by responsibility inside a package. Create a new package only for a real boundary, as with `internal/forwarder`; don't split packages just to shorten files, and don't split one transaction across independently committed steps.

## Contribution standards

- **Go style.** Standard formatting (`gofmt`), idiomatic naming, doc comments on exported declarations, errors wrapped with context (`%w`). Accept `context.Context` first for blocking or I/O operations and propagate it.
- **Persistence.** Runtime writes go through the store's serialized writer — no side write paths. Multi-step invariants belong in one transaction. Never weaken a database constraint to work around application validation. Treat stored headers, payloads, and error text as sensitive.
- **Testing.** Test observable behavior and invariants, not only the happy path: duplicates and idempotency, failure and rollback, context cancellation, schema upgrades from prior versions, and retention boundaries are all in scope when they apply. Keep tests deterministic (controlled clocks, temp dirs). If a check can't run, say so — never report an unrun check as passing.
- **Scope.** Smallest coherent change that solves the problem. No new dependencies, interfaces, or features without a clear need. Call out any change to public behavior, data formats, or configuration.

## Commit messages

Use an imperative subject, optionally with a scope, plus a body for anything non-trivial:

```text
feat(store): durable group lifecycle

- Name the concrete mechanisms, files, schemas, or methods changed.
- Name the tests added or updated.
- Keep claims accurate to the diff — describe what the code does,
  not what it aspires to do.
```

## Pull requests (once public)

- Describe what changed and why, and how you verified it (commands run, results).
- Keep unrelated refactoring out of the change.
- Update documentation when behavior, configuration, or schemas change.
