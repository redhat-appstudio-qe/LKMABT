---
name: developer-guide
description: >
  Use when understanding the LKMABT codebase architecture, package layout,
  workflow phases, Kubernetes client usage, artifact layout, or design
  decisions before making changes.
---

# Developer Guide

Architecture reference for LKMABT (Local Multi-Architecture Build Testing Tool
for Konflux Clusters). Read this before changing behavior that spans more than
one package.

## Branch Discipline

Every session establishes its own base; never inherit one.

- Before the first edit, run `git status --short --branch` and
  `git log --oneline origin/main..HEAD`. A non-empty range means the checkout
  carries commits that are not on `main`.
- Continuing an open PR is the one valid reason to work on such a branch: its
  commits are the PR, and addressing review feedback belongs on it. Confirm the
  branch is that PR's head (its upstream branch is the PR head) before editing.
- Any other non-empty range is inherited work: stop and create a fresh branch
  from `origin/main` instead of stacking new work on it.
- Start new work with `git fetch origin` and
  `git switch -c <type>/<slug> origin/main`. A local topic branch — including
  one that looks related to the task — is not a valid base.
- Work already merged is finished. A squash merge shows the PR number in the
  subject on `origin/main` (`… (#14)`); such a branch must not be continued, and
  an unmerged local branch must not be assumed active. Ask before branching
  from another workstream's work.
- Never edit or commit on `main`.
- If the requested change depends on code that exists only on an unmerged
  branch, state the dependency and ask which base to use before editing.

Full branch, verification, and PR rules: `skills/pr-workflow.md`.

## What the Tool Does

LKMABT validates that the multi-platform-controller builds the selected
architectures on ROKS Konflux clusters. The default selection is every Linux
platform: `linux/amd64`, `linux/arm64`, `linux/s390x`, and `linux/ppc64le`.

1. Triggers builds against fixed test repositories (GitHub and GitLab)
2. Waits for successful PipelineRuns
3. Verifies pre-publication build outputs for every architecture each cluster runs
4. Collects runtime evidence and artifacts

The harness never creates, forks, or deletes repositories. It reuses fixed
fixtures, sets their PipelineRun `build-platforms` parameter to the selection,
and appends harmless comments to trigger PipelineRuns.

## Package Organization

**`cmd/lkmabt/`** — Entry point
- `main.go` parses the command line and dispatches through `cli.Execute`

**`internal/cli/`** — Command execution
- `Runner` interface defines the execution contract
- `Execute()` drives the full workflow lifecycle
- Owns the aggregate run directory, the sequential cluster runner, and the per-cluster/per-provider run logs
- `Console` serializes output from concurrent providers. On a terminal it draws one live line per active step, updates heartbeat detail in place via `Step.Update`, clips overlong live rows and prints the full detail as a durable line, and overwrites a completed step with a `✓`/`✗` line; on a pipe, file, or CI log it emits plain bullet/check lines without ANSI codes. `Suspend` pauses the live region while an interactive login owns the terminal, and prompts buffer progress until they finish
- `internal/workflow.Runner` reports each executed phase through `Options.OnPhaseStart` and `Options.OnPhaseEnd`; `phaseObserver` maps them to step descriptions and emojis, and `LiveStage.Progress` feeds PaC/build heartbeats into the active step while `LiveStage.Log` keeps the full lines

**`internal/clusterfile/`** — Clusters-file schema
- Strict parsing of `name`, `type`, `ibmcloud`, `ibmcloud-endpoint`, `kubeconfig`, and `server` per entry
- `name` is the report/`--cluster` identity; the kflux catalog name is that name with a leading `kflux-` removed, and authentication resolves a real host label through `kflux cluster list --output json` when the derived name is not itself a catalog entry
- `type: roks` defaults `ibmcloud` to `name` and `ibmcloud-endpoint` to `vpe`; IBM Cloud fields are rejected on `ocp` entries
- Resolves kubeconfig paths (`~` expansion, relative-to-file); the default is `~/.kube/config-<catalog>`, kflux's own per-cluster path

