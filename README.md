# Local Multi-Architecture Build Testing Tool for Konflux Clusters

LKMABT validates multi-architecture builds on Konflux clusters. For every
cluster in the clusters file it:

1. Checks that the cluster can run multi-platform builds;
2. Creates or reuses the tenant namespace, Application, and Components;
3. Selects the architectures in each fixture PipelineRun and appends one
   harmless comment to each fixture Dockerfile;
4. Waits for the matching PipelineRuns to succeed;
5. Verifies the pre-publication Buildah outputs cover the selected
   architectures. All four are selected by default: `linux/amd64`,
   `linux/arm64`, `linux/s390x`, and `linux/ppc64le`.

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
  - [1. Install LKMABT](#1-install-lkmabt)
  - [2. List the clusters](#2-list-the-clusters)
  - [3. Verify fixture access](#3-verify-fixture-access)
- [Authentication methods](#authentication-methods)
  - [Browser authentication](#browser-authentication)
  - [Configured tokens](#configured-tokens)
- [Running LKMABT](#running-lkmabt)
  - [Selecting architectures](#selecting-architectures)
  - [LKMABT under the hood](#lkmabt-under-the-hood)
  - [When the cluster GitHub App is not installed](#when-the-cluster-github-app-is-not-installed)
  - [During the run](#during-the-run)
- [Cleaning up a namespace](#cleaning-up-a-namespace)
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
  `multi-platform-controller`, and its `host-config` ConfigMap declares every
  architecture the run selects, either in a local/dynamic platform list or on a
  static `host.<name>.platform` entry. By default LKMABT requires
  `linux/amd64`, `linux/arm64`, `linux/s390x`, and `linux/ppc64le`; select
  `aws`, `ibm`, or a single architecture to narrow the requirement. Dynamic
  provisioning is sufficient; physical architecture nodes are not required.
- PaC onboarding is complete for all three components, with current `.tekton`
  configuration on each fixture's default branch. LKMABT waits for PaC
  readiness and does not merge onboarding changes.
- GitHub clusters publish the Konflux GitHub App URL in
  `konflux-info/konflux-public-info` (`integrations.github.application_url`),
  and that app is installed for `redhat-appstudio-qe`. See
  [When the cluster GitHub App is not installed](#when-the-cluster-github-app-is-not-installed).

### Network access

Every run needs network access to:

- Each cluster's API server, taken from `server` or from the kubeconfig. ROKS
  entries choose the master endpoint with `ibmcloud-endpoint`: `vpe` (default)
  and `private` endpoints are not publicly routable and are reachable only from
  the VPC or a connected network such as the VPC VPN; `link` uses the IBM Cloud
  Direct Link connection. Connect before starting a run, otherwise cluster
  authentication and every workflow call fail;
- `github.com` and `gitlab.com` for provider API calls and trigger commits;
- `gitlab.cee.redhat.com` when LKMABT has to clone and build kflux.

## Setup

### 1. Install LKMABT

```bash
./scripts/install.sh
```

The installer builds `lkmabt` and installs it into a writable directory already
on `PATH`, preferring `$GOBIN`, `$(go env GOPATH)/bin`, `~/.local/bin`, `~/bin`,
or `/usr/local/bin`. If none is writable, it installs into `~/.local/bin` and
prints the one-line `PATH` change instead of leaving `lkmabt` undiscoverable.
It builds with `GOWORK=off`, so a parent `go.work` that does not list this
module cannot block installation.

Every example below invokes `lkmabt` from `PATH`.

### 2. List the clusters

Start from the example and edit the copy:

```bash
cp .lkmabt-clusters.yaml.example .lkmabt-clusters.yaml
```

The copy is ignored by Git; keep it local. 
A ROSA cluster entry looks like this:

```yaml
clusters:
  - name: kflux-ocp-p01
```

A cluster's entry can also have the following fields:

| Field               | Required  | Purpose                                                                                                                                                           |
| ------------------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`              | Yes       | Unique identity used by `--cluster`, in logs, and as the report directory name. It also derives the kflux catalog name and the default kubeconfig path.           |
| `kubeconfig`        | No        | Kubeconfig path, `~/.kube/config-<catalog>` by default. `~` expands; relative paths resolve against the clusters file. Every kubeconfig is verified on every run. |
| `server`            | No        | API server URL. Omit it to use the kubeconfig's own server.                                                                                                       |
| `type`              | No        | `ocp` (default) or `roks`.                                                                                                                                        |
| `ibmcloud`          | ROKS only | IBM Cloud cluster name or ID passed to `ibmcloud ks cluster config`; defaults to `name`.                                                                          |
| `ibmcloud-endpoint` | ROKS only | `vpe` (default), `private`, or `link`.                                                                                                                            |

The kflux catalog name comes from `name`: a leading `kflux-` is removed and
other names are unchanged. The host `kflux-ocp-p01.7ayg.p1.openshiftapps.com`
is therefore catalog cluster `ocp-p01` through the name `kflux-ocp-p01`. 
List catalog names with `kflux cluster list`; an unknown catalog name fails with
kflux's `unknown cluster` error. The `kflux` field was removed; a file that still sets it is rejected with a message to delete the field and keep the
prefix on `name`.

A ROKS cluster entry looks like this:

```yaml
clusters:
  - name: kflux-lw-p01
    type: roks
    ibmcloud: kflux-lw-p01
    ibmcloud-endpoint: vpe
```

ROKS cluster runs reuse an active `ibmcloud` session or run `ibmcloud login --sso`,
write the entry's kubeconfig with `ibmcloud ks cluster config`, then verify it
with `oc whoami`. 
If a ROKS kubeconfig is still rejected, open the cluster's
OpenShift web console once from the IBM Cloud console and re-run. 
ROKS entries never fall through to kflux or `oc login`.

For ROSA cluster entries, kflux refreshes default-path kubeconfigs; if `kflux` is
missing, LKMABT clones and builds it into `~/.local/bin/kflux`. 
Any entry still rejected falls back to `oc login`. 
All available fields are documented in `.lkmabt-clusters.yaml.example`.

### 3. Verify fixture access

Run the commands from [Accounts and access](#accounts-and-access). In a
terminal, a configured GitHub token is checked before the first cluster; a
browser-login token is only exercised when the first trigger commit is
written. Checking now avoids a failed run.

## Authentication methods

LKMABT needs a GitHub token for GitHub runs and a GitLab token for GitLab runs.
To provide these, choose either method; `both` uses both credentials. Values in the process
environment take precedence over `.lkmabt.env`. 
A GitHub token acquired through browser authentication is written to a private temporary directory as a `0600` file and deleted when the last cluster finishes. A prompted GitLab PAT is kept for the run and revoked after the last cluster; a configured `GITLAB_BOT_TOKEN`
is never revoked.

### Browser authentication

Use this method when you want to leave the provider tokens empty in `.lkmabt.env`. This authentication method requires a terminal; a non-interactive run cannot open a browser and therefore fails before any cluster work.

#### GitHub 

LKMABT prints `github: authentication required; starting the gh browser login` and runs `gh auth login --web` in an isolated `GH_CONFIG_DIR`. It requests the `repo`, `read:org`, and `read:user` scopes. Complete the browser flow. Without a terminal, the run fails with:

```text
GitHub authentication is required: set GITHUB_TOKEN or run in a terminal to use the gh browser login
```

#### GitLab

LKMABT opens the prefilled personal access token page (scopes to `api` and `write_repository`) and asks you to set the expiration to 1 day.
Create the token, then paste it at the hidden `Paste the personal access token:`
prompt. 
PaC cannot use OAuth tokens, so there is no Web login. Interactive entry
is supported only for `gitlab.com`; set `GITLAB_BOT_TOKEN` for self-managed
instances, custom ports, or HTTP endpoints. 
The token is written into the tenant SCM Secret `gitlab-build-secret` so PaC can clone the fixture. Each cluster's finalization deletes its Secret; the acquired PAT is revoked once after the last cluster with `DELETE /personal_access_tokens/self`. Without a terminal, the run
fails with:

```text
GitLab authentication is required: set GITLAB_BOT_TOKEN or run in a terminal to paste a personal access token
```

### Configured tokens

Create the environment file from the example and fill in the tokens you use:

```bash
cp .lkmabt.env.example .lkmabt.env
```

Provider commands read `.lkmabt.env` automatically when the file exists; a
missing default file is fine, so environment-only credentials work. Pass
`--env-file <path>` only to use a different file, and an explicit path that
does not exist is an error. `cleanup` needs no provider credentials and does
not read the file. The copy is ignored by Git; keep it local. The parser reads
`KEY=VALUE` lines and does not execute the file. A GitLab token and its API URL
must come from the same source: when the token's source has no URL but the
other source does, LKMABT fails with an error instead of sending the token to
the default endpoint.

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
# Browser authentication, all four architectures
lkmabt github
lkmabt gitlab
lkmabt both

# One architecture, a provider group, or several
lkmabt github arm64
lkmabt both ibm
lkmabt github s390x arm64
lkmabt github s390x, arm64

# Configured tokens
lkmabt github --env-file my.env
lkmabt gitlab --env-file my.env
lkmabt both --env-file my.env
```

- `github` and `gitlab` test one fixture; `both` tests both.

### Selecting architectures

The optional architecture arguments select the architectures the triggered
builds must produce. One or more arguments follow the fixture command and
precede flags; separate them with spaces or commas (`s390x arm64`,
`s390x,arm64`, or shell-split `s390x, arm64`). Repeated platforms expand once,
in argument order. Omit every architecture argument to build and verify all
four.

| Argument | Builds |
| --- | --- |
| `arm64` | `linux/arm64` |
| `amd64` | `linux/amd64` |
| `s390x` | `linux/s390x` |
| `ppc64le` | `linux/ppc64le` |
| `aws` | `amd64` and `arm64` |
| `ibm` | `s390x` and `ppc64le` |

LKMABT applies the selection to every component:

1. It rewrites the `build-platforms` parameter in each component's `.tekton`
   push PipelineRun. Configuration commits use `[skip ci]`, so PaC creates no
   build for them.
2. It commits one harmless comment to each fixture Dockerfile. These trigger
   commits start the builds.
3. Each component's trigger commit produces one PipelineRun. The PipelineRun's
   `build-images` matrix expands the platform list into one Buildah TaskRun per
   selected architecture. Two architectures across three components therefore
   produce three PipelineRuns and six architecture builds, not six
   PipelineRuns.
4. Preflight requires the cluster to declare every selected platform. Build
   verification requires one successful Buildah output per platform, and the
   final result line lists the tested architectures.

The fixtures are shared; the configuration and Dockerfile commits are visible
to other users.

### LKMABT under the hood

- Clusters run one at a time in clusters-file order. Each cluster finishes or
  fails before the next one starts. `both` runs the two providers concurrently
  inside a cluster; each provider gets its own live status line, and durable
  notices carry a `github run - ` or `gitlab run - ` prefix. Single-provider
  runs are unlabeled.
- Each run uses one tenant namespace per provider it tests:
  `mathwizz-test-github` for GitHub and `mathwizz-test-gitlab` for GitLab.
  Single-provider runs accept `--tenant-namespace`.
- In a terminal, each active workflow phase is a live line with a spinner,
  action, and emoji, such as `⣎⡱ Verify builds (github) 🏗️`. Periodic PaC and
  build heartbeats update that line instead of adding output, and a completed
  step is replaced by a green `✓` line. `both` shows one live line per provider.
  Non-interactive output (redirected streams or CI) prints every heartbeat as a
  plain ` • <action>  ...` / ` ✓ <action>` / ` ✗ <action>` line with no ANSI
  codes, and `run.log` always records the full heartbeat line. Heartbeat
  details wider than the terminal are printed once as a durable line above the
  live region; the live row shows a clipped summary.
- Instructions that need action — GitHub App installation steps, the GitLab
  token page and scopes, and login output — are durable blocks printed above the
  live lines. Animation is suspended while `gh`/`glab` or a hidden token prompt
  owns the terminal. Warnings, failures, and confirmation prompts are also
  durable and never overwritten.
- The console prints a `cluster <name>` heading per cluster,
  `cluster <name>: finished (<n> passed, <n> failed)`, one result line per
  provider and cluster with the tested architectures, and the report paths. A
  cluster that fails authentication prints
  ` ✗ Authenticate cluster <name> 🔐` and the reason.
- Reports and evidence are stored under `.lkmabt-runs/`; see
  [Run logs and artifact retention](#run-logs-and-artifact-retention).

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

- A component is accepted only after its PipelineRun reports `Succeeded=True`;
  successful Buildah TaskRuns alone are not enough, so
  `waiting for PipelineRun ... to finish` means the run is still in progress. A
  missing or unlabeled platform fails as a pipeline misconfiguration.
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

## Cleaning up a namespace

```bash
lkmabt cleanup --cluster <name> --run-id <run-id>
```

- `--run-id` is the provider run ID: the final `<run-id>` segment of
  `github-run-<time>_<date>_<run-id>` or `gitlab-run-<time>_<date>_<run-id>`,
  not the aggregate report run ID. A `both` run creates a separate provider run
  ID and namespace for GitHub and GitLab, so clean up each namespace with its
  own provider run ID.
- `cleanup` authenticates the cluster with the same ladder as a run and checks
  that the connected API server matches the clusters-file entry.
- It locates exactly one aggregate directory holding that run for the cluster;
  `latest/` is searched like any other directory, and the state-directory lock
  is held while the run is verified so a concurrent run cannot rotate `latest/`
  mid-cleanup. If a live run owns `latest/`, only permanent aggregate
  directories are searched.
- It loads the manifest and requires the exact run ID and cluster server, then
  verifies the saved artifacts and collection report.
- It re-inspects the tenant namespace and requires both ownership labels plus
  the exact run ID.
- It holds the tenant namespace lock and prompts
  `Delete tenant namespace <name> owned by run <id> [y/N]`; deletion happens
  only on an explicit yes.
- A non-interactive run cannot approve the prompt, so it retains the namespace
  and exits successfully.
- Deletion waits at most five minutes; on timeout it reports the namespace
  controller's remaining resources or finalizers.
- `cleanup` does not read `.lkmabt.env` and does not accept `--env-file`; it
  needs no provider credentials.

## Run logs and artifact retention

Runtime evidence is stored under `.lkmabt-runs/`.

```text
.lkmabt-runs/
├── latest/                                          # Current aggregate run, a real directory
│   ├── .aggregate.json                              # Run ID and start time used for rotation
│   ├── <aggregate-run-name>-report.md               # Markdown report
│   ├── <aggregate-run-name>-report.html             # HTML report
│   ├── <aggregate-run-name>-run.log                 # Aggregate run log
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
├── .latest.lock                                     # Held for the run by the state-directory owner
└── <aggregate-run-name>/                            # Previous aggregate runs
```

- `<aggregate-run-name>` is
  `multi-run-<hh:mm_d.m.yyyy>_<aggregate-run-id>`.
- `<provider>` is `github` or `gitlab`; `<run-id>` is the provider run ID used
  by `cleanup`.
- `latest/` holds the most recently started aggregate run and is written in
  place. At the next invocation the previous `latest/` is renamed to its
  `<aggregate-run-name>/` using the run ID and start time in `.aggregate.json`,
  and a fresh `latest/` is created.
- The top-level reports and aggregate log carry the aggregate run name, so
  `ls latest/` shows when the run started. Cluster and provider logs keep their
  directory-scoped `run.log` name.
- No run is stored twice and `latest` is not a symlink.
- A run holds `.latest.lock` for its whole duration. A second run against the
  same state directory fails immediately with an actionable message instead of
  writing into the live run's `latest`; wait for the first run or pass a
  different `--state-dir`.
- If rotation fails for a filesystem reason, the new run is written to its
  permanent directory and a warning is logged.
- Pod log requests use the run start time as `SinceTime` and stop with the
  run's collection context; historical logs are not requested.
- Report links target concrete directories and only existing files.
- Credential-like fields in JSON evidence are redacted before writing.
- Incomplete failure-artifact collection suppresses cleanup prompting and
  retains the namespace.
