# ate-tui

A terminal UI for [Agent Substrate](https://ate.dev), in the style of
[sofka](https://sofka.rs). It browses atespaces, actors, actor templates,
tags, workers and WorkerPools live, and explains why an actor or worker is
unhealthy.

```
brew install ate-tooling/tap/ate-tui
# or
go install github.com/ate-tooling/ate-tui/cmd/ate-tui@latest
ate-tui                 # current kubeconfig context, all atespaces
ate-tui -a research     # start in one atespace
ate-tui workers         # start on a view
ate-tui --demo          # sample data, no cluster needed
```

## Connecting

ate-tui connects the way `kubectl ate` does:

- It port-forwards to the `api` Service in `ate-system`, or dials `--endpoint`.
- It verifies ateapi's serving cert against the live `servicedns.podcert.ate.dev/identity`
  ClusterTrustBundle.
- It authenticates with a minted `ate-client` ServiceAccount token, or with `--token-file`.

`ATE_NAMESPACE`, `ATE_API_SERVICE_NAME` and `ATE_CLIENT_SERVICE_ACCOUNT`
override the defaults, as they do for `kubectl ate`. WorkerPools and worker pod
logs come from the Kubernetes API.

ate-tui is read-only. It never calls a mutating RPC.

## Keys

| Key | Action |
| --- | --- |
| `:` | command palette: `:actors [atespace]`, `:workers`, `:templates`, `:workerpools`, `:atespaces`, `:tags`, `:timeline`, `:q` |
| `/` | filter (space = AND, `!term` = NOT); `esc` clears |
| `ctrl-z` | faults only: crashed, stuck, draining, full, not ready |
| `tab` / `shift-tab` | cycle views |
| `[` / `]` | view history |
| `0` | all atespaces |
| `S` / `I` | cycle sort column / invert |
| `⏎` | actor: explain · worker: assignments · template: its actors · atespace: its actors · pool: its workers |
| `X` | explain (actors, workers, templates, pools) |
| `y` | yaml |
| `l` | follow actor logs from its worker pod (`f` toggles follow) |
| `T` | actor timeline of state changes seen this session |
| `o` / `t` / `p` | jump to worker / template / pool |
| `r` | refresh now, or reconnect |
| `?` | help |

## Explain

`X` diagnoses from ateapi data alone:

- **Actors:** crash message and time, and transitions stuck for over 2 minutes.
  It also shows snapshot generations and whether one is durable, the assigned
  worker's state, and whether any worker matching the selector has a free slot.
  Template golden snapshot errors appear here too.
- **Workers:** state, slot and resource use, epoch lag, and assignments.
- **Templates:** golden snapshot status and the actor states built from the template.
- **WorkerPools:** ready replicas, sandbox class resolution, and how many workers registered.

## Design

Every network call runs in a Bubble Tea command, so keys never wait on I/O.
Lists are cached per view. A revisit renders the cached rows at once while a
background refresh runs, and a poll (`--refresh`, default 2s) keeps the view
live. ateapi has no watch RPC, so polling is the only option. Rows are
pre-rendered off the UI goroutine. Filtering and sorting run in memory, and
only visible rows are drawn. On 10k actors, a filter keystroke takes ~0.4ms
and a frame ~0.2ms (`go test ./internal/ui -bench .`).

```
cmd/ate-tui        flags, wiring
internal/ateconn   port-forward, trust bundle, token, paged List calls
internal/explain   pure diagnosis functions (tested)
internal/ui        model, views, keys, pager (yaml/explain/logs/timeline)
internal/demo      in-memory backend for --demo
```