**`internal/clusterauth/`** — Authentication ladder
- Every non-ROKS kubeconfig is verified with `oc whoami` on every run: an expired token leaves the file readable, so readability proves nothing. A rejected default-path kubeconfig is refreshed through `kflux cluster login`; before login, `kflux cluster list --output json` resolves real host labels such as `stone-stage-p01` to catalog aliases such as `stg-p01`, and the alias names the default kubeconfig. If a batch kflux login fails, each cluster is retried individually so an unknown catalog cannot block valid ones. A custom-path kubeconfig or an unresolved cluster falls back to `oc login` only when the entry has a server or a readable kubeconfig; a missing kubeconfig with no server fails with guidance rather than using the ambient context. `type: roks` entries are verified and refreshed through the IBM Cloud CLI
- ROKS: `ibmcloud target` probes for an active session, `ibmcloud login --sso` runs when absent, `ibmcloud ks cluster config --cluster <ibmcloud> --endpoint <ibmcloud-endpoint>` writes the entry's kubeconfig through `KUBECONFIG`, and a second `oc whoami` verifies the result
- Non-interactive checks are bounded (`oc whoami` 60s, `ibmcloud target`/`ks cluster config` 2m) so one stalled credential plugin fails that cluster instead of blocking the run; only `ibmcloud login --sso` stays interactive

**`internal/auth/`** — Provider credential acquisition
- Creates one run-scoped `lkmabt-auth-*` directory with 0700 credentials written as 0600 files
- GitHub: runs `gh auth login --web` with an isolated `GH_CONFIG_DIR` and `--insecure-storage`
- GitLab: opens a prefilled PAT page (1-day expiration, `api` and `write_repository`), prompts for the token without echo, and deletes the ephemeral SCM Secret during finalization; the run-acquired token is revoked once after the last cluster
- `RunClusters` deletes the directory after the last cluster; non-interactive runs must provide the provider tokens

**`internal/report/`** — Aggregate reports
- Renders Markdown and HTML reports for a multi-cluster run

**`internal/config/`** — Configuration parsing
- Parses environment files and command-line arguments
- Validates cluster context; provider credentials are acquired at run time

**`internal/workflow/`** — Core orchestration logic
- `LiveStage` coordinates the full test workflow
- Phases: preflight → fixture-ready → triggered → builds-verified → build-outputs-verified → completed
- Each phase transition is validated (see `model.ValidateTransition`)

**`internal/cluster/`** — Kubernetes interactions
- `RealCluster` wraps k8s client-go operations
- Preflight checks validate cluster prerequisites (multi-platform-controller deployment, host-config entries). `CheckScheduling` classifies each requested platform; `RunPreflight` treats an undeclared platform as informational and a declared but incomplete platform as a failure, and the workflow narrows `Request.Platforms` to the supported subset
- `AdmissionInspector.QuotaMessage` reads the Kueue `QuotaReserved=False` condition for the Workload owned by a pending PipelineRun
- GitHub preflight reads the cluster's app slug from `konflux-info/konflux-public-info` and confirms that app is installed for the fixture organization; interactive runs open the installation page and wait until the installation appears
- Build output verification confirms node evidence (Buildah outputs) match expected architectures

**`internal/providers/`** — Git forge abstraction
- `Provider` interface abstracts GitHub and GitLab operations
- `github/` and `gitlab/` subpackages implement the interface
- Operations: validate fixtures, read Dockerfiles, update files to trigger builds

**`internal/model/`** — Domain types and phase state machine
- `Phase` enum defines workflow states
- `FixtureIdentity`, `PipelineRunIdentity`, `TriggerCommit` represent test run entities
- Phase transitions are strictly validated

**`internal/platform/`** — Architecture selection
- Maps one or more CLI architecture arguments (`arm64`, `amd64`, `s390x`, `ppc64le`, `aws`, `ibm`) to canonical platforms; repeated or overlapping tokens are deduplicated in argument order
- Owns canonical names, TaskRun platform alias normalization, and the fixture pipeline spelling (`linux/x86_64` for `linux/amd64`)
- `platform.All()` is the default selection; adding a future OS (Windows, iOS) means adding canonical platforms and a selection entry

