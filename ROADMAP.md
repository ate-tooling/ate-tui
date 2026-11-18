# Roadmap

Milestone 1 (done): read-only browser, explain, logs, timeline, palette,
filter, faults, async cached views, `--demo`.

## Milestone 2 (done): Actions with safety

Read-only stays the default. Writes need `--write` or a per-context allowlist
in config, and every write asks for confirmation.

- Actor: suspend (`DurabilityMode` sync/async), resume, revert, delete.
- Worker: drain, mark available, mark unavailable.
- `space` marks rows, and actions apply to the marked rows or the current one.
  The confirm dialog lists the objects. Delete requires typing the name.
- The row shows its new state at once, then a quick re-poll confirms it.
  PermissionDenied and FailedPrecondition show as plain messages.
- `:journal` keeps a local log of actions and their results.

## Milestone 3 (done): Deeper diagnosis

- Logs follow the actor across worker migration (like `kubectl ate logs -f`),
  with search, a container/lifecycle filter and line wrap.
- `u` relations: actor ↔ template ↔ golden tag ↔ worker ↔ pool ↔ node.
- `:pulse`: actor states per atespace, worker slot use, pool readiness,
  recent crashes and stuck actors.
- Egress policy per actor (`GetActorEgressPolicy`), and global and atespace
  access policy bindings. ateapi has no can-i RPC, so ate-tui shows bindings
  and does not predict permissions.
- `E`: Kubernetes events for the hosting worker pod.

## Milestone 4 (done): Proxy

- `f` on an actor forwards a local port through atenet-router, like
  `kubectl ate proxy actor`. The router dial logic lives in substrate's
  internal `ateclient`, so it needs a local copy.
- `:pf` lists active forwards and stops them.

## Milestone 5 (done): Config and release

- Config file: refresh interval, default view, keybindings, skins, write
  allowlist per context.
- `:ctx` switches kube context and reconnects.
- Wide mode (`w`), OSC52 copy, mouse support.
- goreleaser, Homebrew tap, Nix flake.

## Later

- Create atespaces, templates, actors and tags from YAML in `$EDITOR`.
