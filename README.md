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
ate-tui --write         # allow actions (each still asks to confirm)
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

## Safety

ate-tui is read-only by default, and the header shows `[read-only]`. Writes
are enabled by `--write`, or for chosen kube contexts by
`~/.config/ate-tui/config.yaml` (`$XDG_CONFIG_HOME` is honored):

```yaml
writeContexts: [kind-dev, gke_my-project_us-central1_staging]
```

The header then shows `[write]`. `--demo` allows writes only with `--write`.

- Every action opens a confirm dialog that lists the objects. `y`/`⏎`
  accepts, `n`/`esc` cancels. Delete asks you to type the actor's name, or
  `yes` when several are marked; nothing else works while that dialog is open.
- `space` marks rows. Actions apply to the marked rows, or to the current row
  when none are marked. Marks clear after an action or a view change.
- Actions run in the background, in parallel. The row shows its new state at
  once and a quick re-poll confirms it. The status line reports failures per
  object, e.g. `suspended 3, failed 1: …`, with a hint for PermissionDenied
  and Unauthenticated.
- Each result is appended to `~/.local/state/ate-tui/journal.jsonl`
  (`$XDG_STATE_HOME` is honored). `:journal` lists this session's actions
  and earlier ones.

## Keys

| Key | Action |
| --- | --- |
| `:` | command palette: `:actors [atespace]`, `:workers`, `:templates`, `:workerpools`, `:atespaces`, `:tags`, `:pulse`, `:access`, `:timeline`, `:journal`, `:q` |
| `/` | filter (space = AND, `!term` = NOT); `esc` clears |
| `ctrl-z` | faults only: crashed, stuck, draining, full, not ready |
| `tab` / `shift-tab` | cycle views |
| `[` / `]` | view history |
| `0` | all atespaces |
| `S` / `I` | cycle sort column / invert |
| `⏎` | actor: explain · worker: assignments · template: its actors · atespace: its actors · pool: its workers |
| `X` | explain (actors, workers, templates, pools) |
| `y` | yaml |
| `l` | follow actor logs across worker moves (see [Logs](#logs)) |
| `u` | relations of any row; `⏎` jumps to one (see [Diagnosis](#diagnosis)) |
| `e` | actor: egress policy |
| `E` | actor or worker: Kubernetes events of the worker pod |
| `T` | actor timeline of state changes seen this session |
| `o` / `t` / `p` | jump to worker / template / pool |
| `r` | refresh now, or reconnect |
| `space` | mark / unmark the row |
| `s` | actor: suspend (`tab` in the dialog toggles async / sync durability; async is the default) |
| `R` | actor: resume |
| `v` | actor: revert to its durable snapshot (clears a crash) |
| `ctrl-d` | actor: delete (type the name to confirm) |
| `D` | worker: drain (one-way) |
| `A` / `U` | worker: mark available / unavailable |
| `?` | help |

## Logs

`l` follows an actor the way `kubectl ate logs -f` does. Each poll reads the
actor with `GetActor`:

- When it moves to another worker pod, the old stream is closed and one opens
  on the new pod, after a `── moved to <ns>/<pod> ──` line.
- While it is not running, the pager shows `waiting: actor is Suspended`, once
  per state, and reconnects when the actor runs again.
- A reconnect to the same pod resumes from the kubelet timestamp of the last
  line shown (`Timestamps` and `SinceTime`), so no line appears twice.

Inside the pager:

| Key | Action |
| --- | --- |
| `f` | toggle follow; scrolling up pauses it |
| `/` | search; `n` / `N` next / previous match; `esc` clears |
| `c` | source: all → containers → lifecycle |
| `w` | toggle line wrap |

The pager keeps the last 20,000 lines.
In `--demo`, the actor named `…-nomad` moves between two workers, with a
suspended pause in between, so you can watch this happen.

## Diagnosis

- **`u` relations:**
  - actor: its template, source tag, golden tag, worker, pool and node;
  - worker: its pool, node and assigned actors;
  - template: its golden tag and actors;
  - pool: its workers;
  - atespace: its actors, templates and tags.

  `⏎` opens the object's view with the cursor on it. A node opens the workers
  on that node.
- **`:pulse`:** a cluster overview that refreshes on each poll. It shows
  actor counts by state per atespace, worker slot, CPU and memory use per
  pool, WorkerPool readiness, recent crashes, actors stuck over 2 minutes,
  and template golden snapshot errors. It reuses the all-atespace actors,
  workers, pools and templates lists, fetched in parallel.
- **`e` egress:** the actor's `EgressPolicy` rules, with protocol, hosts,
  ports (proto defaults: http 80, https 443) and the names of injected
  credential headers. Credential URIs and prefixes are never shown. An actor
  with no policy has all egress denied, and its DNS lookups answer NXDOMAIN.
- **`:access`:** global and per-atespace `AccessPolicy` bindings (role →
  members). ateapi has no can-i RPC, so ate-tui shows the bindings and does
  not predict permissions. The server's bootstrap owners are global owners
  whether or not the global policy exists.
- **`E` events:** core/v1 events for the worker pod hosting the actor, or the
  selected worker's pod (field selector `involvedObject.name`), newest first.

## Explain

`X` diagnoses from ateapi data alone:

- **Actors:** crash message and time, and transitions stuck for over 2 minutes.
  It also shows snapshot generations and whether one is durable, the assigned
  worker's state, and whether any worker matching the selector has a free slot.
  Template golden snapshot errors appear here too, and so do Warning events
  from the last hour on the hosting worker pod.
- **Workers:** state, slot and resource use, epoch lag, assignments, and
  recent Warning events on the worker pod.
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
internal/ateconn   port-forward, trust bundle, token, paged List calls, writes
internal/config    config.yaml (write allowlist), XDG paths
internal/explain   pure diagnosis functions (tested)
internal/ui        model, views, keys, pager (yaml/explain/timeline/journal),
                   logs follow, relations, pulse, policies and events,
                   actions and the confirm dialog
internal/demo      in-memory backend for --demo
```