**`internal/collector/`** — Failure artifact collection
- On test failure, collects PipelineRuns, TaskRuns, Pods, logs
- Evidence is redacted and stored under `.lkmabt-runs/`

**`internal/evidence/`** — Artifact storage and manifest generation
- `manifest.json` and `status.json` are written for each run
- Credential-like fields are redacted before writing

**`internal/cleanup/`** — Namespace lifecycle management
- Creates namespaces labeled with `app.konflux-ci.org/managed-by=lkmabt`
- Prompts before deletion (retains the namespace on user decline, failure-artifact collection errors, or an unavailable prompt during finalization); `cleanup` fails with an explicit error when the prompt is unavailable
- Strips finalizers from known deadlock-prone resources before namespace deletion and re-strips them while waiting for deletion

**`skills/`** — Deep-dive reference files for agents.

## Client Types & GVR Definitions

**Client interfaces**:
- `dynamic.Interface` — unstructured Kubernetes API calls (PipelineRuns, TaskRuns, Components, Applications)
- `kubernetes.Interface` — typed API calls (Pods, Nodes, container logs)
- `cluster.ClientSet` — holds `Config`, `Kubernetes`, `Dynamic`, and `Discovery` fields

**Reusable GVR variables** (do NOT redefine these):
```go
cluster.PipelineRunGVR    // tekton.dev/v1/pipelineruns (in build_outputs.go)
cluster.TaskRunGVR        // tekton.dev/v1/taskruns
cluster.ApplicationGVR    // appstudio.redhat.com/v1alpha1/applications (in fixtures.go)
cluster.ComponentGVR      // appstudio.redhat.com/v1alpha1/components
cleanup.NamespaceGVR      // v1/namespaces (in cleanup.go)
workflow.PipelineRunGVR   // tekton.dev/v1/pipelineruns (in live_stage.go)
collector.TaskRunGVR      // tekton.dev/v1/taskruns (in failed_builds.go)
```

**When adding new GVRs**: define them in the most specific package (e.g., an
ImageRepository GVR belongs in the package that needs it). Reuse existing
definitions via import when the same GVR is needed across packages.

## Workflow Execution Flow

1. **Preflight** — Validate cluster connectivity, multi-platform-controller deployment, host-config entries (a static `host.<name>.platform` entry must also have `address`, `user`, and `secret`), and the cluster's GitHub App installation (GitHub runs; interactive runs wait for and can extend a missing installation wait). Each requested platform is classified against the live host-config: an undeclared platform is informational and the selection is narrowed to the supported subset for this cluster; a declared but incomplete platform fails, as does a cluster that supports none of the requested platforms. The GitHub App check is independent of architecture support
2. **Fixture Ready** — Validate test repositories are accessible, read current Dockerfile SHAs
3. **Triggered** — Set each fixture PipelineRun's `build-platforms` parameter to the selected architectures, append trigger comments to Dockerfiles, commit changes. Configuration commits use the `lkmabt configure [skip ci]` subject so PaC creates no throwaway PipelineRun for them
4. **Builds Verified** — Wait for each component's Buildah TaskRuns to succeed (timeout configurable); capture each component's build-output evidence on the poll that observes its build success, while its PipelineRun and TaskRuns still exist. The PipelineRun's post-build tasks are not a completion gate
5. **Build Outputs Verified** — Confirm the captured evidence provides one successful output per selected architecture, fetching only components that were not captured
6. **Completed** — Prompt for namespace cleanup

On failure, transition to **Failed** phase and collect artifacts. Clusters run
sequentially in clusters-file order; each completes or fails before the next
starts.

