# Security

## Reporting

Open a [security advisory](https://github.com/SaiPisey2/jetsam/security/advisories/new).
Please do not open a public issue for a vulnerability.

## What jetsam touches

- It reads a Prometheus HTTP API. Every request is a GET.
- With `-apply` it writes one file to one branch of one GitHub repository
  and opens a pull request. Without it, nothing leaves the machine.
- The GitHub token is read from `$GITHUB_TOKEN` only. It is never accepted
  as a flag, because a flag value is visible in `ps` output and shell
  history, and it is redacted out of any error text before that text is
  printed.

## What is treated as untrusted

Metric names, job labels and rule names come from whatever is being
scraped, not from the operator. They are rendered with visible escapes
before reaching a terminal or a pull request body, escaped with
`regexp.QuoteMeta` before becoming a relabel regex, and written as YAML
scalar nodes rather than interpolated into YAML text.

`propose` reads one file and, with `-apply`, replaces it wholesale. It
refuses the commit unless the remote file still matches the bytes the edit
was computed from.
