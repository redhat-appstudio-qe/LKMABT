## Summary
<!-- What does this PR do and why? Link the Jira ticket. -->

Resolves: KFLUXINFRA-XXXX

## Changes
<!-- Bullet list of what changed. Name the packages, not just the feature. -->

## Testing
<!-- There is no CI: state the local runs. Run from the repo root on Go 1.26+
     (see go.mod); prefix the commands with GOWORK=off when an unrelated parent
     go.work does not list this module. -->
- [ ] `go build -o bin/lkmabt ./cmd/lkmabt`
- [ ] `gofmt -l ./cmd ./internal` prints nothing
- [ ] `go vet ./...`
- [ ] `go test ./...` (add `-race` for concurrency changes)
- [ ] Live run for workflow, cluster, or provider changes (evidence below)

## Live-Run Evidence
<!-- Required when workflow, cluster, or provider behavior changes.
     Summarize the report; .lkmabt-runs/ is gitignored, so quote the
     relevant lines instead of committing artifacts. -->

- Invocation: the exact command, e.g. `lkmabt both arm64 --clusters-file <file>`
- Clusters: the clusters-file `name` entries and their kflux catalog names
- Result: aggregate result, plus each cluster/provider row with its stage and
  error on failure
- Architectures: the report's `Architectures` value
- Report: `.lkmabt-runs/latest/<aggregate-run-name>-report.md`

## Notes
<!-- Anything reviewers should know: trade-offs, follow-ups, risks. Call out
     changes to namespace deletion, artifact redaction, or provider writes. -->