### Build Wait Progress and Deadlock Detection
PaC onboarding and build waits emit at most one progress line per minute: PaC
shows enabled/total plus pending component names; build waits name each
component's matching PipelineRun, pending/running age, and Buildah TaskRun
counts, with milestone lines at each phase entry. `VerifyBuilds` inspects the
`pipelinesascode.tekton.dev/git-auth-secret` annotation on pending
PipelineRuns: when the referenced Secret is absent for more than two minutes it
fails with `MissingPacAuthSecretError`, which names the run, component, Secret,
and pending duration. The diagnostic reads PaC's `secret-created` annotation
and finalizer: a Secret PaC created and then lost points at a concurrent run, a
PipelineRun without the `pipelinesascode.tekton.dev/finalizer` was never
reconciled by the PaC watcher, and a reconciled run with
`secret-created=false` has not finished Secret creation. An unreconciled run
causes one automatic component retrigger and a second build wait; a second
unreconciled run fails with the same diagnosis. A Secret read that fails for any
other reason warns once and keeps waiting. A failed Buildah TaskRun, or a
PipelineRun that fails before all of its Buildah TaskRuns complete, fails the
wait immediately instead of waiting for more TaskRuns. Successful Buildah
TaskRuns finish the component as soon as they are observed; LKMABT does not wait
for image-index, scans, tagging, or other post-build tasks, and a PipelineRun
that reports failure only after every Buildah TaskRun succeeded is not a build
failure.

Build-output evidence is captured on the same poll that observes a component
succeed, from the PipelineRun and TaskRun objects already listed, so PaC
`max-keep-runs` cleanup cannot erase it while slower components build. Cached
evidence satisfies verification even after the PipelineRun is pruned. If a
PipelineRun that was observed at least once disappears before its evidence is
captured, the wait fails immediately with
`PipelineRunDisappearedError` naming the component and the last observed
PipelineRun, instead of waiting for the build timeout.

### Tenant Namespace Locking
`LiveRunner.Run` and `cleanupRun` take a non-blocking `flock` on
`<user cache>/lkmabt/locks/<sha256(server + namespace)>.lock` before mutating
cluster resources. A contended call fails immediately with the namespace,
server, and recorded holder (run ID, pid, start time), before any artifact is
created. The lock is per Unix user and host; it does not protect against runs
from other machines or users, and prefixed `<tenant>-*` namespaces are outside
its key.

## Testing Structure

Ginkgo v2 + Gomega, TDD (failing test first). Test files are colocated with the
implementation and use external `package <pkg>_test`; each package registers a
runner in `<package>_suite_test.go`. Fake clients come from
`k8s.io/client-go/dynamic/fake` and `k8s.io/client-go/kubernetes/fake`; command
execution uses a fake `clusterauth.CommandRunner`.

Style rules, pitfalls, and test organization: `skills/testing-standards.md`.

## Key Behavioral Constraints

### Fixed Fixtures Only
The harness operates against two hardcoded test repositories:
- `redhat-appstudio-qe/dr_test_mathwizz` (GitHub)
- `konflux-qe/dr_test_mathwizz_gl` (GitLab)

Never suggest creating, forking, or deleting repositories. The harness reuses
these fixtures across runs.

### Namespace Ownership
Namespaces are labeled with `app.konflux-ci.org/managed-by=lkmabt` and
`app.konflux.org/run-id=<run-id>`. The harness:
- Creates namespaces if missing
- Deletes owned scoped namespaces (`<tenant>` and `<tenant>-*`) automatically and logs the deletion
- Prompts for unowned or label-mismatched namespaces individually, re-inspecting each before deletion; a declined prompt fails only that cluster at `preflight`

### Artifact Retention
All runtime evidence is stored under `.lkmabt-runs/`:
- `latest/` is a real directory holding the current aggregate run. At the next
  invocation the previous run is renamed in place to its
  `multi-run-<hh:mm_d.m.yyyy>_<aggregate-run-id>/` using the run ID and start
  time in `.aggregate.json`, and a fresh `latest/` is created.
- The top-level reports and aggregate log are prefixed with the aggregate run
  name, so `ls latest/` shows when the run started; cluster and provider logs
  keep their directory-scoped `run.log` name.
- A run holds `.latest.lock` for its whole duration, so a second run against the
  same state directory fails immediately instead of writing into the live
  `latest/`; if rotation fails for a filesystem reason, the new run is written
  to its permanent directory with a warning.
- No run is stored twice, and `latest` is not a symlink.
- Failed runs retain `session/manifest.json`, workload snapshots, pod logs, and `collection-report.json`
- Credential-like fields in JSON evidence are redacted before writing

