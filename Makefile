.PHONY: help test build docs docs-serve check-versions rebake-corpus rebake-corpus-all schema-canary-codex regen-conformance test-clients

# Six canonical scenarios per harness; the rebake-corpus-all loop
# iterates these. Deliberately NOT every script under test/scripts/: a
# version-shape recording that exists for one harness only (e.g.
# claude/settled-after-turn, which pins Claude Code 2.1.247's "· done <clock>"
# end-of-turn clause) is rebaked on demand via `make rebake-corpus`, not in the
# all-harnesses loop that would try to record it for codex too.
SCENARIOS := short-reply long-markdown code-block interrupted-mid-reply tool-call multi-turn
HARNESSES := codex claude

help:
	@echo "harness-wrapper make targets:"
	@echo ""
	@echo "  make test                  hermetic suite (go vet + gofmt + go test -race)"
	@echo "  make test-clients          TS + Python client suites (needs network + Node >=18.19)"
	@echo "  make build                 go build ./..."
	@echo "  make docs                  build the docs site (docs/md/ -> docs/html/)"
	@echo "  make docs-serve            preview the docs site at http://localhost:4321"
	@echo "  make check-versions        offline upstream-version drift check (npm registry)"
	@echo "  make rebake-corpus HARNESS=<name> SCENARIO=<name>"
	@echo "                             re-record one scenario via screenbench-record --script"
	@echo "                               HARNESS in {codex, claude}"
	@echo "                               SCENARIO in {$(SCENARIOS)}"
	@echo "  make rebake-corpus-all     refresh entire corpus across all harnesses"
	@echo "                             (PAID for codex/claude API tokens)"
	@echo "  make schema-canary-codex   re-record codex short-reply then re-run the"
	@echo "                             codex transcript reader's real-corpus smoke test"
	@echo "  make regen-conformance     regenerate the cross-language conformance corpus"
	@echo "                             (test/conformance/) — ordered: chatd then external"

test:
	go vet ./...
	@unformatted="$$(gofmt -l . | grep -v '^vendor/' || true)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed for:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	go test -race ./...

build:
	go build ./...

# test-clients: run the shipped SDK test suites. NOT hermetic — `npm install`
# needs network (there is no package-lock.json or vendored node_modules under
# clients/typescript/, and tsx is a devDependency), and `node --import` needs
# Node >=18.19. The typecheck step runs tsc -p tsconfig.test.json (noEmit) so
# the tests' compile-time assertions are checked without polluting dist/.
# The python suite must run from clients/python so `discover -t .` puts that
# directory (where harness_chat.py lives) on sys.path.
test-clients:
	cd clients/typescript && npm install && npm run typecheck && npm test
	cd clients/python && python3 -m unittest discover -s tests -t .

# docs: regenerate the static documentation site from the canonical markdown
# under docs/md/ into docs/html/ (a gitignored build artifact). The generator is
# a nested Go module (docs/gen) so its deps don't touch the main module.
docs:
	cd docs/gen && go run . build

# docs-serve: preview the built site locally (override the port with PORT).
docs-serve:
	cd docs/gen && go run . serve

# check-versions: the verdict line is printed by the PROGRAM (see its package
# comment), not decided here. It has to be: this used to `go run` the command
# and switch on $$? , but `go run` collapses any non-zero child status to 1, so
# the "registry unreachable" arm was unreachable code and every npm outage
# announced "drift detected".
#
# Build first, then run, so the documented exit codes (1 = drift, 2 = registry
# or read error) survive. Only 2 is propagated: drift is the expected state at
# every new upstream release and must not fail the target — callers tell drift
# from an outage by the verdict text, and a non-zero status here means "this
# check produced no signal".
#
# The catch-all arm is deliberate: an exit code this target does not recognise
# (a panic, a future code) must surface as a failure rather than fall into the
# "drift is fine" arm. Collapsing unknown into benign is the exact bug above.
# CHECK_VERSIONS_ARGS: extra flags for the sentry binary. Empty by default, so
# the operator-facing `make check-versions` is unchanged. It exists so the
# regression test in cmd/check-versions/makefile_exit_test.go can point THIS
# recipe at a dead registry -- the defect it guards (a collapsed exit status)
# lives in the recipe, not in Go code, and is untestable without a way in.
CHECK_VERSIONS_ARGS ?=

check-versions:
	@dir="$$(mktemp -d)"; \
	go build -o "$$dir/check-versions" ./cmd/check-versions || { rm -rf "$$dir"; exit 2; }; \
	"$$dir/check-versions" $(CHECK_VERSIONS_ARGS); code=$$?; \
	rm -rf "$$dir"; \
	case $$code in \
		0|1) exit 0 ;; \
		2) exit 2 ;; \
		*) echo "✗ check-versions failed unexpectedly (exit $$code)"; exit $$code ;; \
	esac

