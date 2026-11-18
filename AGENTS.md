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
- `internal/cli/` — command execution, aggregate run directory, sequential cluster execution, run logs
- `internal/config/` — env-file and flag parsing
- `internal/clusterfile/` — clusters-file schema (`name`, `type`, `kflux`, `ibmcloud`, `ibmcloud-endpoint`, `kubeconfig`, `server`)
- `internal/clusterauth/` — authentication ladder (verify with `oc whoami` → ROKS: `ibmcloud` → else: `kflux cluster login` → `oc login`)
- `internal/auth/` — GitHub `gh` browser login, the GitLab PAT prompt, and the run-scoped credential store with guaranteed deletion
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

- **Branches:** create a dedicated branch from up-to-date `origin/main` before the first edit; never edit or commit on `main`, and never stack new work on the branch the checkout already has — check `git log --oneline origin/main..HEAD` first. Continuing an open PR you were asked to update is the exception (see `skills/developer-guide.md` and `skills/pr-workflow.md`)
- **Pre-push:** `go build`, `gofmt -l ./cmd ./internal`, `go vet ./...`, `go test ./...` — no CI exists, so these are the only gates (see `skills/pr-workflow.md`)
- **Commits:** `KFLUXINFRA-<number>: <description>`; trailers per team convention (see `skills/pr-workflow.md`)
- **Qodo:** onboarded — invoke Qodo skills (`qodo-review`, `qodo-get-rules`, etc.) before every commit.
- **Fixtures:** only `redhat-appstudio-qe/dr_test_mathwizz` and `konflux-qe/dr_test_mathwizz_gl`; never create, fork, rename, or delete repositories
- **Tests:** Ginkgo v2 + Gomega, TDD, colocated external `_test` packages (see `skills/testing-standards.md`)
- **Artifacts:** `.lkmabt-runs/` is gitignored local evidence; credential-like fields are redacted before writing

## Gotchas

