# Testing

The testing strategy now lives in the docs, rendered with diagrams:

- **[Testing Tiers](docs/md/internal/testing/README.md)** — the five-tier strategy (golden-snapshot
  API freeze → pattern units → corpus replay → fake-harness integration → live conformance + drift),
  the three contracts, and the version-independent invariants.
- **[Corpus](docs/md/internal/testing/corpus.md)** — the recorded byte-stream corpus and how to
  refresh it.
- **[Fake Harness](docs/md/internal/testing/fakeharness.md)** — the scriptable real-PTY fake that
  powers the timing-sensitive integration layer.

Run the hermetic suite:

```sh
make test   # go vet + gofmt + go test -race ./...
```
