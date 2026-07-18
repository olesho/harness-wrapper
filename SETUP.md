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

## SonarQube scanning (optional)

Enabling the `sonar` feature (`harness edit <name> --confirm <name> --sonar on`)
seeds a `sonar-project.properties` and runs a SonarQube scan on `pre-push` and in
`harness ci`. SonarQube is a heavy, self-hosted service you install yourself; the
harness never provisions it. The scan reads:

- `SONAR_HOST_URL` — defaults to `http://localhost:9000`.
- `SONAR_TOKEN` — a **local** API credential (SonarQube UI → My Account →
  Security); nothing is sent off your machine. Falls back to
  `~/Work/infra/sonarqube/.env` if the env var is unset.

If Docker is missing, the server is unreachable, or no token is found, the scan
**soft-skips with a warning and passes** — a fresh machine without SonarQube is
never blocked. Run `harness doctor` to see whether a scan will run.

## Semgrep SAST (optional)

Enabling the `semgrep` feature (`harness edit <name> --confirm <name> --semgrep on`)
runs a Semgrep SAST scan (`--config auto`) on `pre-push` and in `harness ci`, via
the official `semgrep/semgrep` Docker image — nothing is added to your project's
dependencies. Like the sonar scan it **soft-skips with a warning and passes** when
Docker is not on `PATH`, so a machine without Docker is never blocked. Findings from
a scan that does run fail the gate. `harness doctor` reports Docker availability.
