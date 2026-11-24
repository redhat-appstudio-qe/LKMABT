# LKMABT

Local multi-architecture build validation harness for ROKS Konflux clusters.
Triggers builds against fixed test repositories, waits for PipelineRuns,
verifies the selected architectures' pre-publication build outputs (all four of
`linux/amd64`, `linux/arm64`, `linux/s390x`, and `linux/ppc64le` by default),
and collects runtime evidence.

## Quick Commands

| Action | Command |
|--------|---------|
| Install | `./scripts/install.sh` |
| Build | `go build -o bin/lkmabt ./cmd/lkmabt` |
| Run all tests | `go test ./...` |
| Test one package | `go test ./internal/cluster` |
| Run one suite | `go test ./internal/workflow -run TestWorkflow` |
| GitHub run | `lkmabt github --clusters-file .lkmabt-clusters.yaml` |
| GitLab run | `lkmabt gitlab` |
| Both providers | `lkmabt both` |
| Clean up a recorded run | `lkmabt cleanup --cluster <name> --run-id <run-id>` |

Run from the repository root: the clusters file, `.lkmabt.env`, and
`.lkmabt-runs/` resolve against the working directory. `./scripts/install.sh`
puts `lkmabt` in a writable directory already on `PATH`.

## Project Layout

- `cmd/lkmabt/` — CLI entry point, argument parsing, signal handling
- `scripts/install.sh` — installs `lkmabt` into a writable `PATH` directory
- `internal/cli/` — command execution, aggregate run directory, sequential cluster execution, run logs
- `internal/config/` — env-file and flag parsing
- `internal/clusterfile/` — clusters-file schema (`name`, `type`, `ibmcloud`, `ibmcloud-endpoint`, `kubeconfig`, `server`); the kflux catalog name and default kubeconfig derive from `name`
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
- **Artifacts:** `.lkmabt-runs/` is gitignored local evidence; top-level reports and logs are prefixed with the aggregate run name, and credential-like fields are redacted before writing

## Gotchas

