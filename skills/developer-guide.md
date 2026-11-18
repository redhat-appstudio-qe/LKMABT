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

LKMABT validates that the multi-platform-controller handles `linux/amd64` and
`linux/arm64` build orchestration on ROKS Konflux clusters:

1. Triggers builds against fixed test repositories (GitHub and GitLab)
2. Waits for successful PipelineRuns
3. Verifies pre-publication build outputs for both architectures
4. Collects runtime evidence and artifacts

The harness never creates, forks, or deletes repositories. It reuses fixed
fixtures and appends harmless comments to trigger PipelineRuns.

## Package Organization

**`cmd/lkmabt/`** — Entry point
- `main.go` parses the command line and dispatches through `cli.Execute`

**`internal/cli/`** — Command execution
- `Runner` interface defines the execution contract
- `Execute()` drives the full workflow lifecycle
- Owns the aggregate run directory, the sequential cluster runner, and the per-cluster/per-provider run logs

**`internal/clusterfile/`** — Clusters-file schema
- Strict parsing of `name`, `type`, `kflux`, `ibmcloud`, `ibmcloud-endpoint`, `kubeconfig`, and `server` per entry
- `name` is the report/`--cluster` identity; `kflux` is the kflux catalog name (`cluster login -c`) and defaults to `name`
- `type: roks` defaults `ibmcloud` to `name` and `ibmcloud-endpoint` to `vpe`; IBM Cloud fields are rejected on `ocp` entries
- Resolves kubeconfig paths (`~` expansion, relative-to-file); the default is `~/.kube/config-<kflux>`, kflux's own per-cluster path

**`internal/clusterauth/`** — Authentication ladder
- Every non-ROKS kubeconfig is verified with `oc whoami` on every run: an expired token leaves the file readable, so readability proves nothing. A rejected one is refreshed through `kflux cluster login` (default kubeconfig path) or `oc login` (any path) and otherwise fails at `authentication`. `type: roks` entries are verified and refreshed through the IBM Cloud CLI
- ROKS: `ibmcloud target` probes for an active session, `ibmcloud login --sso` runs when absent, `ibmcloud ks cluster config --cluster <ibmcloud> --endpoint <ibmcloud-endpoint>` writes the entry's kubeconfig through `KUBECONFIG`, and a second `oc whoami` verifies the result
- Non-interactive checks are bounded (`oc whoami` 60s, `ibmcloud target`/`ks cluster config` 2m) so one stalled credential plugin fails that cluster instead of blocking the run; only `ibmcloud login --sso` stays interactive

**`internal/auth/`** — Provider credential acquisition
- Creates one run-scoped `lkmabt-auth-*` directory with 0700 credentials written as 0600 files
- GitHub: runs `gh auth login --web` with an isolated `GH_CONFIG_DIR` and `--insecure-storage`
- GitLab: runs `glab auth login` (choose Web) with an isolated `GLAB_CONFIG_DIR`, keeps that directory for OAuth refresh, and deletes the ephemeral SCM Secret during finalization
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
- Preflight checks validate cluster prerequisites (multi-platform-controller deployment, host-config entries)
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

**`internal/collector/`** — Failure artifact collection
- On test failure, collects PipelineRuns, TaskRuns, Pods, logs
- Evidence is redacted and stored under `.lkmabt-runs/`

**`internal/evidence/`** — Artifact storage and manifest generation
- `manifest.json` and `status.json` are written for each run
- Credential-like fields are redacted before writing

**`internal/cleanup/`** — Namespace lifecycle management
- Creates namespaces labeled with `app.konflux-ci.org/managed-by=lkmabt`
- Prompts before deletion (retains namespace on user decline or failure-artifact collection errors)
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

