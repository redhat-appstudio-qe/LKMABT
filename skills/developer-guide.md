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
- Owns the aggregate run directory, the fixed cap of three concurrent clusters, and the per-cluster/per-provider run logs

**`internal/clusterfile/`** — Clusters-file schema
- Strict parsing of `name`, `type`, `kflux`, `ibmcloud`, `ibmcloud-endpoint`, `kubeconfig`, and `server` per entry
- `name` is the report/`--cluster` identity; `kflux` is the kflux catalog name (`cluster login -c`) and defaults to `name`
- `type: roks` defaults `ibmcloud` to `name` and `ibmcloud-endpoint` to `vpe`; IBM Cloud fields are rejected on `ocp` entries
- Resolves kubeconfig paths (`~` expansion, relative-to-file); the default is `~/.kube/config-<kflux>`, kflux's own per-cluster path

**`internal/clusterauth/`** — Authentication ladder
- `ocp` entries win with an existing readable kubeconfig; `type: roks` entries are verified with `oc whoami` on every run and refreshed through the IBM Cloud CLI when the API server rejects the kubeconfig
- ROKS: `ibmcloud target` probes for an active session, `ibmcloud login --sso` runs when absent, `ibmcloud ks cluster config --cluster <ibmcloud> --endpoint <ibmcloud-endpoint>` writes the entry's kubeconfig through `KUBECONFIG`, and a second `oc whoami` verifies the result

**`internal/report/`** — Aggregate reports
- Renders Markdown and HTML reports for a multi-cluster run

**`internal/config/`** — Configuration parsing
- Parses environment files and command-line arguments
- Validates required credentials and cluster context

**`internal/workflow/`** — Core orchestration logic
- `LiveStage` coordinates the full test workflow
- Phases: preflight → fixture-ready → triggered → builds-verified → build-outputs-verified → completed
- Each phase transition is validated (see `model.ValidateTransition`)

**`internal/cluster/`** — Kubernetes interactions
- `RealCluster` wraps k8s client-go operations
- Preflight checks validate cluster prerequisites (multi-platform-controller deployment, host-config entries)
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
- Strips finalizers from known deadlock-prone resources before namespace deletion

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

1. **Preflight** — Validate cluster connectivity, multi-platform-controller deployment, host-config entries
2. **Fixture Ready** — Validate test repositories are accessible, read current Dockerfile SHAs
3. **Triggered** — Append trigger comments to Dockerfiles, commit changes
4. **Builds Verified** — Wait for PipelineRuns to succeed (timeout configurable)
5. **Build Outputs Verified** — Confirm Buildah TaskRuns contain node evidence for `linux/amd64` and `linux/arm64`
6. **Completed** — Prompt for namespace cleanup

On failure, transition to **Failed** phase and collect artifacts.

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

## Design Decisions

### Fixed Fixtures Pattern
The tool uses **persistent fixtures** rather than creating/deleting test resources on every run:
- One persistent tenant namespace per provider (GitHub, GitLab); `run both`
  starts them concurrently with distinct run IDs and artifact directories
- One Application with three Components (`mathwizz-web-server`, `mathwizz-history-worker`, `mathwizz-frontend`)
- Persistent provider forks (never deleted by the tool)

**Rationale**: avoids PaC onboarding delays, reduces flakiness from resource
creation/deletion races, enables faster iteration during development.

### Trigger Pattern from DR Suite
Trigger logic is based on
`infra-deployments/tests/disaster-recovery/tenant_application_lifecycle.go`:
- Append timestamped comment to each component's Dockerfile
- Commit through provider API
- Wait for PaC-triggered PipelineRuns matching the exact commit SHA

This pattern is proven reliable in the DR suite's periodic CI runs.

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

## Relationship to Disaster Recovery Suite

LKMABT extracts patterns from `infra-deployments/tests/disaster-recovery` but is
not part of the DR e2e suite. It is a standalone local operator tool for
validating multi-arch builds. Consult the DR suite's
`tenant_application_lifecycle.go` for trigger pattern changes and collection
helpers.

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
If cleanup hangs despite finalizer stripping, check:
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
