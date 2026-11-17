# LKMABT

Local multi-architecture build validation harness for ROKS Konflux clusters.
Triggers builds against fixed test repositories, waits for PipelineRuns,
verifies `linux/amd64` and `linux/arm64` pre-publication build outputs, and
collects runtime evidence.

## Quick Commands

| Action | Command |
|--------|---------|
| Build | `go build -o bin/lkmabt ./cmd/lkmabt` |
| Run all tests | `go test ./...` |
| Test one package | `go test ./internal/cluster` |
| Run one suite | `go test ./internal/workflow -run TestWorkflow` |
| GitHub run | `./bin/lkmabt run github --env-file .lkmabt.env --clusters-file .lkmabt-clusters.yaml` |
| GitLab run | `./bin/lkmabt run gitlab --env-file .lkmabt.env` |
| Both providers | `./bin/lkmabt run both --env-file .lkmabt.env` |
| Clean up a recorded run | `./bin/lkmabt cleanup --cluster <name> --run-id <run-id> --env-file .lkmabt.env` |

Run from the repository root: the env file, clusters file, and `.lkmabt-runs/`
resolve against the working directory.

## Project Layout

- `cmd/lkmabt/` — CLI entry point, argument parsing, signal handling
- `internal/cli/` — command execution, aggregate run directory, three-cluster concurrency cap, run logs
- `internal/config/` — env-file and flag parsing
- `internal/clusterfile/` — clusters-file schema (`name`, `type`, `kflux`, `ibmcloud`, `ibmcloud-endpoint`, `kubeconfig`, `server`)
- `internal/clusterauth/` — authentication ladder (kubeconfig → ROKS: `ibmcloud` → else: `kflux cluster login` → `oc login`)
- `internal/workflow/` — orchestration and phase state machine
- `internal/cluster/` — client-go wrapper, preflight checks, build-output verification
- `internal/providers/`, `internal/providers/github/`, `internal/providers/gitlab/` — provider abstraction and implementations
- `internal/collector/` — failure artifact collection
- `internal/evidence/` — manifests, status files, redaction
- `internal/cleanup/` — namespace ownership and deletion
- `internal/report/` — Markdown and HTML reports
- `internal/model/` — domain types and validated phase transitions
- `skills/` — deep-dive reference files for agents

## Key Conventions

- **Branches:** create a dedicated branch before the first edit; never edit or commit on `main` (see `skills/pr-workflow.md`)
- **Pre-push:** `go build`, `gofmt -l ./cmd ./internal`, `go vet ./...`, `go test ./...` — no CI exists, so these are the only gates (see `skills/pr-workflow.md`)
- **Commits:** `KFLUXINFRA-<number>: <description>`; trailers per team convention (see `skills/pr-workflow.md`)
- **Qodo:** not onboarded — do not invoke Qodo skills (`qodo-review`, `qodo-get-rules`, etc.)
- **Fixtures:** only `redhat-appstudio-qe/dr_test_mathwizz` and `konflux-qe/dr_test_mathwizz_gl`; never create, fork, rename, or delete repositories
- **Tests:** Ginkgo v2 + Gomega, TDD, colocated external `_test` packages (see `skills/testing-standards.md`)
- **Artifacts:** `.lkmabt-runs/` is gitignored local evidence; credential-like fields are redacted before writing

## Gotchas

- **Build output verification** reads TaskRun YAML artifacts and expects one `linux/amd64` and one `linux/arm64` node label per component; a missing label fails as pipeline misconfiguration, not harness error (see `skills/developer-guide.md`)
- **Cluster names are not kflux catalog names** — `name` is the report and `--cluster` identity, `kflux` is the catalog name; host `kflux-ocp-p01.7ayg.p1.openshiftapps.com` is catalog cluster `ocp-p01`, and a wrong value surfaces kflux's `unknown cluster` error
- **PipelineRun overshoot** for one component/commit fails the run; it is a regression alarm for STONEINTG-1732
- **ROKS login is opt-in** with `type: roks`; those entries never use kflux or `oc login`. `ibmcloud ks cluster config` merges into the kubeconfig named by `KUBECONFIG`, and the ladder verifies the file with `oc whoami` on every run (a rejected file stays readable), so a developer who never opened the cluster's web console fails at `authentication` with instructions instead of at `preflight`
- **Namespace cleanup** deletes only namespaces labeled `app.konflux-ci.org/managed-by=lkmabt` with the matching run ID; anything else is prompted and re-inspected
- **A parent `go.work` that omits this module** breaks `./...` patterns; run commands with `GOWORK=off`

## Skills

- Before writing or reviewing Go code, read `skills/go-coding-standards.md`
- Before writing or reviewing tests, read `skills/testing-standards.md`
- Before opening a PR or working through review feedback, read `skills/pr-workflow.md`
- When understanding the architecture, workflow phases, or artifact layout, read `skills/developer-guide.md`