1. **Preflight** — Validate cluster connectivity, multi-platform-controller deployment, host-config entries, and the cluster's GitHub App installation (GitHub runs; interactive runs wait for and can extend a missing installation wait)
2. **Fixture Ready** — Validate test repositories are accessible, read current Dockerfile SHAs
3. **Triggered** — Append trigger comments to Dockerfiles, commit changes
4. **Builds Verified** — Wait for PipelineRuns to succeed (timeout configurable)
5. **Build Outputs Verified** — Confirm Buildah TaskRuns contain node evidence for `linux/amd64` and `linux/arm64`
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
and pending duration and suggests checking for a concurrent run. A Secret read
that fails for any other reason warns once and keeps waiting. A matched
PipelineRun with a `Succeeded=False` condition fails the wait immediately
instead of waiting for Buildah TaskRuns.

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
- `latest/` resolves to the most recently started run directory from run
  creation. It is a relative symlink that exposes artifacts as they are written
  and remains on that run through success or failure until another run starts.
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
both the OAuth token from glab's Web login and a `GITLAB_BOT_TOKEN` access
token; GitLab rejects OAuth tokens sent as `PRIVATE-TOKEN`. A configured
`GITHUB_TOKEN` is checked against the organization's App installations, the
fixture repository's push permission, and the repositories of each
non-suspended installation that covers selected repositories; an interactive run
replaces a token that fails those checks for credential reasons with
the token from the existing `gh` login when it passes them, and otherwise
with the `gh` browser login. Transient, rate-limit, and timeout check failures
abort the run without replacing the token. A classic or OAuth token may need the
`read:user` scope to list selected-installation repositories; add it with
`gh auth refresh -h github.com -s read:user`. GitHub rejects a `gh` OAuth user
token (`gho_`) for that endpoint even with the scope, so the acquisition check
skips it and preflight keeps the warning path. The acquisition check does not
require the fixture in every installation because the cluster app slug is
unknown there; a listing that remains unverifiable after the browser login still
only produces the preflight warning. When `GITLAB_BOT_TOKEN` is empty, GitLab runs
`glab auth login` (choose Web) with a private `GLAB_CONFIG_DIR`, then reads the
token with `glab config get token --host`; the glab directory stays in the
store so `glab api user` can refresh the OAuth token before each cluster. Each
acquired token is kept in a 0600 file inside one run-scoped `lkmabt-auth-*`
temporary directory and reused for every cluster. The ephemeral GitLab token is
also written to the tenant SCM Secret for PaC, and finalization deletes that
Secret and revokes the run-acquired token with
`DELETE /personal_access_tokens/self`. An OAuth token answers 404 and is left
to expire; a configured `GITLAB_BOT_TOKEN` is never revoked.

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
delete manually.

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
opportunistic: reuse or refresh of other repository-scoped Secrets happens
only when the API allows it, and a `Forbidden` list falls back to managing the
owned Secret. Forbidden updates during an ephemeral refresh are skipped.

Finalization deletes the Secret only for ephemeral tokens and only after
confirming the managed-by and run-ID labels, using a UID precondition when the
Secret has one. A missing Secret is treated as already cleaned up. A failed
ownership check or API call is joined into the returned error while failure
evidence is still collected and saved, so the report retains its artifacts.

### Sequential Cluster Execution
Clusters run one at a time in clusters-file order so run-scoped credentials are
reused and only one cluster is mutated at a time. `run both` still runs GitHub and
GitLab concurrently inside a cluster.

## Design Decisions

### Fixed Fixtures Pattern
The tool uses **persistent fixtures** rather than creating/deleting test resources on every run:
- One persistent tenant namespace per provider (GitHub, GitLab); `run both`
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

## Artifact Layout

Each run creates a timestamped directory under `.lkmabt-runs/`:

```
.lkmabt-runs/
├── latest -> multi-run-<hh:mm_d.m.yyyy>_<run-id>/
└── multi-run-<hh:mm_d.m.yyyy>_<run-id>/
    ├── report.md
    ├── report.html
    ├── run.log
    └── <cluster>/
        ├── run.log
        └── <provider>-run-<hh:mm_d.m.yyyy>_<run-id>/   (run.log, manifest.json, status.json, workload/, failed-builds/, logs/, collection-report.json)
```

### TaskRun YAML Snapshots
Every TaskRun in the namespace is persisted as a complete YAML snapshot before
cleanup:
- **Why**: diagnose PaC, Tekton, and build failures that manifest in TaskRun status/conditions
- **Scope**: all TaskRuns (not just `build-container`), including generated, failed, and auxiliary TaskRuns
- **Redaction**: sensitive fields removed before writing (same boundary as JSON artifacts)
- **Required for cleanup**: namespace deletion is blocked if TaskRun YAML collection fails

### Failed Build Log Collection
When a PipelineRun fails, container logs are collected from failed TaskRun step
containers:
- Last 80 lines per container (configurable via `FailedBuildCollector.TailLines`)
- Logs written to `failed-builds/<taskrun>_<container>.log`
- Critical for diagnosing pod admission failures and step container errors

Pod admission errors appear as generic "exited with code 1: Error" in TaskRun
conditions. Container logs capture the actual failure message.

## Gotchas

### Build Output Verification
Build output verification (`internal/cluster/build_outputs.go`) confirms that
Buildah TaskRuns contain node evidence for both architectures. The harness:
- Reads TaskRun YAML artifacts from `.tekton.dev~1results~1TASKRUN_ARTIFACTS_PATH~1outputs~1taskrun.yaml`
- Extracts `provenance.buildConfig.tasks[].invocation.environment.labels["konflux.dev/node-arch"]`
- Expects exactly one `linux/amd64` and one `linux/arm64` entry per component

If verification fails, check that:
1. The multi-platform-controller is running and has valid `host-config` entries
2. The PipelineRuns completed successfully (builds-verified phase passed)
3. The Buildah TaskRuns wrote TASKRUN_ARTIFACTS results

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
`name` in the clusters file is the report and `--cluster` identity; `kflux` is
the catalog name (`kflux cluster list`). They differ when the user's name is the
API server's first label, e.g. host `kflux-ocp-p01.7ayg.p1.openshiftapps.com` is
catalog cluster `ocp-p01`. An unknown catalog name surfaces kflux's own
`unknown cluster` error. `type: roks` entries never call kflux; their `kflux`
value only names the default kubeconfig path.

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