- **Architecture selection** — optional architecture arguments after `github`, `gitlab`, or `both` are one or more of `arm64`, `amd64`, `s390x`, `ppc64le`, `aws` (amd64+arm64), or `ibm` (s390x+ppc64le), separated by spaces or commas (`s390x arm64`, `s390x,arm64`); duplicates expand once in argument order, and omitting all means all four Linux platforms. Triggering rewrites each fixture PipelineRun's `build-platforms` parameter before the Dockerfile comment, and those configuration commits use `[skip ci]` so PaC does not create throwaway builds; preflight, verification, and evidence follow the selection (see `skills/developer-guide.md`)
- **Build output verification** reads TaskRun results and platform metadata and expects one successful output per selected architecture per component; a missing or unlabeled platform fails as pipeline misconfiguration, not harness error (see `skills/developer-guide.md`)
- **Build dependencies are probed per cluster** — before the fixture creates components, each selected component's declared public endpoints are probed once from a short-lived pod in the tenant namespace (`internal/cluster/egress.go`). The backends declare `proxy.golang.org` and `dl-cdn.alpinelinux.org`; the frontend declares `registry.npmjs.org`. The pod is pinned to the tenant workload nodes (`konflux-ci.dev/workload=konflux-tenants`) because private clusters restrict those nodes' egress while general nodes keep internet access; a probe on a general node would misreport reachability. An unreachable endpoint drops every selected component that needs it and prints the component selection alert (`not built:` / `reason:` / `testing:`); if every selected component drops, fixture setup fails with the reasons. Only the probe script's own failure exit code proves an unreachable endpoint: an evicted, OOM-killed, deadline-terminated, or never-started probe pod (the probe image is the frontend's, so its availability says nothing about a backend endpoint) is an unknown-outcome error, never a silent drop. The probe requires `create`, `get`, and `delete` on `pods` in the tenant namespace, checked with `SelfSubjectAccessReview` at preflight (see `skills/developer-guide.md`)
- **One process per state directory** — a run holds `.lkmabt-runs/.latest.lock` until it finishes; a second run against the same `--state-dir` fails immediately with an actionable message instead of writing into the live `latest/`, so wait for the first run or use another state directory
- **Cluster names are not kflux catalog names** — `name` is the report and `--cluster` identity; the kflux catalog name is `name` with a leading `kflux-` removed. When that is not a catalog name, authentication resolves a real host label (for example `stone-stage-p01` to `stg-p01`) from `kflux cluster list --output json`, and the derived catalog names the default kubeconfig. An unresolved name with no readable kubeconfig or server fails at authentication with guidance instead of using the ambient `oc login` context; catalog names absent from kflux's listing are excluded from the batch login, and a kflux batch login failure is retried per cluster so valid clusters still refresh. An interactive run that finds a FAS identity provider on the cluster's OAuth login page uses `oc login --web`; a non-interactive run needs a readable kubeconfig.
- **GitHub App installation is per cluster** — `preflight` reads the app slug from `konflux-info/konflux-public-info` and verifies that app is installed for the fixture organization. An interactive run prints the installation steps, opens the installation page, and waits until an organization administrator installs the app; failed checks are retried and progress is reported every 5 minutes, and when the preflight timeout expires the run asks whether to keep waiting. Declining, or a non-interactive run, fails with the installation URL. An app limited to selected repositories must include the fixture; a confirmed selection that omits it fails the wait immediately with the installation settings URL, while selections the token cannot list are skipped without user-facing output. After an approved wait that outlives the preflight deadline, the remaining preflight steps continue on the run context
- **GitLab API auth uses Bearer** — the provider sends `Authorization: Bearer` for the configured or prompted personal access token; `PRIVATE-TOKEN` rejects OAuth tokens with 401
- **Provider credentials are per run** — `GITHUB_TOKEN` and `GITLAB_BOT_TOKEN` are used when set; otherwise GitHub runs `gh auth login --web` with an isolated `GH_CONFIG_DIR` and `--insecure-storage`, and GitLab opens a prefilled PAT page (1-day expiration, `api` and `write_repository`) and prompts for the token without echo. A configured `GITHUB_TOKEN` is validated for App-installation listing, fixture push access, and repository listing for selected App installations; classic/OAuth tokens may need `read:user` (`gh auth refresh -h github.com -s read:user`), but `gh` OAuth user tokens (`gho_`) skip the listing check because GitHub rejects them for that endpoint. An interactive run first reuses the existing `gh` login when it passes the same checks, then falls back to the `gh` browser login. Transient, rate-limit, and timeout check failures abort the run without replacing the token.  The ephemeral GitLab token is also written to the tenant SCM Secret for PaC; each cluster's finalization deletes that Secret, and the run-acquired token is revoked once after the last cluster with `DELETE /personal_access_tokens/self` (a configured `GITLAB_BOT_TOKEN` is never revoked). All credential files live under a `lkmabt-auth-*` temporary directory that is deleted when the last cluster finishes, including on failure; a deletion failure fails the run. Clusters run one at a time so credentials are acquired once and reused
- **GitLab SCM Secret is harness-owned** — fixture setup requires `get`/`create`/`update` on Secrets in the tenant namespace, plus `delete` for ephemeral GitLab tokens, using `SelfSubjectAccessReview` after the namespace exists; failures name the namespace and show an example role grant. A missing tenant namespace is requested through the OpenShift `ProjectRequest` API, which binds the requester as project admin; when that API is unavailable or forbidden, setup falls back to creating a bare Namespace. A ProjectRequest does not copy labels, so the run identity needs cluster-scoped `update` on `namespaces` to mark the project for cleanup; a denied update fails setup and names the namespace to delete manually. The labels are applied only when the namespace UID matches the project returned by the request; a namespace another actor created is refused. Fixtures manage only `gitlab-build-secret`, labeled `app.konflux-ci.org/managed-by=lkmabt` and `app.konflux.org/run-id=<run-id>`. Both labels mark harness ownership: a Secret carrying them may be updated and adopted by the current run, which refreshes the run ID. Deletion additionally requires the run ID to equal the current run. A same-named Secret missing either label is never updated or deleted; the run fails on that collision after inspecting its labels, so a legacy unowned Secret must be deleted once after access is granted. Finalization deletes only the run-owned Secret, with a UID precondition.
- **Build waits are never silent and fail fast** — PaC onboarding and build waits report at most once per minute, naming pending components and each component's PipelineRun, age, and Buildah TaskRun counts. On a terminal those heartbeats update the active provider's live build line; non-interactive output prints one plain line per minute, and `run.log` always keeps every full line. A pending PipelineRun whose annotated PaC git-auth Secret is missing past two minutes fails with the Secret name and a concurrent-run hint; when PaC never added its finalizer, LKMABT retriggers that component once before failing. Other Secret read errors warn once and keep waiting. A failed Buildah TaskRun, or a PipelineRun that fails before all its Buildah TaskRuns complete, fails immediately; a PipelineRun that fails only in a post-build task is outside LKMABT's build-stage scope. Each component's build-output evidence is captured on the poll that observes its build success, from the PipelineRun and TaskRun objects already listed, so PaC `max-keep-runs` pruning cannot erase it; an observed PipelineRun that disappears before capture fails immediately with the component and last observed run instead of waiting for the build timeout.
- **Artifact collection retries transient failures** — required workload snapshots, including `workload/pods.json`, retry network timeouts and API 408/429/5xx responses twice with backoff before finalization fails; optional pod logs continue past individual container errors and report the missed streams in `collection-report.json`
- **One process per tenant namespace** — runs and `cleanup` take a non-blocking per-user flock keyed by API server and tenant namespace; a contended run fails before writing artifacts and names the holder. The lock does not cover other hosts/users or prefixed `<tenant>-*` namespaces.
- **PipelineRun overshoot** for one component/commit fails the run; it is a regression alarm for STONEINTG-1732
- **Every kubeconfig is verified** with `oc whoami` before cluster work; a readable file with an expired token is not authenticated. A rejected `ocp` kubeconfig is refreshed through kflux (default path) or `oc login`, and otherwise fails at `authentication`, never at `preflight`
- **ROKS login is opt-in** with `type: roks`; those entries never use kflux or `oc login`. `ibmcloud ks cluster config` merges into the kubeconfig named by `KUBECONFIG`, and the ladder verifies the file with `oc whoami` on every run (a rejected file stays readable), so a developer who never opened the cluster's web console fails at `authentication` with instructions instead of at `preflight`
- **Namespace cleanup** deletes only namespaces labeled `app.konflux-ci.org/managed-by=lkmabt` with the matching run ID; anything else is prompted and re-inspected. An aggregate run records each run-owned tenant namespace during finalization and asks for deletion after the last cluster, so prompts never interrupt the cluster sequence. Deletion waits at most five minutes, re-strips finalizers that controllers re-add, and on timeout fails with the namespace name plus the namespace controller's remaining resources/finalizers instead of hanging.
- **A parent `go.work` that omits this module** breaks `./...` patterns; run commands with `GOWORK=off`

## Skills

- Before writing or reviewing Go code, read `skills/go-coding-standards.md`
- Before writing or reviewing tests, read `skills/testing-standards.md`
- Before opening a PR or working through review feedback, read `skills/pr-workflow.md`
- Only when `README.md` requires a documentation update, read `skills/update-readme.md`; it defines the user-facing structure and parallel-edit rules and is not needed otherwise
- When understanding the architecture, workflow phases, or artifact layout, read `skills/developer-guide.md`