# rebake-corpus: re-record one scenario via screenbench-record --script.
# Costs real API tokens for codex/claude.
#
# Resolves harness binary name (claude-code → claude) and dispatches.
# The harness name passed to --harness matches the directory under
# test/corpus/ and test/scripts/; the binary name is one of {codex,
# claude}.
rebake-corpus:
ifndef HARNESS
	$(error HARNESS is required, e.g. HARNESS=codex)
endif
ifndef SCENARIO
	$(error SCENARIO is required, e.g. SCENARIO=short-reply)
endif
	@harness_dir=$(HARNESS); \
	binary_name=$(HARNESS); \
	corpus_dir=$(HARNESS); \
	case $(HARNESS) in \
		claude|claude-code) harness_dir=claude; corpus_dir=claude-code; binary_name=claude ;; \
		codex)              harness_dir=codex;  corpus_dir=codex;       binary_name=codex ;; \
		*) echo "✗ unknown harness $(HARNESS); want codex | claude"; exit 2 ;; \
	esac; \
	bin=$$(command -v $$binary_name 2>/dev/null); \
	if [ -z "$$bin" ]; then echo "✗ $$binary_name not found in PATH"; exit 2; fi; \
	script_path=test/scripts/$$harness_dir/$(SCENARIO).json; \
	out_dir=test/corpus/$$corpus_dir/$(SCENARIO); \
	if [ ! -f "$$script_path" ]; then echo "✗ missing script $$script_path"; exit 2; fi; \
	echo "→ recording $$harness_dir/$(SCENARIO) via $$bin"; \
	mkdir -p $$out_dir; \
	( cd $(CURDIR)/internal/screenbench && \
	  go run -tags screenbench ./cmd/screenbench-record \
	    --harness $$corpus_dir \
	    --bin "$$bin" \
	    --out "$(CURDIR)/$$out_dir" \
	    --auto-version \
	    --script "$(CURDIR)/$$script_path" \
	    --notes "rebake via Makefile on $$(date -u +%Y-%m-%dT%H:%M:%SZ)" )

# rebake-corpus-all: refresh every canonical scenario across every
# harness. Spends real API dollars for codex/claude. After recording,
# re-runs the adapter test suite to surface any TUI-marker drift —
# failure means escalate to the upgrade playbook
# (docs/md/internal/versions-drift.md).
rebake-corpus-all:
	@echo "▶ rebake-corpus-all will run 12 live recordings (6 scenarios × 2 harnesses)."
	@echo "  codex + claude consume API tokens; estimated total ~\$$0.33."
	@echo "  Ctrl-C now to abort; otherwise resuming in 5 seconds..."
	@sleep 5
	@failed=""; \
	for h in $(HARNESSES); do \
		for s in $(SCENARIOS); do \
			echo ""; echo "==== $$h / $$s ===="; \
			if ! $(MAKE) --no-print-directory rebake-corpus HARNESS=$$h SCENARIO=$$s; then \
				failed="$$failed $$h/$$s"; \
			fi; \
		done; \
	done; \
	echo ""; \
	if [ -n "$$failed" ]; then \
		echo "✗ failed scenarios:$$failed"; \
		echo "  Diagnose with the upgrade playbook (docs/md/internal/versions-drift.md)."; \
		exit 1; \
	fi; \
	echo "▶ all scenarios recorded; running adapter regression..."; \
	go test -race ./pkg/turns/harness/... ./internal/screenbench/scenario/ || { \
		echo "✗ adapter tests failed against fresh corpus — TUI marker likely shifted."; \
		echo "  See docs/md/internal/versions-drift.md."; \
		exit 1; \
	}; \
	echo "✓ corpus refreshed and adapter tests green."

# schema-canary-codex: tightest single-harness drift check that
# exercises the real upstream binary. Records codex's short-reply
# scenario, then re-runs the transcript reader's real-corpus smoke
# test against whatever new JSONL the live recording produced. A
# failure here means codex's session schema shifted in a way the
# reader's decoder doesn't yet handle.
schema-canary-codex:
	@$(MAKE) --no-print-directory rebake-corpus HARNESS=codex SCENARIO=short-reply
	@echo ""
	@echo "▶ re-running codex transcript real-corpus smoke against the fresh on-disk JSONL..."
	@go test -run TestReadAgainstRealCorpus -v ./pkg/transcript/codex/...

# regen-conformance: regenerate the cross-language conformance corpus under
# test/conformance/ (see its README). ORDERED two-step: the chatd-hosted test
# emits gateway/ (its DTOs are unexported package-main types), then the external
# test emits turnresult/ + cli/ and hashes the WHOLE corpus into MANIFEST.sha256.
# A plain `UPDATE_GOLDEN=1 go test ./...` does NOT guarantee this order and can
# write a manifest over stale gateway bytes — always use this target.
regen-conformance:
	UPDATE_GOLDEN=1 go test ./cmd/harness-chatd/
	UPDATE_GOLDEN=1 go test ./test/conformance/
	@echo "✓ conformance corpus regenerated; run 'go test ./...' to verify no diff."
