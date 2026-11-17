---
name: pr-workflow
description: >
  Use when an agent is opening, monitoring, or iterating on a pull request in
  the LKMABT repository, including branch discipline, local verification,
  commit conventions, and review expectations.
---

# PR Workflow

Full lifecycle reference for pull requests in LKMABT
(`github.com/redhat-appstudio-qe/LKMABT`). Pull requests target `main`.

## Branch Discipline

Create a dedicated branch before the first file edit:

```bash
git switch -c <type>/<slug>   # e.g. fix/kflux-catalog-name
```

Never edit or commit on `main`, and never reuse another workstream's branch
without asking. Uncommitted work on a shared branch hides whose changes are
whose and blocks a clean PR diff.

## Pre-Push Verification

There is no CI in this repository. The repository has no `.github/workflows`,
no Makefile, and no CODEOWNERS; every check below is local and must pass before
pushing:

```bash
go build -o bin/lkmabt ./cmd/lkmabt
gofmt -l ./cmd ./internal     # must print nothing
go vet ./...
go test ./...
```

`go test ./...` runs the Ginkgo suites. For behavior that touches concurrency,
add `-race`.

If the checkout sits inside an unrelated parent `go.work`, every `./...` pattern
fails with "directory prefix . does not contain modules listed in go.work"; run
the commands with `GOWORK=off`.

## Commit Messages

Jira-tracked work carries the ticket ID:

```
KFLUXINFRA-1234: describe the change in the imperative mood
```

Body bullets state what changed and why. Trailers follow the team convention:
`Co-Authored-By:` with the agent's name and tool identifier for human-paired
sessions, `Authored-By:` when the agent authored the change autonomously.

## Pull Request Content

Use `.github/PULL_REQUEST_TEMPLATE.md`. Fill every section; do not leave
placeholders.

- **Summary**: what the PR does and why, linking the Jira ticket.
- **Testing**: which commands ran and what passed. Absence of CI makes this the
  only evidence reviewers have.
- **Live-run evidence**: when the change affects workflow behavior, the PR must
  state the cluster and provider matrix exercised (cluster entry name, kflux
  catalog name, `github`/`gitlab`/`both`) and summarize findings from the
  `.lkmabt-runs/` report. Run evidence is gitignored — quote the relevant lines
  instead of committing artifacts.
- **Risk**: anything that changes namespace deletion, artifact redaction, or
  provider write operations needs explicit reviewer attention.

## Review Requirements

- Anyone may review. The repository has no CODEOWNERS file and no committed
  branch-protection config, so ask which reviewer set is expected when it is
  unclear, and get at least one maintainer approval before merging.
- Reviewers run the harness locally when the change alters workflow or cluster
  interaction; state in the PR what they need (cluster, credentials, fixtures).
- Address review feedback by evaluating it technically, pushing back with
  reasoning when appropriate, then iterating until approved.

## Quality Tooling

**Qodo is not onboarded.** Do not invoke Qodo skills (`qodo-review`,
`qodo-get-rules`, etc.) for this repository, and do not expect automated Qodo
comments on PRs.

## After Merge

LKMABT is a local operator tool with no deployment pipeline. There is no
staging, soak, or production rollout step, and no image reference to bump. Keep
`main` releasable: if a merged change breaks the harness against a live cluster,
treat it as the next PR's first task.