### Cluster Context Validation
The API server for each cluster comes from the `server` field of its
clusters-file entry; when omitted, it defaults to the loaded kubeconfig's own
server.

### Per-Cluster GitHub App Validation
Each Konflux cluster onboards repositories through its own GitHub App. The
harness reads that app from `konflux-info/konflux-public-info` and verifies
during `preflight` that the app is installed for the fixture organization, so a
cluster that cannot onboard the fixture fails before the run creates a tenant
namespace or touches the fixture. In an interactive session, a missing
installation prints the installation steps, opens
`https://github.com/apps/<slug>/installations/new`, and rechecks the
installation every 10 seconds. The GitHub API token is acquired earlier in the
run; the browser page only performs the organization-owner installation
approval. Failed checks are retried, and progress, including the latest
failure, is reported every 5 minutes. When the preflight timeout expires, an
interactive run asks whether to keep waiting and, if approved, polls on the run
context until the installation appears or the run is canceled.
Declining, or a non-interactive run, fails with the installation URL.
Installations limited to selected repositories must include the fixture;
a confirmed selection that omits it fails the wait immediately with the
installation settings URL, while selections the token cannot list log a
warning and continue. After an approved wait that outlives the preflight
deadline, the remaining preflight steps continue on the run context.

### Provider Credential Lifecycle
When `GITHUB_TOKEN` is empty, GitHub runs call `gh auth login --web` with a
private `GH_CONFIG_DIR` and `--insecure-storage`, then read the token with
`gh auth token`. GitLab API requests use `Authorization: Bearer`, which accepts
a personal access token; GitLab rejects OAuth tokens sent as `PRIVATE-TOKEN`. A configured
`GITHUB_TOKEN` is checked against the organization's App installations, the
fixture repository's push permission, and the repositories of each
non-suspended installation that covers selected repositories; an interactive run
replaces a token that fails those checks for credential reasons with
the token from the existing `gh` login when it passes them, and otherwise
with the `gh` browser login. Transient, rate-limit, and timeout check failures
abort the run without replacing the token. A 404 for a selected installation the token cannot see is skipped; preflight
verifies the cluster App against the fixture. A classic or OAuth token may need
the `read:user` scope to list selected-installation repositories; add it with
`gh auth refresh -h github.com -s read:user`. GitHub rejects a `gh` OAuth user
token (`gho_`) for that endpoint even with the scope, so the acquisition check
skips it and preflight keeps the warning path. The acquisition check does not
require the fixture in every installation because the cluster app slug is
unknown there; a listing that remains unverifiable after the browser login still
only produces the preflight warning. When `GITLAB_BOT_TOKEN` is empty, GitLab opens the prefilled PAT page (1-day
expiration, `api` and `write_repository`) and prompts for the token without
echo; the token is used directly and is never written to the credential store.
The ephemeral GitLab token is also written to the tenant SCM Secret for PaC;
each cluster's finalization deletes that Secret, and the run-acquired token is
revoked once after the last cluster with `DELETE /personal_access_tokens/self`.
A configured `GITLAB_BOT_TOKEN` is never revoked.

The directory is removed when the run ends, including after failures and
cancellations, and a removal failure fails the run. Authentication requires an
interactive terminal; non-interactive runs must provide the tokens. A
configured `GITHUB_TOKEN` needs the `read:org` scope, an owner who
administers the fixture organization, push access to the fixture repository,
and `read:user` when an App installation is limited to selected repositories.

### GitLab SCM Secret Lifecycle
Fixture setup creates the tenant namespace before writing the GitLab Secret.
When the namespace is absent, it first requests one through the OpenShift
`ProjectRequest` API (`project.openshift.io/v1`, resource `projectrequests`),
which binds the requester as project admin. A NotFound or Forbidden
ProjectRequest API falls back to creating a bare Namespace, which surfaces the
cluster's own namespace RBAC error. A ProjectRequest does not guarantee label
copying, so setup applies the ownership labels after the controller creates the
Namespace. That update is cluster-scoped: the run identity needs `update` on
`namespaces`, and a denial fails setup with an error naming the namespace to
delete manually. The namespace is labeled only when its UID matches the project
returned by the ProjectRequest, so a namespace created by another actor during
the request is refused rather than adopted.

