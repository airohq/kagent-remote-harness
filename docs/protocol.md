# Protocol

The harness and the runner speak HTTP: one request per turn, with the
agent's events streamed back as newline-delimited JSON (NDJSON). Version 1
carries what a single coding agent conversation needs; the gaps at the end say
what kagent offers that it doesn't carry yet.

## Requests

**Every request carries `Authorization: Bearer <token>`.** The runner answers
`401` to any other token and compares tokens in constant time. On kagent, the
harness sends a placeholder and the EgressPolicy's gateway puts the real token
in, so the token never enters the sandbox.

| Request | Answers |
| --- | --- |
| `GET /v1/info` | The runner's protocol version, machine and harnesses |
| `POST /v1/turn` | Runs one turn and streams its events |

**Every answer from the runner has the header `Remote-Harness-Protocol: 1`.**
The harness tells the runner's answers from the transport's by it. An answer
without it, such as ngrok's "endpoint offline" page or a proxy's `502`, means
the runner is unreachable.

### `GET /v1/info`

```json
{"protocol": 1, "machine": "johndoe-mbp", "harnesses": ["claude"]}
```

`machine` names the runner's machine: `--machine`, or the host name. It ties
a conversation to its machine (see [Sessions](#sessions)).

### `POST /v1/turn`

The host selects the working directory when starting the supervisor. A turn
cannot request a host mount. Native coding-agent arguments are local worker settings. They apply across
turns, including when resuming a conversation.

```json
{
  "prompt": "Fix the failing test in pkg/foo.",
  "continuation_id": "5f1c…",
  "instructions": "You work in the repository in your working directory.",
  "model": "opus"
}
```

| Field | Meaning |
| --- | --- |
| `prompt` | The user's message |
| `continuation_id` | The coding agent's session to resume: a Claude Code session or a Codex thread. Empty starts a new one |
| `instructions` | The Agent's system prompt. Claude Code appends it to its own; Codex gets it at the start of a new thread |
| `model` | The model from the Agent's `modelConfig`, passed to the CLI (`--model`, `-m`). Empty uses the CLI's default |

The runner answers `200` with `Content-Type: application/x-ndjson` and one
event per line, or with an error status:

| Status | Meaning | The harness |
| --- | --- | --- |
| `200` | The turn runs; events follow | Streams the events |
| `400` | The request is malformed | Fails the turn |
| `401` | Wrong token | Fails the turn |
| `409` | The runner is busy with another turn | Retries until it waits too long |
| `413` | The request is over 1 MiB | Fails the turn |
| `429` | Too many requests with a wrong token this minute | Fails the turn |

## Events

**A turn's stream ends with exactly one `done` or `failure` event.** A stream
that ends without one failed, such as when the connection broke.

| `type` | Fields | kagent event |
| --- | --- | --- |
| `session` | `continuation_id`, `machine` | `SessionStarted` |
| `text` | `text` | `TextDelta` |
| `tool_call` | `id`, `name`, `arguments` | `ToolCall` |
| `tool_result` | `id`, `name`, `result`, `is_error` | `ToolResult` |
| `failure` | `message` | The turn fails with `message` |
| `done` | | The turn ends |

```json
{"type":"session","continuation_id":"5f1c…","machine":"johndoe-mbp"}
{"type":"text","text":"The test expects a sorted list."}
{"type":"tool_call","id":"t1","name":"Edit","arguments":{"file_path":"pkg/foo/foo.go"}}
{"type":"tool_result","id":"t1","name":"Edit","result":"ok"}
{"type":"done"}
```

**The runner flushes each event as it happens**, so kagent shows the agent's
progress live. It cuts a line from the coding agent over 1 MiB short.

**`text` events append to the agent's answer.** Claude Code's text streams as
it is written, a few words per event, and each piece arrives once. Codex's
text arrives one message per event.

**Every step the coding agent takes is a `tool_call` and its `tool_result`.**
For Codex: a command is `shell`, with `command`; a file change is
`file_change`; a web search is `web_search`, with `query`, reported when it
has finished; an MCP tool is `mcp__<server>__<tool>`, as Claude Code names
it, with the tool's arguments and its text result. A failed command or MCP
call has `is_error`. Codex's reasoning and to-do list are not events.

**A turn that runs longer than the runner's `--turn-timeout` ends with a
`failure`** that says so, after the runner stops the coding agent.

## Sessions

**The harness stores the session and its machine; the runner stores the
work.** After a turn, the harness keeps `continuation_id` and `machine` in
the sandbox's `/data`, which kagent keeps across suspends. The next turn
sends `continuation_id`, and the runner resumes it (`claude --resume`,
`codex exec resume`).

**A session belongs to the machine that started it.** The working copy and
the coding agent's transcript exist only there. When a turn's `session` event
names another machine, the harness fails the turn with a message instead of
letting the agent continue without its work. That happens when two runners
serve one URL, which isn't supported.

## Offline runners

**A turn waits for an unreachable runner, then fails.** The harness retries a
failed connection, an answer without the protocol header, and `409`, with
backoff, for `REMOTE_HARNESS_WAIT` (default `2m`). Then it fails the turn with
a message that the runner is offline. The conversation goes on when the
runner is back.

**Closing the request cancels the turn.** When kagent cancels a turn, the
harness closes its request, and the runner stops the coding agent.

## Gaps

**kagent's harness runtime offers more than version 1 carries.** Each gap,
and why it matters. Approvals come first, because they let a person confirm
what untrusted input asks for ([Untrusted input](security.md#untrusted-input)).

| Gap | Why it matters |
| --- | --- |
| **Next:** approvals and questions (kagent's `PendingTurn`: `ApprovalRequest`, `AskUserRequest`) | You approve a command in kagent instead of the runner refusing it. Needs a pending event and a resume request |
| Harness choice (`harness` in the turn; `harnesses` in `/v1/info` already) | One runner serves Claude Code and Codex agents at once; the runner refuses a harness it doesn't have. Now a runner runs the one `--harness` it was started with |
| Machine affinity (a routing hint the transport understands) | A pool of machines behind one URL, each conversation sent to its own machine |
| Concurrent sessions (a working copy per session) | Several conversations on one machine without sharing files |
| Usage (kagent's `ModelCall`: tokens, model, stop reason) | kagent shows the turn's cost and model |
| Tool status and nesting (`ToolStatus`, `ParentToolID`) | kagent shows declined and unfinished tools, and a subagent's tools under it |
| Trace context (`traceparent` from the turn's `TraceOrigin`) | The runner's spans join kagent's trace |

## Local supervisor integration

The first valid container turn pulls the published default worker tag and
resolves its image ID once per supervisor lifetime. Other image references
are resolved by the local engine when creating the container.

A Go client can embed `runner/supervisor.New`, serve its authenticated HTTP
handler on its transport, and call `Close` to cancel active turns and release
the backend. Both execution modes use the same v1 protocol.

Supervisor verbosity reports credential names and event types. Only fixed
descriptions of recognized failures appear in verbose logs.

A transport can also launch `remote-harness supervise --socket PATH --name NAME`
with a freshly generated `REMOTE_HARNESS_TOKEN`. An explicit `--sandbox TYPE`
selects the execution environment. The helper also accepts the original
`--runner TYPE` spelling. Omission uses the helper's default (`container`). Use a socket in a private
directory. The helper serves the same HTTP v1 protocol there. A successful
authenticated info request confirms readiness and leaves the worker cold.
Setting `REMOTE_HARNESS_PARENT_PIPE=1` asks the supervisor to stop when its
parent closes standard input.

Inside the worker container, the engine attachment carries one JSON
`TurnRequest` per line on standard input and the v1 events on standard
output. Only one turn is in flight. The supervisor supplies the machine
identity on the HTTP session event. EOF, timeout or cancellation releases
the worker. The supervisor never replays a turn after losing the stream.

An embedding application may provide a local Codex or Claude Code login volume independently of a worker’s workspace. Selection, rotation and retention of the login happen on the container engine. The turn and tunnel protocols carry no provider credentials during sign-in.
