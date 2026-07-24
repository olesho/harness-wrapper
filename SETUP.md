# Setup

This project is managed by the `veracity` CLI. If `veracity` is not installed:

```sh
go install github.com/olesho/veracity/cmd/veracity@$(cat .veracity-version 2>/dev/null || echo latest)
```

Then, from the repo root:

```sh
veracity bootstrap   # install project dependencies + git hooks
veracity verify      # confirm the working tree matches veracity.lock.json
```

To create a **new** project from scratch, run `veracity setup` in an empty
directory (the interactive `/veracity-setup` agent skill can drive it), or pipe a
config:

```sh
veracity setup --print-config-template > setup.json   # edit it, then:
veracity setup --config setup.json
```

## SonarQube scanning (optional)

Enabling the `sonar` feature (`veracity edit <name> --confirm <name> --sonar on`)
seeds a `sonar-project.properties` and runs a SonarQube scan **locally only** — on
the `pre-push` git hook and on a local `veracity ci`. The server is self-hosted and
reachable only from your machine, so the scan is **skipped on remote CI runners**
(anything with `CI=true`, e.g. GitHub Actions). SonarQube is a heavy service you
install yourself; the veracity never provisions it. The scan reads:

- `SONAR_HOST_URL` — defaults to `http://localhost:9000`.
- `SONAR_TOKEN` — a **local** API credential (SonarQube UI → My Account →
  Security); nothing is sent off your machine. Falls back to
  `~/Work/infra/sonarqube/.env` if the env var is unset.

If Docker is missing, the server is unreachable, or no token is found, the scan
**soft-skips with a warning and passes** — a fresh machine without SonarQube is
never blocked. Run `veracity doctor` to see whether a scan will run.

## Semgrep SAST (optional)

Enabling the `semgrep` feature (`veracity edit <name> --confirm <name> --semgrep on`)
runs a Semgrep SAST scan (`--config auto`) on `pre-push` and in `veracity ci`, via
the official `semgrep/semgrep` Docker image — nothing is added to your project's
dependencies. Like the sonar scan it **soft-skips with a warning and passes** when
Docker is not on `PATH`, so a machine without Docker is never blocked. Findings from
a scan that does run fail the gate. `veracity doctor` reports Docker availability.
