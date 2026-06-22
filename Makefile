.PHONY: help test build docs docs-serve check-versions rebake-corpus rebake-corpus-all schema-canary-gemini

# Six canonical scenarios per harness; the rebake-corpus-all loop
# iterates these. Kept in sync with test/scripts/<harness>/*.json.
SCENARIOS := short-reply long-markdown code-block interrupted-mid-reply tool-call multi-turn
HARNESSES := codex claude gemini

help:
	@echo "harness-wrapper make targets:"
	@echo ""
	@echo "  make test                  hermetic suite (go vet + gofmt + go test -race)"
	@echo "  make build                 go build ./..."
	@echo "  make docs                  build the docs site (docs/md/ -> docs/html/)"
	@echo "  make docs-serve            preview the docs site at http://localhost:4321"
	@echo "  make check-versions        offline upstream-version drift check (npm registry)"
	@echo "  make rebake-corpus HARNESS=<name> SCENARIO=<name>"
	@echo "                             re-record one scenario via screenbench-record --script"
	@echo "                               HARNESS in {codex, claude, gemini}"
	@echo "                               SCENARIO in {$(SCENARIOS)}"
	@echo "  make rebake-corpus-all     refresh entire corpus across all harnesses"
	@echo "                             (PAID for codex/claude API tokens; gemini free-tier)"
	@echo "  make schema-canary-gemini  re-record gemini short-reply then re-run the"
	@echo "                             gemini transcript reader's real-corpus smoke test"

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

# docs: regenerate the static documentation site from the canonical markdown
# under docs/md/ into docs/html/ (a gitignored build artifact). The generator is
# a nested Go module (docs/gen) so its deps don't touch the main module.
docs:
	cd docs/gen && go run . build

# docs-serve: preview the built site locally (override the port with PORT).
docs-serve:
	cd docs/gen && go run . serve

check-versions:
	@go run ./internal/cmd/upstream-version-sentry 2>/dev/null; \
	code=$$?; \
	case $$code in \
		0) echo "" ; echo "✓ all pins match latest" ;; \
		1) echo "" ; echo "⚠ drift detected — see docs/md/internal/versions-drift.md when ready" ;; \
		2) echo "" ; echo "✗ sentry could not query the npm registry" ; exit 2 ;; \
	esac

# rebake-corpus: re-record one scenario via screenbench-record --script.
# Costs real API tokens for codex/claude (Gemini uses local oauth).
#
# Resolves harness binary name (claude-code → claude) and dispatches.
# The harness name passed to --harness matches the directory under
# test/corpus/ and test/scripts/; the binary name is one of {codex,
# claude, gemini}.
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
		gemini)             harness_dir=gemini; corpus_dir=gemini;      binary_name=gemini ;; \
		*) echo "✗ unknown harness $(HARNESS); want codex | claude | gemini"; exit 2 ;; \
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
# harness. Spends real API dollars for codex/claude (gemini uses
# local oauth). After recording, re-runs the adapter test suite to
# surface any TUI-marker drift — failure means escalate to the
# upgrade playbook (docs/md/internal/versions-drift.md).
rebake-corpus-all:
	@echo "▶ rebake-corpus-all will run 18 live recordings (6 scenarios × 3 harnesses)."
	@echo "  codex + claude consume API tokens; estimated total ~\$$0.50."
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

# schema-canary-gemini: tightest single-harness drift check that
# exercises the real upstream binary. Records gemini's short-reply
# scenario, then re-runs the transcript reader's real-corpus smoke
# test against whatever new JSONL the live recording produced. A
# failure here means gemini's session schema shifted in a way the
# reader's two-shape decoder doesn't yet handle.
schema-canary-gemini:
	@$(MAKE) --no-print-directory rebake-corpus HARNESS=gemini SCENARIO=short-reply
	@echo ""
	@echo "▶ re-running gemini transcript real-corpus smoke against the fresh on-disk JSONL..."
	@go test -run TestReadAgainstRealCorpus -v ./pkg/transcript/gemini/...
