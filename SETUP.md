# Setup

This project is managed by the `harness` CLI. If `harness` is not installed:

```sh
go install github.com/olesho/harness/cmd/harness@$(cat .harness-version 2>/dev/null || echo latest)
```

Then, from the repo root:

```sh
harness bootstrap   # install project dependencies + git hooks
harness verify      # confirm the working tree matches harness.lock.json
```

To create a **new** project from scratch, run `harness setup` in an empty
directory (the interactive `/harness-setup` agent skill can drive it), or pipe a
config:

```sh
harness setup --print-config-template > setup.json   # edit it, then:
harness setup --config setup.json
```
