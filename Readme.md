# pgit

personal git server

## roadmap

* plugin system
* http api √
* embed ssh √
* webUI √
* dashboard
* highlight style switch
* search
* mirror hook webhook


## Building

>  golang > 1.11 

```
go build
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
