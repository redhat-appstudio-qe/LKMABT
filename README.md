# Local Multi-Architecture Build Testing Tool for Konflux Clusters

LKMABT validates multi-architecture builds on Konflux clusters. For every
cluster in the clusters file it:

1. checks that the cluster can run multi-platform builds;
2. creates or reuses the tenant namespace, Application, and Components;
3. appends one harmless comment to each fixture Dockerfile and commits it;
4. waits for the matching PipelineRuns to succeed;
5. verifies the pre-publication Buildah outputs cover `linux/amd64` and
   `linux/arm64`.

The harness never copies, creates, forks, renames, or deletes repositories, and
it does not query registries, archives, pruning, backup services, or unrelated
resources.

## Contents

- [Prerequisites](#prerequisites)
  - [Local tools](#local-tools)
  - [Accounts and access](#accounts-and-access)
  - [Cluster requirements](#cluster-requirements)
  - [Network access](#network-access)
- [Setup](#setup)
  - [1. Build LKMABT](#1-build-lkmabt)
  - [2. List the clusters](#2-list-the-clusters)
  - [3. Verify fixture access](#3-verify-fixture-access)
- [Authentication methods](#authentication-methods)
  - [Browser authentication](#browser-authentication)
  - [Configured tokens](#configured-tokens)
- [Running LKMABT](#running-lkmabt)
  - [When the cluster GitHub App is not installed](#when-the-cluster-github-app-is-not-installed)
  - [During the run](#during-the-run)
- [Run logs and artifact retention](#run-logs-and-artifact-retention)

## Prerequisites

### Local tools

| Tool | Needed for |
| --- | --- |
| Go 1.26 or newer | Building LKMABT (see `go.mod`). |
| `oc` | Every run. Each kubeconfig is proved with `oc whoami`; a readable file with an expired token is not enough. |
| `kflux` | `ocp` clusters whose kubeconfig is missing or rejected; LKMABT installs it when absent. |
| `ibmcloud` with the `kubernetes-service` plug-in | `type: roks` clusters. |
| `gh` | GitHub runs with an empty `GITHUB_TOKEN`. |

### Accounts and access

**GitHub.** Your account needs push access to
[redhat-appstudio-qe/dr_test_mathwizz](https://github.com/redhat-appstudio-qe/dr_test_mathwizz);
LKMABT commits the trigger there. Verify it after `gh auth login`:

```bash
gh api repos/redhat-appstudio-qe/dr_test_mathwizz --jq .permissions.push
# true
```

Installing the cluster's Konflux GitHub App requires an owner of the
`redhat-appstudio-qe` organization; push access alone cannot approve an
installation. The App must also be published by the cluster and installed for
the organization; see [Cluster requirements](#cluster-requirements).

**GitLab.** Your account needs at least Maintainer (access level 40) on
[konflux-qe/dr_test_mathwizz_gl](https://gitlab.com/konflux-qe/dr_test_mathwizz_gl);
the trigger commit lands on `main`. Verify it in GitLab or with `glab api`:

```bash
glab api projects/konflux-qe%2Fdr_test_mathwizz_gl --jq '[.permissions.project_access.access_level, .permissions.group_access.access_level] | map(. // 0) | max'
# 40 (Maintainer) or 50 (Owner), including access inherited from konflux-qe
```

**Cluster.** Your identity needs permission to create the tenant namespace
through an OpenShift `ProjectRequest`, cluster-scoped `update` on `namespaces`
to label it for cleanup, and permission to delete it during cleanup. Inside
the namespace the run needs access to `pipelineruns`, `taskruns`,
`applications`, and `components`. GitLab runs also need `get`, `create`, and
`update` on Secrets there, plus `delete` when the run acquires its own GitLab
token. If you administer an existing namespace, grant the Secret access with:

```bash
oc adm policy add-role-to-user edit <user> -n mathwizz-test-gitlab
```

That binding is namespace-scoped; it does not grant the cluster-scoped create
and delete permissions above.

Provider credentials are described in
[Authentication methods](#authentication-methods).

### Cluster requirements

- The `multi-platform-controller` deployment is ready in namespace
  `multi-platform-controller`, and its `host-config` ConfigMap declares
  `linux/amd64` and `linux/arm64`. Dynamic arm64 provisioning is sufficient;
  physical arm64 nodes are not required.
- PaC onboarding is complete for all three components, with current `.tekton`
  configuration on each fixture's default branch. LKMABT waits for PaC
  readiness and does not merge onboarding changes.
- GitHub clusters publish the Konflux GitHub App URL in
  `konflux-info/konflux-public-info` (`integrations.github.application_url`),
  and that app is installed for `redhat-appstudio-qe`. See
  [When the cluster GitHub App is not installed](#when-the-cluster-github-app-is-not-installed).

### Network access

Every run needs network access to:

- each cluster's API server, taken from `server` or from the kubeconfig. ROKS
  entries choose the master endpoint with `ibmcloud-endpoint`: `vpe` (default)
  and `private` endpoints are not publicly routable and are reachable only from
  the VPC or a connected network such as the VPC VPN; `link` uses the IBM Cloud
  Direct Link connection. Connect before starting a run, otherwise cluster
  authentication and every workflow call fail;
- `github.com` and `gitlab.com` for provider API calls and trigger commits;
- `gitlab.cee.redhat.com` when LKMABT has to clone and build kflux.

## Setup

### 1. Build LKMABT

```bash
go build -o bin/lkmabt ./cmd/lkmabt
```

### 2. List the clusters

Start from the example and edit the copy:

```bash
cp .lkmabt-clusters.yaml.example .lkmabt-clusters.yaml
```

The copy is ignored by Git; keep it local. A minimal kflux entry:

```yaml
clusters:
  - name: kflux-ocp-p01
    kflux: ocp-p01
```

| Field | Required | Purpose |
| --- | --- | --- |
| `name` | Yes | Unique identity used by `--cluster`, in logs, and as the report directory name. |
| `kflux` | No | kflux catalog name passed to `kflux cluster login -c`; defaults to `name`. It also names the default kubeconfig path, `~/.kube/config-<kflux>`. |
| `kubeconfig` | No | Kubeconfig path. `~` expands; relative paths resolve against the clusters file. Every kubeconfig is verified on every run. |
| `server` | No | API server URL. Omit it to use the kubeconfig's own server. |
| `type` | No | `ocp` (default) or `roks`. |
| `ibmcloud` | ROKS only | IBM Cloud cluster name or ID passed to `ibmcloud ks cluster config`; defaults to `name`. |
| `ibmcloud-endpoint` | ROKS only | `vpe` (default), `private`, or `link`. |

`kflux` is the short catalog name, not the API server host:
`kflux-ocp-p01.7ayg.p1.openshiftapps.com` is catalog cluster `ocp-p01`. List
catalog names with `kflux cluster list`; an unknown name fails with kflux's
`unknown cluster` error.

A ROKS entry:

```yaml
clusters:
  - name: kflux-lw-p01
    type: roks
    ibmcloud: kflux-lw-p01
    ibmcloud-endpoint: vpe
```

ROKS runs reuse an active `ibmcloud` session or run `ibmcloud login --sso`,
write the entry's kubeconfig with `ibmcloud ks cluster config`, then verify it
with `oc whoami`. If a ROKS kubeconfig is still rejected, open the cluster's
OpenShift web console once from the IBM Cloud console and re-run. ROKS entries
never fall through to kflux or `oc login`.

For non-ROKS entries, kflux refreshes default-path kubeconfigs; if `kflux` is
missing, LKMABT clones and builds it into `~/.local/bin/kflux`. Any entry still
rejected falls back to `oc login`. All available fields are documented in
`.lkmabt-clusters.yaml.example`.

### 3. Verify fixture access

Run the commands from [Accounts and access](#accounts-and-access). In a
terminal, a configured GitHub token is checked before the first cluster; a
browser-login token is only exercised when the first trigger commit is
written. Checking now avoids a failed run.

## Authentication methods

LKMABT needs a GitHub token for GitHub runs and a GitLab token for GitLab runs.
Choose either method; `run both` uses both credentials. Values in the process
environment take precedence over `.lkmabt.env`. A GitHub token acquired through
browser authentication is written to a private temporary directory as a `0600`
file and deleted when the last cluster finishes. A prompted GitLab PAT is kept
for the run and revoked after the last cluster; a configured `GITLAB_BOT_TOKEN`
is never revoked.

### Browser authentication

Use this method when you leave the provider tokens empty. It requires a
terminal; a non-interactive run cannot open a browser and fails before any
cluster work.

**GitHub.** LKMABT prints
`github: authentication required; starting the gh browser login` and runs
`gh auth login --web` in an isolated `GH_CONFIG_DIR`. It requests the `repo`,
`read:org`, and `read:user` scopes. Complete the browser flow. Without a
terminal, the run fails with:

```text
GitHub authentication is required: set GITHUB_TOKEN or run in a terminal to use the gh browser login
```

**GitLab.** LKMABT opens the prefilled personal access token page (scopes
`api` and `write_repository`) and asks you to set the expiration to 1 day.
Create the token, then paste it at the hidden `Paste the personal access token:`
prompt. PaC cannot use OAuth tokens, so there is no Web login. Interactive entry
is supported only for `gitlab.com`; set `GITLAB_BOT_TOKEN` for self-managed
instances, custom ports, or HTTP endpoints. The token is written into the tenant
SCM Secret `gitlab-build-secret` so PaC can clone the fixture. Each cluster's
finalization deletes its Secret; the acquired PAT is revoked once after the last
cluster with `DELETE /personal_access_tokens/self`. Without a terminal, the run
fails with:

```text
GitLab authentication is required: set GITLAB_BOT_TOKEN or run in a terminal to paste a personal access token
```

### Configured tokens

Create the environment file from the example and fill in the tokens you use:

```bash
cp .lkmabt.env.example .lkmabt.env
```

Pass it to a run with `--env-file .lkmabt.env`; LKMABT does not read the file
automatically. The copy is ignored by Git; keep it local. The parser reads
`KEY=VALUE` lines and does not execute the file.

| Variable | Notes |
| --- | --- |
| `GITHUB_TOKEN` | Needs `repo`, `read:org`, and push access to the fixture; its owner must administer `redhat-appstudio-qe` to list App installations. Leave empty for the browser login. |
| `GITLAB_BOT_TOKEN` | Personal access token with `api` and `write_repository`. Leave empty for the token-page prompt on `gitlab.com`. LKMABT never revokes it. |
| `GITLAB_API_URL` | Defaults to `https://gitlab.com/api/v4`. |

**`GITHUB_TOKEN`.** In a terminal, LKMABT checks before the first cluster that
the token can list App installations for `redhat-appstudio-qe` and can push to
the fixture. A token GitHub rejects is replaced by an existing `gh` login when
that login passes the same checks, and otherwise by the `gh` browser login.
Transient failures, rate limits, and timeouts abort the run without replacing
the token. Classic or OAuth tokens need `read:org`. Cluster preflight also
lists installation repositories for Apps installed for selected repositories;
when the token cannot list them, preflight logs a warning and continues, and
the trigger-stage PaC wait remains the backstop.

**`GITLAB_BOT_TOKEN`.** The token is used as-is and is never revoked. PaC
reuses a usable `gitlab-build-secret` when one exists; otherwise LKMABT creates
it and keeps it for later runs.

## Running LKMABT

Run from the checkout root, or pass absolute paths. `--clusters-file` defaults
to `.lkmabt-clusters.yaml`.

```bash
# Browser authentication
./bin/lkmabt run github
./bin/lkmabt run gitlab
./bin/lkmabt run both

# Configured tokens
./bin/lkmabt run github --env-file .lkmabt.env
./bin/lkmabt run gitlab --env-file .lkmabt.env
./bin/lkmabt run both --env-file .lkmabt.env
```

- `github` and `gitlab` test one fixture; `both` tests both.
- Clusters run one at a time in clusters-file order. Each cluster finishes or
  fails before the next one starts. `both` runs the two providers concurrently
  inside a cluster, and console lines from each provider carry a
  `github run - ` or `gitlab run - ` prefix.
- Each run uses one tenant namespace per provider it tests:
  `mathwizz-test-github` for GitHub and `mathwizz-test-gitlab` for GitLab.
  Single-provider runs accept `--tenant-namespace`.
- In a terminal, each workflow phase runs an animated spinner that is replaced
  in place by a green `✓` line naming the action and its emoji, such as
  ` ✓ Preflight cluster (github) 🔍`. Non-interactive output (redirected
  streams or CI) gets the same information as plain, non-animated
  ` • <action>  ...` / ` ✓ <action>` / ` ✗ <action>` lines with no ANSI codes.
- The console prints a `cluster <name>` heading per cluster,
  `cluster <name>: finished (<n> passed, <n> failed)`, one result line per
  provider and cluster, and the report paths. A cluster that fails
  authentication prints ` ✗ Authenticate cluster <name> 🔐` and the reason.
- Reports and evidence are stored under `.lkmabt-runs/`; see
  [Run logs and artifact retention](#run-logs-and-artifact-retention).

`cleanup` needs a provider run ID, which is the suffix of
`github-run-<time>_<date>_<run-id>` or `gitlab-run-<time>_<date>_<run-id>`
inside `.lkmabt-runs/<aggregate>/<cluster>/`. The aggregate run ID in the
report title is not accepted. Use the ID belonging to the provider whose
namespace you want to delete:

```bash
./bin/lkmabt cleanup --cluster <name> --run-id <run-id>
```

`cleanup` authenticates the cluster, finds the run by exact run ID, verifies
the saved artifacts and both ownership labels, and asks before deleting the
namespace. A `both` run creates a separate provider run ID and namespace for
GitHub and GitLab, so clean up each namespace with its own provider run ID.

### When the cluster GitHub App is not installed

GitHub preflight reads the App slug from `konflux-info/konflux-public-info` and
checks that the App is installed for `redhat-appstudio-qe`. This happens
before the run touches the tenant namespace or commits a trigger.

**Interactive run.** LKMABT reports
`GitHub App <slug> is not installed for organization redhat-appstudio-qe.` and
opens `https://github.com/apps/<slug>/installations/new` (the URL is printed if
the browser does not open). To complete the installation:

1. sign in to the browser as an organization owner and approve the App;
2. select the `redhat-appstudio-qe` organization;
3. grant access to `redhat-appstudio-qe/dr_test_mathwizz`, or to all
   repositories;
4. return to the terminal.

LKMABT rechecks every 10 seconds and continues automatically once the
installation appears; it prints
`GitHub App <slug> installation verified`. Progress, including the latest check
failure, is reported every 5 minutes. If the preflight timeout (default 10
minutes; `--preflight-timeout`) expires, the run asks
`GitHub App <slug> is still not installed. Keep waiting?`. Answering `y`
continues the wait; declining fails that cluster at `preflight`. `Ctrl-C`
cancels the wait.

**Non-interactive run.** The cluster fails at `preflight` with the installation
URL. Other clusters in the file still run.

**Installed for selected repositories, but the fixture is missing.** The check
fails immediately at `preflight` with the installation settings URL. Add
`redhat-appstudio-qe/dr_test_mathwizz` to the installation or select all
repositories.

**Token cannot list installation repositories.** Preflight logs a warning and
continues; the PaC wait during triggering remains the backstop for a missing
selection. This applies to configured tokens and browser logins.

### During the run

- Triggering appends one comment to each of the three Dockerfiles on the
  fixture's default branch and commits it. The fixtures are shared; your
  commits are visible to other users.
- A missing tenant namespace is requested through an OpenShift `ProjectRequest`,
  which binds the requester as project admin; if that API is unavailable or
  forbidden, LKMABT creates a bare `Namespace`. Cleanup labels are applied only
  after the created Namespace's UID matches the returned project, and the
  GitLab Secret access review runs before the Secret is written.
- LKMABT waits for PaC readiness and for the triggered PipelineRuns.
  `--preflight-timeout` (10m) bounds preflight and fixture setup.
  `--build-timeout` (60m) bounds triggering, the build wait, and build-output
  verification. `--trigger-timeout` (5m) bounds only the wait for a triggered
  PipelineRun to appear. A matching PipelineRun that has already failed fails
  the wait immediately.
- Owned stale tenant namespaces (`<tenant>` and `<tenant>-*`) are deleted
  automatically and logged. Namespaces with absent or different ownership
  labels are prompted individually and re-inspected before deletion. Declining
  fails that cluster at `preflight`.
- After success, LKMABT collects and verifies the run artifacts, then prompts
  before deleting the current tenant namespace. Declining keeps the namespace
  and the successful result. After failure, it saves the failure artifacts
  first, then prompts; if collection is incomplete, it skips the prompt and
  retains the namespace. A non-interactive run never prompts, so both the
  success and failure paths retain the namespace.
- Namespace deletion waits are bounded to five minutes and re-strip
  deadlock-prone finalizers that controllers re-add. A namespace that still
  cannot be deleted fails with its name and the namespace controller's
  remaining resources or finalizers.
- GitLab fixtures own only `gitlab-build-secret`, labeled
  `app.konflux-ci.org/managed-by=lkmabt` and `app.konflux.org/run-id=<run-id>`.
  A Secret carrying both labels may be adopted and refreshed by the current
  run; a same-named Secret missing either label is never modified or deleted,
  and a legacy unowned Secret must be deleted once after access is granted.
- Long waits are never silent: PaC onboarding and build waits report at most
  once per minute, naming pending components, PipelineRun ages, and Buildah
  counts. A pending PipelineRun whose annotated PaC git-auth Secret is missing
  past two minutes fails with the Secret name and whether PaC created it: a
  lost created Secret hints at a concurrent run, while a PipelineRun without
  the PaC finalizer was never reconciled by the PaC watcher. Other Secret read
  errors warn once.
- Two LKMABT processes cannot share one tenant namespace on the same API server.
  Each run and `cleanup` hold a non-blocking per-user file lock keyed by API
  server and namespace; a contended run fails before creating artifacts and
  names the current holder. Use another `--tenant-namespace` or wait for that
  run.

## Run logs and artifact retention

Runtime evidence is stored under `.lkmabt-runs/`.

```text
.lkmabt-runs/
├── multi-run-<hh:mm_d.m.yyyy>_<aggregate-run-id>/   # One directory per invocation
│   ├── .started-at                                  # Start marker used to choose latest/
│   ├── report.md                                    # Markdown report
│   ├── report.html                                  # HTML report
│   ├── run.log                                      # Aggregate run log
│   └── <cluster-name>/                              # One directory per clusters-file entry
│       ├── run.log                                  # Cluster-level log
│       └── <provider>-run-<hh:mm_d.m.yyyy>_<run-id>/
│           ├── manifest.json                        # Run identity, phase, fixture, triggers
│           ├── status.json                          # Compact phase and failure snapshot
│           ├── session/manifest.json                # Collected run manifest
│           ├── workload/applications.json           # Workload snapshot: Applications
│           ├── workload/components.json             # Workload snapshot: Components
│           ├── workload/pipelineruns.json           # Workload snapshot: PipelineRuns
│           ├── workload/taskruns.json               # Workload snapshot: TaskRuns
│           ├── workload/taskruns/                   # TaskRun YAML artifacts
│           ├── workload/pods.json                   # Workload snapshot: Pods
│           ├── logs/<namespace>/<pod>/<container>.log  # Available pod logs
│           ├── failed-builds/                       # Logs from failed TaskRun steps
│           └── collection-report.json               # Artifact collection outcome
├── latest -> multi-run-...                          # Most recently started aggregate run
└── <older aggregate directories>/
```

- `<provider>` is `github` or `gitlab`; `<run-id>` is the provider run ID used
  by `cleanup`.
- `latest` is a relative symlink to the most recently started aggregate run. It
  exposes artifacts as they are written and keeps pointing at that run through
  success or failure; the next run atomically repoints it. If an older
  directory-form `latest` cannot be exchanged atomically, LKMABT preserves it
  and refuses to switch.
- Pod log requests use the run start time as `SinceTime` and stop with the
  run's collection context; historical logs are not requested.
- Report links target concrete directories and only existing files.
- Credential-like fields in JSON evidence are redacted before writing.
- Incomplete failure-artifact collection suppresses cleanup prompting and
  retains the namespace.