After the namespace exists, setup runs one `SelfSubjectAccessReview` per verb:
`get`, `create`, and `update` on `secrets` in the tenant namespace; ephemeral
tokens also require `delete`. A denial or review error fails fixture setup with
the namespace and an example `oc adm policy add-role-to-user edit <user> -n
<namespace>` grant, before the Secret is written.

Fixtures own exactly one Secret, `gitlab-build-secret`, and label it
`app.konflux-ci.org/managed-by=lkmabt` plus `app.konflux.org/run-id=<run-id>`.
The Secret carries the GitLab SCM labels and repository annotation that
build-service matches. Both labels mark harness ownership: a Secret carrying
them may be updated and adopted by the current run, which refreshes the run ID.
Deletion additionally requires the run ID to equal the current run. A Secret
with that name but missing either label is never updated or deleted; the run
fails with an error naming the Secret and namespace, and a legacy unowned
Secret must be deleted once after access is granted. Secret listing is
opportunistic: a configured token may reuse another repository-scoped Secret
when the API allows it, and a `Forbidden` list falls back to managing the owned
Secret. A run-acquired token is never written into another repository-scoped
Secret, because that Secret would outlive the token's revocation.

Finalization deletes the Secret only for ephemeral tokens and only after
confirming the managed-by and run-ID labels, using a UID precondition when the
Secret has one. A missing Secret is treated as already cleaned up. A failed
ownership check or API call is joined into the returned error while failure
evidence is still collected and saved, so the report retains its artifacts.

### Sequential Cluster Execution
Clusters run one at a time in clusters-file order so run-scoped credentials are
reused and only one cluster is mutated at a time. `both` still runs GitHub and
GitLab concurrently inside a cluster.

### Admission Deferral
- `workflow.ErrDeferred` is the sentinel; `workflow.IsDeferred` uses `errors.Is`. `AdmissionPendingError` (`internal/workflow/admission.go`) unwraps to it and carries the namespace plus each pending `PipelineRun`. Detection requires `PipelineRunPending` in `spec.status` or the `Succeeded` condition reason, no `startTime`, and no child TaskRuns for longer than `admissionPendingThreshold` (10 minutes).
- `workflow.Runner.Run` (`internal/workflow/run.go`) persists `model.Deferral` as the manifest's `deferred` field and in `status.json` (`internal/evidence/manifest.go`), instead of writing `Failure`; the manifest keeps its last completed phase.
- `LiveRunner.Run` (`internal/cli/local.go`) calls `finalizeDeferredRun`, which deletes only the deferred attempt's owned tenant namespace without prompting and skips failure-artifact collection.
- `scheduleClusters` (`internal/cli/multi.go`) retries a deferred cluster once after the first pass with fresh run IDs and trigger commits. `clusterRun.FirstAttempt`, `retryNote`, and `resultLinks` preserve first-attempt results and artifact links; a second deferral becomes `cluster remained admission-pending after one retry: <reason>`. Console output uses `Step.EndDeferred` and `deferredLine` (`internal/cli/status.go`).

### Per-Cluster Platform Support
- `cluster.CheckScheduling` (`internal/cluster/client.go`) returns `SchedulingResult{Checks, Platforms}`; `PlatformSupport{Platform,Supported,Configured,Details}` distinguishes an undeclared platform (`Configured=false,Supported=false`) from a declared but incomplete one (`Configured=true,Supported=false`).
- `RunPreflight` (`internal/cluster/preflight.go`) fails for a declared-but-incomplete platform and when none of the requested platforms is supported; an undeclared platform does not fail preflight. `PreflightResult.Supported()` and `Unsupported()` expose the split.
- `LiveStage.Preflight` (`internal/workflow/live_stage.go`) calls `limitToSupportedPlatforms`, emits an informational notice, and mutates `Request.Platforms` to the supported subset so triggering and verification use it.
- Kueue diagnostics use `cluster.KueueWorkloadGVR` and `AdmissionInspector.QuotaMessage` (`internal/cluster/admission.go`), which reads `QuotaReserved=False.message` from the Workload owned by the PipelineRun. Missing Workloads and errors fall back to the PipelineRun condition and never fail the wait.

