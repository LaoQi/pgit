# pgit

personal git server

## roadmap

* http api √
* embed ssh √
* mirror hook webhook

## Building

> golang >= 1.21

```
go build ./cmd/pgit
```

## Runing

```bash
# first make config file
pgit -d > config.json
# your own configure
vim config.json
# run
pgit -c config.json

```

## Features

* Single-port HTTP+SSH multiplexing with protocol auto-detection
* Path mapping: decouple git access URLs from storage directories via aliases
* Pure API (no web frontend), HTTP Basic Auth optional
