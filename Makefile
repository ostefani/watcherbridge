# WatcherBridge make targets. Go targets use the module cache; UI
# targets run inside ui/ with the pinned Node toolchain (.nvmrc).
# `make ui-check` is the Step 22 gate: lint (a11y warnings fatal),
# committed-dist build, and the 200 KiB gzip budget.

.PHONY: ui ui-check ui-drift

ui:
	cd ui && npm ci --no-audit --no-fund && npm run build

ui-check:
	cd ui && npm ci --no-audit --no-fund && npm run lint && npm run build && npm run smoke && npm run size

# ui-drift rebuilds the bundle and fails when the committed output
# differs: internal/viewer/web/dist must always match ui/ sources.
# Both checks matter: diff catches stale files, status catches a dist
# that was never committed (untracked files are invisible to diff).
ui-drift:
	cd ui && npm ci --no-audit --no-fund && npm run build
	git diff --exit-code --stat -- internal/viewer/web/dist
	test -z "$$(git status --porcelain -- internal/viewer/web/dist)"