## Design Decisions

### Fixed Fixtures Pattern
The tool uses **persistent fixtures** rather than creating/deleting test resources on every run:
- One persistent tenant namespace per provider (GitHub, GitLab); `both`
  starts both providers concurrently inside the cluster with distinct run IDs
  and artifact directories; clusters themselves run sequentially
- One Application with three Components (`mathwizz-web-server`, `mathwizz-history-worker`, `mathwizz-frontend`)
- Persistent provider forks (never deleted by the tool)

**Rationale**: avoids PaC onboarding delays, reduces flakiness from resource
creation/deletion races, enables faster iteration during development.

### Platform Detection Strategy
Build output verification uses TaskRun labels, annotations, params, and results
to determine platform. **No Pod/Node lookup** is performed:
- `PLATFORM` result from Buildah TaskRun
- TaskRun labels/annotations when result is absent
- **Fails early with "pipeline misconfiguration"** if platform cannot be determined from TaskRun metadata alone

**Rationale**: TaskRun metadata is authoritative for pipeline-generated builds.
Pod/Node lookup was over-engineered fallback logic that masked pipeline labeling
bugs.

### Artifact Collection Philosophy
Artifacts are collected **on demand** (failure, explicit collection command)
rather than continuously:
- Baseline collection: manifest, status, essential workload snapshots
- Failure collection: TaskRun YAML snapshots, container logs, pod events, PipelineRun details
- All JSON artifacts are **redacted** (credential-like fields removed) before writing

**Rationale**: reduces storage overhead for successful runs; redaction prevents
accidental credential leakage in shared artifacts.

Required sources retry transient failures (network timeouts, API 408/429/5xx)
twice with exponential backoff before the collector declares artifacts
incomplete; `workload/pods.json` remains mandatory. Optional pod logs are
best-effort per container: one failed log stream no longer discards the rest,
and the collection report records how many were missed.

## Artifact Layout

Each run writes into `.lkmabt-runs/latest/`; the next invocation renames it to
its permanent timestamped directory:

```
.lkmabt-runs/
├── latest/                                          # Current aggregate run (real directory)
│   ├── .aggregate.json                              # Run ID and start time used for rotation
│   ├── <aggregate-run-name>-report.md
│   ├── <aggregate-run-name>-report.html
│   ├── <aggregate-run-name>-run.log
│   └── <cluster>/
│       ├── run.log
│       └── <provider>-run-<hh:mm_d.m.yyyy>_<run-id>/   (run.log, manifest.json, status.json, workload/, failed-builds/, logs/, collection-report.json)
├── .latest.lock                                     # Held for the run; refuses a second process
└── multi-run-<hh:mm_d.m.yyyy>_<aggregate-run-id>/   # Previous aggregate runs
```

### TaskRun YAML Snapshots
Every TaskRun in the namespace is persisted as a complete YAML snapshot before
cleanup:
- **Why**: diagnose PaC, Tekton, and build failures that manifest in TaskRun status/conditions
- **Scope**: all TaskRuns (not just `build-container`), including generated, failed, and auxiliary TaskRuns
- **Redaction**: sensitive fields removed before writing (same boundary as JSON artifacts)
- **Required for cleanup**: namespace deletion is blocked if TaskRun YAML collection fails

### Failed Build Log Collection
When the build stage fails — a failed Buildah TaskRun or a PipelineRun failure
before its Buildah TaskRuns complete — container logs are collected from failed
TaskRun step containers:
- Last 80 lines per container (configurable via `FailedBuildCollector.TailLines`)
- Logs written to `failed-builds/<taskrun>_<container>.log`
- Critical for diagnosing pod admission failures and step container errors

Pod admission errors appear as generic "exited with code 1: Error" in TaskRun
conditions. Container logs capture the actual failure message.

## Gotchas

