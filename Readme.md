# pgit

personal git server

## roadmap

* http api √
* embed ssh √
* mirror hook webhook

## Features

* Single-port HTTP+SSH multiplexing with protocol auto-detection
* Pure Go git wire protocol v0 (clone/push over HTTP smart-http + SSH), no `git` binary required
* Path mapping: decouple git access URLs from storage directories via aliases
* Built-in WebUI (embedded, exportable to disk for customization)
* Built-in markdown guide for agents (`GET /api/v1/guide.md`: interfaces, usage)
* Mirror repositories: scheduled/manual sync from remote HTTP/HTTPS repos with JSONL sync log
* Relay repositories: downstream pushes gated on the live upstream baseline, forwarded upstream asynchronously
* Mirror archive: standard git bundle snapshots taken *before* an upstream deletion/force-push is mirrored (fail-safe, restorable via API/CLI)
* Repository browsing API (tree/blob/archive/commits) in pure Go
* HTTP Basic Auth optional

## Building

> golang >= 1.26

```
go build ./cmd/pgit
```

## Running

```bash
# first make config file
pgit -d > config.json
# your own configure
vim config.json
# run
pgit -c config.json
```

架构与约束见 `AGENTS.md`，变更历史见 `CHANGELOG.md`。
