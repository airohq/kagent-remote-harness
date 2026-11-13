# AGENTS.md

kagent-remote-harness runs a kagent agent's turns on a machine outside the
cluster, with a local coding agent. This file is for coding agents and
people changing this repository; keep it short.

## Layout

| Path | Holds |
| --- | --- |
| `wire` | The protocol between the harness and the runner. A change here changes `docs/protocol.md` |
| `runner` | The runner as an `http.Handler`: authentication, limits, and the Claude Code and Codex adapters |
| `internal/harness` | The kagent side: the bootstrap bundle and the relay to the runner |
| `cmd/remote-harness` | The runner's CLI, `remote-harness serve` |
| `cmd/harness` | The kagent BYO harness's entry point |
| `Dockerfile` | Both images: target `harness` and target `runner` |
| `docs/` | Security and the protocol. The README is the user guide |

## Rules

- **The runner must not depend on kagent or on a transport.** Programs embed
  `runner` behind their own transport.
- **A change in behavior updates its docs in the same change**: the README's
  commands and flags, `docs/security.md`, `docs/protocol.md`.
- **Security defaults stay explicit.** No default harness or permission mode;
  the coding agent never gets the runner's token; no content in logs without
  `--log-content`.
- **On a kagent bump, start the harness with config delivered the way the new
  kagent delivers it** (today, the bootstrap bundle), not only compile it.

## Checks

```sh
go vet ./...
go test -race ./...
```