### Build Output Verification
Build output verification (`internal/cluster/build_outputs.go`) confirms that
Buildah TaskRuns contain one successful output per selected platform. The
harness:
- Reads the `PLATFORM` result, then TaskRun labels, annotations, and `PLATFORM` params
- Normalizes aliases such as `linux/x86_64` to `linux/amd64` and accepts `linux-s390x` and `linux-ppc64le`
- Requires every platform in `Request.Platforms` per component and records the observed outputs and digests
- Fails as a pipeline misconfiguration when a Buildah TaskRun has no recognizable platform
- Captures evidence as each component succeeds and reuses it during `Build Outputs Verified`; a PipelineRun pruned after capture does not invalidate the result, while an observed run pruned before capture fails fast

If verification fails, check that:
1. The multi-platform-controller is running and its `host-config` declares every platform the cluster is expected to run (an undeclared platform narrows the selection instead of failing preflight)
2. The Buildah TaskRuns completed successfully (builds-verified phase passed); the PipelineRun's post-build tasks are not required
3. The fixture PipelineRun `build-platforms` parameter and `build-images` matrix use the selected platforms

### Provider API Rate Limits
GitHub and GitLab providers include retry logic with exponential backoff for 5xx
responses (`internal/providers/retry.go`). API rate limit (429) responses are not
automatically retried.

### Phase Transition Validation
All phase transitions are validated via `model.ValidateTransition()`. Attempting
an invalid transition (e.g., `completed → triggered`) returns an error. The only
valid backward transition is any non-terminal phase → `failed`.

### PipelineRun Overshoot Detection
If multiple PipelineRuns succeed for the same component/commit combination, the
run fails with an overshoot error. This is a regression alarm for STONEINTG-1732.
Check PaC configuration if this occurs.

### Cleanup Hangs
Namespace deletion waits are bounded to five minutes and re-strip finalizers
that controllers re-add; a timeout reports the namespace controller's remaining
resources and finalizers. If cleanup still fails, check:
- Controller pod health (PaC, image-controller, multi-platform-controller)
- Namespace events for webhook denial or admission errors
- `kubectl describe namespace <tenant>` for stuck finalizers from other sources

### Cluster Names Versus kflux Catalog Names
`name` in the clusters file is the report and `--cluster` identity. The kflux
catalog name is derived from it: a leading `kflux-` is removed. When the result
is not itself a catalog cluster, authentication resolves the entry name (or an
explicit server host) to a catalog host label from
`kflux cluster list --output json`; for example `stone-stage-p01` resolves to
`stg-p01`, and the catalog alias names the default kubeconfig path
`~/.kube/config-stg-p01`. If no catalog matches, set `server` or `kubeconfig`;
LKMABT does not run an `oc login` against the ambient context when the target
kubeconfig is missing. A batch kflux login failure is retried per cluster so an
unknown name cannot block the valid ones. `type: roks` entries never call
kflux; the catalog name only names the default kubeconfig path.

### FAS Identity Providers
A cluster whose OAuth login page advertises a Fedora Account System identity
provider (for example `FAS-OpenID`) cannot be refreshed by kflux, which supports
Kerberos and Red Hat SSO only. In an interactive run, the authentication ladder
reads the API server's OAuth metadata and login page and uses
`oc login --web --server=<api>`, letting the browser complete the FAS login. The
probe sends no credentials and refuses issuer or redirect destinations that
resolve to private, loopback, link-local, multicast, CGNAT, or reserved
addresses; the configured API server itself is allowed. The probe is skipped
when the run is non-interactive; those runs need a kubeconfig that already holds
a usable token.

### ROKS First-Time Login
`type: roks` authenticates with `ibmcloud login --sso` and
`ibmcloud ks cluster config`, then verifies with `oc whoami`. When the API
server rejects the written kubeconfig, the cluster fails at `authentication`
with instructions to open its OpenShift web console once from the IBM Cloud
console. That console visit creates the user's cluster identity; no CLI-only
check can prove it happened beforehand.

### Go Workspace Interference
If the checkout sits inside an unrelated parent `go.work`, `./...` patterns fail
with "directory prefix . does not contain modules listed in go.work". Run
commands with `GOWORK=off`.

## Documentation Updates

Read `skills/update-readme.md` only when a change requires editing `README.md`.
It defines the required user-facing section structure and how to merge parallel
documentation edits. It is not general architecture context, and no other task
needs it.