- **Build output verification** reads TaskRun YAML artifacts and expects one `linux/amd64` and one `linux/arm64` node label per component; a missing label fails as pipeline misconfiguration, not harness error (see `skills/developer-guide.md`)
- **Cluster names are not kflux catalog names** — `name` is the report and `--cluster` identity, `kflux` is the catalog name; host `kflux-ocp-p01.7ayg.p1.openshiftapps.com` is catalog cluster `ocp-p01`, and a wrong value surfaces kflux's `unknown cluster` error
- **GitHub App installation is per cluster** — `preflight` reads the app slug from `konflux-info/konflux-public-info` and verifies that app is installed for the fixture organization. An interactive run prints the installation steps, opens the installation page, and waits until an organization administrator installs the app; failed checks are retried and progress is reported every 5 minutes, and when the preflight timeout expires the run asks whether to keep waiting. Declining, or a non-interactive run, fails with the installation URL. An app limited to selected repositories must include the fixture; a confirmed selection that omits it fails the wait immediately with the installation settings URL, while selections the token cannot list log a warning and continue. After an approved wait that outlives the preflight deadline, the remaining preflight steps continue on the run context
- **GitLab API auth uses Bearer** — the provider sends `Authorization: Bearer` for the configured or prompted personal access token; `PRIVATE-TOKEN` rejects OAuth tokens with 401
- **Provider credentials are per run** — `GITHUB_TOKEN` and `GITLAB_BOT_TOKEN` are used when set; otherwise GitHub runs `gh auth login --web` with an isolated `GH_CONFIG_DIR` and `--insecure-storage`, and GitLab opens a prefilled PAT page (1-day expiration, `api` and `write_repository`) and prompts for the token without echo. A configured `GITHUB_TOKEN` is validated for App-installation listing, fixture push access, and repository listing for selected App installations; classic/OAuth tokens may need `read:user` (`gh auth refresh -h github.com -s read:user`), but `gh` OAuth user tokens (`gho_`) skip the listing check because GitHub rejects them for that endpoint. An interactive run first reuses the existing `gh` login when it passes the same checks, then falls back to the `gh` browser login. Transient, rate-limit, and timeout check failures abort the run without replacing the token.  The ephemeral GitLab token is also written to the tenant SCM Secret for PaC; each cluster's finalization deletes that Secret, and the run-acquired token is revoked once after the last cluster with `DELETE /personal_access_tokens/self` (a configured `GITLAB_BOT_TOKEN` is never revoked). All credential files live under a `lkmabt-auth-*` temporary directory that is deleted when the last cluster finishes, including on failure; a deletion failure fails the run. Clusters run one at a time so credentials are acquired once and reused
- **GitLab SCM Secret is harness-owned** — fixture setup requires `get`/`create`/`update` on Secrets in the tenant namespace, plus `delete` for ephemeral GitLab tokens, using `SelfSubjectAccessReview` after the namespace exists; failures name the namespace and show an example role grant. A missing tenant namespace is requested through the OpenShift `ProjectRequest` API, which binds the requester as project admin; when that API is unavailable or forbidden, setup falls back to creating a bare Namespace. A ProjectRequest does not copy labels, so the run identity needs cluster-scoped `update` on `namespaces` to mark the project for cleanup; a denied update fails setup and names the namespace to delete manually. The labels are applied only when the namespace UID matches the project returned by the request; a namespace another actor created is refused. Fixtures manage only `gitlab-build-secret`, labeled `app.konflux-ci.org/managed-by=lkmabt` and `app.konflux.org/run-id=<run-id>`. Both labels mark harness ownership: a Secret carrying them may be updated and adopted by the current run, which refreshes the run ID. Deletion additionally requires the run ID to equal the current run. A same-named Secret missing either label is never updated or deleted; the run fails on that collision after inspecting its labels, so a legacy unowned Secret must be deleted once after access is granted. Finalization deletes only the run-owned Secret, with a UID precondition.
- **Build waits are never silent and fail fast** — PaC onboarding and build waits report at most once per minute, naming pending components and each component's PipelineRun, age, and Buildah TaskRun counts. A pending PipelineRun whose annotated PaC git-auth Secret is missing past two minutes fails with the Secret name and a concurrent-run hint; other Secret read errors warn once and keep waiting. A matched failed PipelineRun fails immediately.
- **One process per tenant namespace** — runs and `cleanup` take a non-blocking per-user flock keyed by API server and tenant namespace; a contended run fails before writing artifacts and names the holder. The lock does not cover other hosts/users or prefixed `<tenant>-*` namespaces.
- **PipelineRun overshoot** for one component/commit fails the run; it is a regression alarm for STONEINTG-1732
- **Every kubeconfig is verified** with `oc whoami` before cluster work; a readable file with an expired token is not authenticated. A rejected `ocp` kubeconfig is refreshed through kflux (default path) or `oc login`, and otherwise fails at `authentication`, never at `preflight`
- **ROKS login is opt-in** with `type: roks`; those entries never use kflux or `oc login`. `ibmcloud ks cluster config` merges into the kubeconfig named by `KUBECONFIG`, and the ladder verifies the file with `oc whoami` on every run (a rejected file stays readable), so a developer who never opened the cluster's web console fails at `authentication` with instructions instead of at `preflight`
- **Namespace cleanup** deletes only namespaces labeled `app.konflux-ci.org/managed-by=lkmabt` with the matching run ID; anything else is prompted and re-inspected. Deletion waits at most five minutes, re-strips finalizers that controllers re-add, and on timeout fails with the namespace name plus the namespace controller's remaining resources/finalizers instead of hanging.
- **A parent `go.work` that omits this module** breaks `./...` patterns; run commands with `GOWORK=off`

## Skills

- Before writing or reviewing Go code, read `skills/go-coding-standards.md`
- Before writing or reviewing tests, read `skills/testing-standards.md`
- Before opening a PR or working through review feedback, read `skills/pr-workflow.md`
- Only when `README.md` requires a documentation update, read `skills/update-readme.md`; it defines the user-facing structure and parallel-edit rules and is not needed otherwise
- When understanding the architecture, workflow phases, or artifact layout, read `skills/developer-guide.md`
