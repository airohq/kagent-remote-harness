# Security

A remote harness lets an agent in a cluster run commands on your machine.
This page says who can make it do that, what it reaches, and how to narrow
both.

## You secure the sandbox and the network

**A turn on your machine bypasses kagent's sandbox.** In kagent, an agent
runs in an isolated sandbox whose egress kagent controls. On your machine,
none of that applies: the coding agent runs with your machine's access and
reaches any network your machine reaches. **Securing both is your
responsibility.**

| What kagent secures in the cluster | What secures it on your machine | We recommend |
| --- | --- | --- |
| The sandbox the agent runs in | Where you run the runner | A hardened container; see [The execution boundary](#the-execution-boundary) |
| Who reaches the agent's runtime | How the harness reaches the runner | A tunnel, to a port published on `127.0.0.1` only; see [The runner's port](#the-runners-port) |

## Who can run commands on your machine

**Anyone who can make the agent take a turn, or who holds the runner's
token.**

| Who | How |
| --- | --- |
| People who can message the agent in kagent | Their message is the turn's prompt |
| Whatever triggers the agent, such as an issue or a webhook | Its text reaches the prompt, so instructions hidden in it reach the coding agent |
| Anyone with the runner's token and its URL | They send turns straight to the runner, past kagent |

**Limit who can message the agent**, with kagent's access control, and keep
the token as [Credentials](#credentials) describes.

## Untrusted input

**The coding agent acts on its prompt without asking you.** Text that anyone
can write, such as an issue or a pull request comment, can carry
instructions the coding agent follows. Choose the execution policy by who
can reach the prompt:

| The prompt comes from | Run the coding agent |
| --- | --- |
| Only you, or people you trust with your machine | Unattended, in a hardened container |
| People you trust, through text others can edit, such as an issue they label | With tools taken away (`--harness-arg=--disallowedTools=Bash`), or Codex with `--codex-sandbox read-only` |
| Anyone | In a disposable environment, such as a fresh VM or container per conversation, holding no credentials worth taking |

Approvals in kagent, so that you confirm an action before it runs, are the
next addition to the [protocol](protocol.md#gaps).

## The execution boundary

**A container is defense in depth, not the boundary for hostile input.** It
narrows what a turn reaches, but containers share your machine's kernel and,
by default, its network. For hostile input, use a VM.

**The `docker run` in the README hardens the runner's container:**

| Setting | Effect |
| --- | --- |
| The image's user, `10001` | The coding agent runs as a user without root, and the image has no `sudo` |
| `--cap-drop ALL`, `--security-opt no-new-privileges` | No Linux capabilities, and no way to gain them |
| `--read-only`, `--tmpfs /tmp` | A turn writes only to the `/work` volume, and to memory that a restart clears. The image's home directory is under `/tmp` |
| `--memory`, `--cpus`, `--pids-limit` | A turn can't take all your machine's memory, CPU or processes |
| `-p 127.0.0.1:8080:8080` | Only programs on your machine, such as ngrok, reach the runner |
| One volume, `/work` | Holds the working copy (`/work/workspace`) and the coding agent's sessions (`/work/state`). No home directory, SSH agent or Docker socket |

**Don't mount your home directory, your SSH agent or the Docker socket.**
Each gives a turn your full access again.

**The container still reaches your local network.** Docker's default
network lets a container reach your LAN, and on Docker Desktop the host
itself through `host.docker.internal`. To stop that, block private address
ranges for the container with firewall rules, or send its traffic through an
egress proxy that allows only the hosts the coding agent needs, such as the
model provider and your git host.

## The runner's port

**Publish the port on `127.0.0.1` only** (`-p 127.0.0.1:8080:8080`), and
let your tunnel, such as ngrok, reach it there. Nothing else on your network
needs to reach the runner.

**An ngrok URL is public.** The token is what keeps others out. The runner
answers at most 10 requests with a wrong token a minute; more get `429` until
the minute passes.

## Resource limits

**The runner limits each turn; the container limits the machine.**

| Limit | Where |
| --- | --- |
| Request size: 1 MiB per turn, `413` above | The runner |
| Turn time: `--turn-timeout`, default `30m` | The runner stops the coding agent and fails the turn |
| Processes left behind | The runner runs the coding agent in its own process group, and stops the whole group when a turn ends, is cancelled or times out |
| Concurrent turns: one, `409` for another | The runner |
| Event size: a line over 1 MiB from the coding agent is cut short | The runner |
| Memory, CPU, processes | The container (`--memory`, `--cpus`, `--pids-limit`) |
| Disk | The `/work` volume. Docker doesn't limit a volume's size, so watch it |

## Approvals

**Nothing asks you before a turn acts.** You choose what the coding agent
does without asking; the runner has no default:

| Harness | Flag | For example | What it does without asking |
| --- | --- | --- | --- |
| `claude` | `--claude-permission-mode` | `acceptEdits` | Edits files. Refuses other tools that need approval, unless your Claude Code settings allow them |
| `codex` | `--codex-sandbox` | `workspace-write` | Edits files and runs commands in the working directory |
| `codex` | `--codex-sandbox` | `read-only` | Reads files and runs read-only commands |

**What you allow in the coding agent's own settings, you allow for every turn
from kagent**, such as `Bash` in Claude Code's allowed tools.

## Credentials

**Every credential has a place, and none of them is on a command line.**

| Credential | Lives in | Rotate | Revoke |
| --- | --- | --- | --- |
| The runner's token | `.env`, readable only by you (`chmod 600`), and a Secret only the EgressPolicy reads | Put a new one in `.env`, recreate the Secret from it, restart the runner | Delete the Secret and stop the runner |
| The coding agent's login, such as `CLAUDE_CODE_OAUTH_TOKEN` | `.env`, and the container's environment | Create a new one, put it in `.env`, restart the runner | Revoke it at the provider |
| A git token, if the agent pushes | The container's environment | Create a new one and restart the runner | Revoke it at the git host; use a fine-grained token for only the repositories the agent needs |

- **The coding agent never gets the runner's token.** The runner removes it
  from the environment it passes on.
- **The runner logs no prompts or output** unless you pass `--log-content`.
  Its log has turn IDs, timing and results.
- **The coding agent's output isn't filtered.** Whatever it prints, including
  a credential it can read, streams to kagent and is stored there. Pass only
  credentials you accept the agent seeing.

## Transcripts and working copies

**Your machine keeps the work; kagent keeps the messages.**

| Data | Lives in | Deleted when |
| --- | --- | --- |
| Working copies | `/work/workspace` on the volume | You delete the volume (`docker volume rm remote-harness`) |
| The coding agent's sessions and transcripts | `/work/state` on the volume (the image sets `CLAUDE_CONFIG_DIR` and `CODEX_HOME` there) | You delete the volume. A later turn can't resume a deleted session |
| The conversation's messages | kagent | kagent's retention decides |
| The runner's log | Docker's log | Docker's log rotation decides. It holds content only with `--log-content` |

## The model provider's API

> [!NOTE]
> **A `modelConfig` on the Agent also opens the sandbox's egress to the
> model provider's API**, such as `api.anthropic.com`, because kagent opens
> it for every ModelConfig. Without an API key in the ModelConfig, kagent
> adds no credential, so the sandbox can reach the API but not use it. Leave
> the key out: the model runs on your machine with the coding agent's own
> login.
