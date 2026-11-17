## Summary
<!-- What does this PR do and why? Link the Jira ticket. -->

Resolves: KFLUXINFRA-XXXX

## Changes
<!-- Bullet list of what changed. Be specific about packages. -->

## Testing
<!-- How was this tested? What passed? There is no CI: state the local runs. -->
- [ ] `go build -o bin/lkmabt ./cmd/lkmabt` passes
- [ ] `gofmt -l ./cmd ./internal` prints nothing
- [ ] `go vet ./...` passes
- [ ] `go test ./...` passes
- [ ] Live run (if behavior changed): cluster `<entry name>` / kflux `<catalog name>`, providers `<github|gitlab|both>`

## Live-Run Evidence
<!-- Required when workflow, cluster, or provider behavior changes.
     Summarize the .lkmabt-runs/ report; artifacts are gitignored. -->

## Notes
<!-- Anything reviewers should know: trade-offs, follow-ups, risks. -->
