# jetsam

Finds the Prometheus metrics nothing reads, and proposes dropping them.

Prometheus tells you how many series each metric has. Your rules tell you
which metrics anything actually queries. jetsam joins the two and opens a
pull request against your scrape config for the difference.

Every recommendation names the queries it was checked against, so you can
redo the check by hand.

## Install

```
go install github.com/SaiPisey2/jetsam/cmd/jetsam@latest
```

## Use

```
jetsam init
jetsam scan
```

## License

Apache 2.0.
