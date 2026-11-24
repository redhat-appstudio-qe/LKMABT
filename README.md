# Local Multi-Architecture Build Testing Tool for Konflux Clusters

*One job: make a Konflux cluster prove it builds the architectures you selected.*

LKMABT validates multi-architecture builds on Konflux clusters. For every
cluster in the clusters file it:

1. Checks that the cluster can run multi-platform builds;
2. Creates or reuses the tenant namespace, Application, and Components;
3. Selects the architectures in each fixture PipelineRun and appends one
   harmless comment to each fixture Dockerfile;
4. Waits for each component's Buildah TaskRuns to succeed;
5. Verifies the pre-publication Buildah outputs cover the selected
   architectures. All four are selected by default: `linux/amd64`,
   `linux/arm64`, `linux/s390x`, and `linux/ppc64le`.

The harness never copies, creates, forks, renames, or deletes repositories, and
it does not inspect image registries, archives, pruning, backup services, or
unrelated resources; it only probes the public endpoints a selected build
downloads from. No dashboard, no telemetry, no *narishkeit*: the evidence lands
in `.lkmabt-runs/`.

It is a smoke test with a clipboard: trigger, wait, verify, file the evidence.
A cluster that declares none of the selected platforms, or that declares an
incomplete or unschedulable platform, fails at preflight, before LKMABT touches
the fixture. A platform the cluster does not declare is informational: LKMABT
continues on that cluster with the platforms its live `host-config` supports. A
component whose build needs a public endpoint the cluster cannot reach is
dropped before it is created, with an alert saying why. A broken build fails
later and leaves its receipt in `.lkmabt-runs/`.

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
    - [GitHub](#github)
    - [GitLab](#gitlab)
  - [Configured tokens](#configured-tokens)
- [Running LKMABT](#running-lkmabt)
  - [Selecting architectures](#selecting-architectures)
  - [LKMABT under the hood](#lkmabt-under-the-hood)
    - [Admission deferral](#admission-deferral)
  - [When the cluster GitHub App is not installed](#when-the-cluster-github-app-is-not-installed)
  - [During the run](#during-the-run)
- [Cleaning up a namespace](#cleaning-up-a-namespace)
- [Run logs and artifact retention](#run-logs-and-artifact-retention)

---
## Prerequisites

The *tachles*: a Go toolchain, cluster credentials, provider accounts with push
access, and a cluster whose multi-platform controller is already configured.
LKMABT checks the last part; it cannot conjure it. Missing one is *tsuris*;
discovering it over coffee beats discovering it at 2 a.m. with a half-finished
run.

### Local tools

| Tool | Needed for |
| --- | --- |
| Go 1.26 or newer | Building LKMABT (see `go.mod`). |
| `oc` | Every run. Each kubeconfig is proved with `oc whoami`; a readable file with an expired token is not enough. |
| `kflux` | `ocp` clusters with a default-path kubeconfig that is missing or rejected; LKMABT installs it when absent. |
| `git` and `make` | Only when `kflux` is absent and LKMABT clones and builds it. |
| `ibmcloud` with the `kubernetes-service` plug-in | `type: roks` clusters. |
| `gh` | GitHub runs without a usable token, including fallback from a rejected configured token. |
| `glab` | Optional; only for the GitLab access check in [Accounts and access](#accounts-and-access). |

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

Push access and organization ownership are different doors: trigger commits
need the first, and the installation flow needs an owner for the second. No
amount of re-running will merge them.

**GitLab.** Your account needs at least Maintainer (access level 40) on
[konflux-qe/dr_test_mathwizz_gl](https://gitlab.com/konflux-qe/dr_test_mathwizz_gl);
the trigger commit lands on the fixture branch (`main` by default). Verify it
in GitLab or with `glab api`:

```bash
glab api projects/konflux-qe%2Fdr_test_mathwizz_gl --jq '[.permissions.project_access.access_level, .permissions.group_access.access_level] | map(. // 0) | max'
# 40 (Maintainer) or 50 (Owner), including access inherited from konflux-qe
```

**Cluster.** Your identity needs permission to create the tenant namespace
through an OpenShift `ProjectRequest`, cluster-scoped `update` on `namespaces`
to label it for cleanup, and permission to delete it during cleanup. Inside
the namespace the run needs access to `pipelineruns`, `taskruns`,
`applications`, and `components`. When the selection includes a component that
downloads from a public endpoint, the run also needs `create`, `get`, and
`delete` on `pods` there: after creating the tenant namespace, LKMABT checks
those permissions and runs one short-lived probe pod per distinct endpoint to
test the cluster's internet egress before it creates those components. A
namespace created through `ProjectRequest` grants its requester project admin,
so the probe check passes there without a separate grant. GitLab runs also need `get`,
`create`, and `update` on Secrets there, plus `delete` when the run acquires its
own GitLab token. If you administer an existing namespace, grant the Secret and
pod access with:

```bash
oc adm policy add-role-to-user edit <user> -n <tenant-namespace>
```

Use the namespace the run will use: `mathwizz-test-github` or
`mathwizz-test-gitlab` by default, or the value passed to
`--tenant-namespace`. That binding is namespace-scoped; it does not grant the
cluster-scoped create and delete permissions above.

RBAC is a hierarchy, not a suggestion: a namespace-scoped `edit` binding will
not lend you cluster-scoped powers.

If `ProjectRequest` is unavailable or forbidden, LKMABT falls back to creating a
bare `Namespace`, which requires cluster-scoped `create` on `namespaces` instead
of the ProjectRequest permission. The fallback does not bind you as project
admin, so the namespace must already grant the access listed above.

Provider credentials are described in
[Authentication methods](#authentication-methods).

### Cluster requirements

These are not suggestions. Preflight checks platforms and access, plus the
GitHub App on GitHub runs, before triggering up to three components that cannot
build.

- The `multi-platform-controller` deployment is ready in namespace
  `multi-platform-controller`, and its `host-config` ConfigMap carries the
  `build.appstudio.redhat.com/multi-platform-config` label. That live ConfigMap
  is authoritative for the architectures each cluster can run. By default
  LKMABT selects `linux/amd64`, `linux/arm64`, `linux/s390x`, and
  `linux/ppc64le`; select `aws`, `ibm`, or a single architecture to narrow the
  selection. Dynamic provisioning is sufficient; physical architecture nodes
  are not required.
  - An architecture the cluster does not declare is informational, not a
    preflight failure. LKMABT prints an alert naming the architectures it will
    not test and the ones it will (`not supported:` / `testing:`), then
    continues on that cluster with the selected architectures the cluster does
    declare.
  - A declared architecture must be schedulable. A static
    `host.<name>.platform` entry needs `address`, `user`, and `secret`; a
    dynamic entry needs a provider type. A declared but incomplete or
    misconfigured entry still fails preflight.
  - A cluster that declares none of the selected architectures fails preflight,
    because it cannot run the selection at all.
  - GitHub App onboarding is a separate preflight check. A cluster that cannot
    install the fixture's GitHub App still fails preflight even when its
    architecture support is sufficient.
- The cluster can reach the public endpoints its selected components' builds
  download from. The backends run `apk add` and `go mod download`
  (`proxy.golang.org`, `dl-cdn.alpinelinux.org`); the frontend runs `npm ci`
  (`registry.npmjs.org`). Before the fixture creates any component, LKMABT
  probes each distinct endpoint once from a pod in the tenant namespace on the
  tenant workload nodes (`konflux-ci.dev/workload=konflux-tenants`), the same
  nodes that run builds. An unreachable endpoint drops every selected component
  that needs it, and the run prints an alert naming the components it will not
  build, the reasons, and the components it will (`not built:` / `reason:` /
  `testing:`). When no selected component can build, the cluster fails fixture
  setup with those reasons instead of running an empty fixture. Only the probe
  script's own failure exit code proves an unreachable endpoint; a probe that
  cannot report a result (missing pod permission, an unschedulable pod, an
  image that cannot be pulled, an evicted or OOM-killed pod) fails the stage
  with the underlying error instead of guessing. The probe runs the frontend's
  Node base image, so a pull failure says nothing about a backend endpoint.
- PaC onboarding is complete for the selected components, with current `.tekton`
  configuration on the fixture branch. Each push PipelineRun must declare a
  `build-platforms` parameter; LKMABT rewrites it and fails when it is absent.
  LKMABT waits for PaC readiness and does not merge onboarding changes.
- GitHub clusters publish the Konflux GitHub App URL in the `info.json` entry
  `integrations.github.application_url` of the
  `konflux-info/konflux-public-info` ConfigMap, and that app is installed for
  `redhat-appstudio-qe`. See
  [When the cluster GitHub App is not installed](#when-the-cluster-github-app-is-not-installed).

### Network access

LKMABT does not tunnel, proxy, or perform miracles. Every run needs network
access to:

- Each cluster's API server, taken from `server` or from the kubeconfig. ROKS
  entries choose the master endpoint with `ibmcloud-endpoint`: `vpe` (default)
  and `private` endpoints are not publicly routable and are reachable only from
  the VPC or a connected network such as the VPC VPN; `link` uses the IBM Cloud
  Direct Link connection. Connect before starting a run, otherwise cluster
  authentication and every workflow call fail;
- `github.com` for GitHub runs and `gitlab.com` for GitLab runs, for provider
  API calls and trigger commits; when `GITLAB_API_URL` points at another host,
  that host replaces `gitlab.com`;
- `gitlab.cee.redhat.com` when LKMABT has to clone and build kflux.

---
## Setup

### 1. Install LKMABT

```bash
./scripts/install.sh
```

The installer builds `lkmabt` and installs it into a writable directory already
on `PATH`, preferring `$GOBIN`, `$(go env GOPATH)/bin`, `~/.local/bin`, `~/bin`,
or `/usr/local/bin`, then any other writable absolute `PATH` directory. If none
is writable, it installs into `~/.local/bin` and exits nonzero after printing
the `export PATH` line needed to make `lkmabt` discoverable. It builds with
`GOWORK=off`, so a parent `go.work` that does not list this module cannot block
installation.

The installer is a *nudnik* about `PATH`: it refuses to leave `lkmabt`
somewhere you cannot find it.

Every example below invokes `lkmabt` from `PATH`.

### 2. List the clusters

Start from the example and edit the copy:

```bash
cp .lkmabt-clusters.yaml.example .lkmabt-clusters.yaml
```

The copy is ignored by Git; keep it local.

An OpenShift cluster entry looks like this:

```yaml
clusters:
  - name: kflux-ocp-p01
```

A cluster's entry can also have the following fields:

| Field               | Required  | Purpose                                                                                                                                                           |
| ------------------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`              | Yes       | Unique identity used by `--cluster`, in logs, and as the report directory name. It also derives or resolves the kflux catalog name and the default kubeconfig path. |
| `kubeconfig`        | No        | Kubeconfig path, `~/.kube/config-<catalog>` by default. `~` expands; relative paths resolve against the clusters file. Every kubeconfig is verified on every run. |
| `server`            | No        | API server URL, an absolute `http` or `https` URL. Omit it to use the kubeconfig's own server. When set, it is also the expected preflight server and the `oc login --server` target. |
| `type`              | No        | `ocp` (default) or `roks`.                                                                                                                                        |
| `ibmcloud`          | ROKS only | IBM Cloud cluster name or ID passed to `ibmcloud ks cluster config`; defaults to `name`.                                                                          |
| `ibmcloud-endpoint` | ROKS only | `vpe` (default), `private`, or `link`.                                                                                                                            |

Two entries cannot share a name or resolve to the same kubeconfig path, and a
non-ROKS entry cannot set `ibmcloud` fields. A `name` must match
`[A-Za-z0-9._-]+` and must not be `.` or `..`. The file must list at least one
cluster under `clusters`, and unknown fields are rejected. The parser is strict
on purpose: a typo in a field name is an error, not a silent default.

The kflux catalog name comes from `name`: a leading `kflux-` is removed. When a
default-path kubeconfig needs refresh and that name is not itself a catalog
cluster, LKMABT resolves a real cluster host label (for example
`stone-stage-p01`) to its catalog alias (`stg-p01`) from
`kflux cluster list --output json`. The alias also names that path
(`~/.kube/config-stg-p01`); an explicit `kubeconfig` is kept. If the name
cannot be resolved, set `server` or `kubeconfig`; LKMABT does not run an ambient
`oc login` for a missing kubeconfig. That is not stubbornness. An ambient
context is how you end up testing the wrong cluster with great confidence.

A ROKS cluster entry looks like this:

```yaml
clusters:
  - name: kflux-lw-p01
    type: roks
    ibmcloud: kflux-lw-p01
    ibmcloud-endpoint: vpe
```

A readable ROKS kubeconfig is verified with `oc whoami` and reused. A missing or
rejected kubeconfig reuses an active `ibmcloud` session, or runs
`ibmcloud login --sso`, then writes the entry's kubeconfig with
`ibmcloud ks cluster config` and verifies it with `oc whoami`. If a written ROKS
kubeconfig is still rejected, open the cluster's OpenShift web console once from
the IBM Cloud console and re-run. That console visit is not superstition; it
creates the cluster identity your kubeconfig needs. ROKS entries never fall
through to kflux or `oc login`.

For OpenShift (`ocp`) entries with default-path kubeconfigs that are missing or
rejected, LKMABT resolves real host labels through `kflux cluster list` and
refreshes those kubeconfigs; if `kflux` is missing, LKMABT clones and builds it
into `~/.local/bin/kflux`. Catalog names absent from the kflux listing are
excluded from the batch login, and a batch login that still fails is retried per
cluster so valid clusters still refresh. `oc login` runs only when
the entry has a server or a readable kubeconfig; a missing kubeconfig with no
server fails with guidance instead of using the ambient kubeconfig's context.
An interactive run whose OAuth login page offers a FAS identity provider uses
`oc login --web` so the browser can complete the FAS login; a non-interactive
run needs a kubeconfig. The probe sends no credentials and refuses issuer or
redirect destinations that resolve to internal or reserved addresses.

All available fields are documented in `.lkmabt-clusters.yaml.example`.

### 3. Verify fixture access

Run the commands from [Accounts and access](#accounts-and-access). In a
terminal, a configured GitHub token is checked before the first cluster; a
browser-login token is first exercised during that cluster's preflight or
fixture setup. Checking now avoids a failed run; five minutes here saves a
*schlep* through failure artifacts later.

---
## Authentication methods

LKMABT needs a GitHub token for GitHub runs and a GitLab token for GitLab runs.
Choose one method per provider; `both` uses both credentials. A non-empty
process-environment value takes precedence over `.lkmabt.env`; a GitLab token
and its `GITLAB_API_URL` must come from the same source. A GitHub token acquired
through browser authentication is written to a private temporary directory as a
`0600` file and deleted when the last cluster finishes. A prompted GitLab PAT is
kept for the run and revoked after the last cluster; a configured
`GITLAB_BOT_TOKEN` is never revoked.

Credentials are handled the boring way on purpose: acquire only what the run
needs, then clean up what LKMABT acquired.

### Browser authentication

Use this method when you want to leave the provider tokens empty in
`.lkmabt.env`. It requires a terminal; a non-interactive run cannot open a
browser and fails before any cluster work.

#### GitHub

LKMABT prints `github: authentication required; starting the gh browser login`
and runs `gh auth login --web` in an isolated `GH_CONFIG_DIR`. It requests the
`repo`, `read:org`, and `read:user` scopes. Complete the browser flow. Without
a terminal, the run fails with:

```text
GitHub authentication is required: set GITHUB_TOKEN or run in a terminal to use the gh browser login
```

#### GitLab

LKMABT opens the prefilled personal access token page (scopes `api` and
`write_repository`) and asks you to set the expiration to 1 day. Create the
token, then paste it at the hidden `Paste the personal access token:` prompt. A
token without the `glpat-` prefix is rejected. PaC cannot use OAuth tokens, so
there is no web login. Interactive entry is supported only for `gitlab.com`;
set `GITLAB_BOT_TOKEN` for self-managed instances, custom ports, or HTTP
endpoints.

The token is written into the tenant SCM Secret `gitlab-build-secret` so PaC can
clone the fixture. The GitLab run's finalization deletes that Secret; the
acquired PAT is revoked once after the last cluster with
`DELETE /personal_access_tokens/self`. Without a terminal, the run fails with:

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
does not exist is an error. The copy is ignored by Git; keep it local. The
parser reads `KEY=VALUE` lines and does not execute the file. A GitLab token
and its API URL must come from the same source: when the token's source has no
URL but the other source does, LKMABT fails with an error instead of sending
the token to the default endpoint.

| Variable | Notes |
| --- | --- |
| `GITHUB_TOKEN` | Needs `repo`, `read:org`, and push access to the fixture; classic tokens may also need `read:user` to list repositories for selected-repository App installations. Its owner must administer `redhat-appstudio-qe` to list App installations. Leave empty for the browser login. |
| `GITLAB_BOT_TOKEN` | Personal access token with `api` and `write_repository`. Leave empty for the token-page prompt on `gitlab.com`. LKMABT never revokes it. |
| `GITLAB_API_URL` | Defaults to `https://gitlab.com/api/v4`. |

**`GITHUB_TOKEN`.** In a terminal, LKMABT checks before the first cluster that
the token can list App installations for `redhat-appstudio-qe`, push to the
fixture, and list repositories for selected-repository App installations. A
token GitHub rejects is replaced by an existing `gh` login when that login
passes the same checks, and otherwise by the `gh` browser login. Transient
failures, rate limits, and timeouts abort the run without replacing the token.
Classic tokens need `read:org` and may need `read:user` for the installation
repository listing; `gh` OAuth tokens skip that listing. Cluster preflight also
lists installation repositories for Apps installed for selected repositories;
when the token cannot list them, preflight skips that check and continues
without user-facing output, and the trigger-stage PaC wait remains the backstop.

**`GITLAB_BOT_TOKEN`.** The token is used as-is and is never revoked. PaC
reuses an existing usable GitLab SCM Secret (labeled
`appstudio.redhat.com/credentials=scm` and
`appstudio.redhat.com/scm.host=gitlab.com`, and annotated for the fixture
repository) when one exists; otherwise LKMABT creates `gitlab-build-secret`
and keeps it for later runs.

---
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

Flags shared by runs and `cleanup`:

| Flag | Default | Notes |
| --- | --- | --- |
| `--clusters-file` | `.lkmabt-clusters.yaml` | Clusters file to read. |
| `--state-dir` | `.lkmabt-runs` | Root for run artifacts and logs. |

Run-only flags:

| Flag | Default | Notes |
| --- | --- | --- |
| `--env-file` | `.lkmabt.env` | A missing default file is ignored; an explicit missing path is an error. |
| `--tenant-namespace` | `mathwizz-test-github` or `mathwizz-test-gitlab` | Single-provider runs select the namespace; `both` fixes it per provider and rejects any other value. |
| `--application` | `mathwizz` | Application name created or reused in the tenant namespace. |
| `--branch` | `main` | Fixture branch for configuration and trigger commits. |
| `--smoke` | off | Build only `mathwizz-history-worker`; a normal run builds all three fixture components, less any component the egress probe drops. Rejected by `cleanup`. |
| `--preflight-timeout` | `10m` | Bounds preflight and fixture setup. |
| `--trigger-timeout` | `5m` | Bounds only the wait for a triggered PipelineRun to appear. |
| `--build-timeout` | `60m` | Bounds each of triggering, the build wait, and build-output verification. |

Smoke runs build the single `mathwizz-history-worker` component; architecture
selection is unchanged.

```bash
lkmabt github --smoke
lkmabt github amd64 --smoke
lkmabt both --smoke
```

The fixture repository is fixed: `--source-repository` must equal the selected
provider's fixture URL (the GitHub fixture for `both`). `cleanup` requires
`--cluster` and `--run-id`.

### Selecting architectures

The optional architecture arguments select the architectures the triggered
builds must produce. One or more arguments follow the fixture command and
precede flags; separate them with spaces or commas (`s390x arm64`,
`s390x,arm64`, or shell-split `s390x, arm64`). Repeated platforms expand once,
in argument order. Omit every architecture argument to select all four by
default; each cluster builds and verifies the subset its `host-config`
declares.

| Argument | Builds |
| --- | --- |
| `arm64` | `linux/arm64` |
| `amd64` | `linux/amd64` |
| `s390x` | `linux/s390x` |
| `ppc64le` | `linux/ppc64le` |
| `aws` | `amd64` and `arm64` |
| `ibm` | `s390x` and `ppc64le` |

The fixture spellings in the committed `build-platforms` value use
`linux/x86_64` for `linux/amd64`; the other platforms are written unchanged.

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
4. Each cluster runs the subset of selected platforms its live `host-config`
   declares. An undeclared platform is informational; a declared but
   incomplete or unschedulable platform fails preflight, and a cluster that
   declares none of the selection fails preflight outright. Build verification
   requires one successful Buildah output per platform the cluster runs. The
   console result line lists the architectures the cluster exercised; the
   per-cluster alert names any requested architecture the cluster skipped.

The fixtures are shared; the configuration and Dockerfile commits are visible
to other users. Your trigger commit is a postcard to everyone else at the
fixture; assuming nobody will notice takes a certain *chutzpah*. One selection
applies to every component the run builds—no per-component architecture
exceptions, because that way lies *mishigas*. A cluster may skip an
architecture or the egress probe may drop a component it cannot build, but the
architecture set is never narrowed for one component only.

### LKMABT under the hood

- Clusters run one at a time in clusters-file order—no parallel heroics. Each
  cluster finishes or fails before the next one starts. `both` runs the two
  providers concurrently inside a cluster; each provider gets its own live
  status line, and durable notices carry a `github run - ` or `gitlab run - `
  prefix. Single-provider runs are unlabeled.
- Each run uses one tenant namespace per provider it tests:
  `mathwizz-test-github` for GitHub and `mathwizz-test-gitlab` for GitLab.
  Single-provider runs accept `--tenant-namespace`; `--application` sets the
  Application name.
- In a terminal, each active workflow phase is a live line with a spinner,
  action, and emoji, such as `⣎⡱ Verify builds (github) 🏗️`. Periodic PaC and
  build heartbeats update that line instead of adding output, and a completed
  step is replaced by a green `✓` line (unless `NO_COLOR` is set). `both` shows
  one live line per provider.
  Non-interactive output (redirected streams or CI) prints a ` • <action>  ...`
  start line, each heartbeat as a plain line, and a ` ✓ <action>` or
  ` ✗ <action>` completion line, with no ANSI codes, and `run.log` always
  records the full heartbeat line. Heartbeat details wider than the terminal
  are printed once as a durable line above the live region; the live row shows
  a clipped summary. The spinner is decorative; the exit code is not.
- Instructions that need action — GitHub App installation steps, the GitLab
  token page and scopes, and login output — are durable blocks printed above the
  live lines. Animation is suspended while `gh` or a hidden token prompt owns
  the terminal. Warnings, failures, and confirmation prompts are also durable
  and never overwritten.
- The console prints a `cluster <name>` heading per cluster,
  `cluster <name>: finished (<n> passed, <n> failed)`, one result line per
  provider and cluster with the architectures it exercised (`architectures:
  ...`) and a `first attempt deferred:` note after a deferred retry, and the
  report paths. A notice names any per-cluster architecture subset. A cluster
  that fails authentication prints
  ` ✗ Authenticate cluster <name> 🔐` and the reason.
- Reports and evidence are stored by default under `.lkmabt-runs/`; see
  [Run logs and artifact retention](#run-logs-and-artifact-retention).

#### Admission deferral

Kueue or another admission controller can hold a triggered PipelineRun before
it starts. LKMABT treats that as a scheduler deferral, not a failure, when all
three are true for more than 10 minutes: the PipelineRun is `PipelineRunPending`
(in `spec.status` or as the `Succeeded` condition reason), it has no
`startTime`, and it has no child TaskRuns.

While the wait continues, the once-per-minute build heartbeat names
`PipelineRunPending`, the PipelineRun age, and the Kueue quota message from the
Workload's `QuotaReserved=False` condition when one is available. When the
threshold passes, the active step ends with a pause marker,
` ⏸ <action> - deferred (admission pending)` instead of a success or failure
marker. Ten minutes is the line between a busy scheduler and a wedged one.
The deferral is recorded in the run manifest, not as a failure, and the last
completed phase is kept.

The cluster is deferred: LKMABT continues with the next cluster in the file.
After the first pass it retries each deferred cluster once, with fresh provider
run IDs and fresh trigger commits. The deferred provider attempt deletes
only its own run-owned tenant namespace, without prompting, so its pending
workloads release their Kueue queue positions; it skips failure-artifact
collection. In a `both` run the sibling provider finalizes normally. If the
retry stays admission-pending too, that cluster fails with
`cluster remained admission-pending after one retry: <reason>`, where `<reason>`
carries the Kueue quota message or the PipelineRun condition.

Reports keep one row per cluster. Each row links to the cluster's run-logs
section and names the failure reason and the architectures the cluster actually
exercised, including any requested architecture it skipped. A cluster whose
first attempt deferred uses the retry result and carries
`first attempt deferred: <reason>` under its run-logs heading; the first
attempt's run directory and `run.log` remain available as additional links.
When the retry completes or fails normally, its namespace joins the aggregate
cleanup prompts; a retry that also defers is cleaned up without one.

### When the cluster GitHub App is not installed

GitHub preflight reads the App slug from `info.json` in the
`konflux-info/konflux-public-info` ConfigMap and checks that the App is
installed for `redhat-appstudio-qe`. This happens before the run touches the
tenant namespace or commits a trigger. A cluster can publish an App and still
not have it installed; those are two different facts, and only the second one
starts builds.

**Interactive run.** LKMABT reports
`GitHub App <slug> is not installed for organization redhat-appstudio-qe.` and
opens `https://github.com/apps/<slug>/installations/new` (the URL is always
printed as well). To complete the installation:

1. sign in to the browser as an organization owner and approve the App;
2. select the `redhat-appstudio-qe` organization;
3. grant access to `redhat-appstudio-qe/dr_test_mathwizz`, or to all
   repositories;
4. return to the terminal.

LKMABT rechecks every 10 seconds and continues automatically once the
installation appears; it prints
`GitHub App <slug> installation verified`. Progress, including the latest check
failure, is reported every 5 minutes. If the preflight timeout
(`--preflight-timeout`) expires, the run asks
`GitHub App <slug> is still not installed. Keep waiting?`. Answering `y`
continues the wait; declining fails that cluster at `preflight`. `Ctrl-C`
cancels the wait.

**Non-interactive run.** The cluster fails at `preflight` with the installation
URL. Other clusters in the file still run.

**Installed for selected repositories, but the fixture is missing.** The check
fails immediately at `preflight` with the installation settings URL. Add
`redhat-appstudio-qe/dr_test_mathwizz` to the installation or select all
repositories.

**Token cannot list installation repositories.** Preflight skips the check and
continues without user-facing output; the PaC wait during triggering remains the
backstop for a missing selection. This applies to configured tokens and browser
logins.

### During the run

- A component is accepted when every Buildah TaskRun for its triggered
  PipelineRun succeeds. LKMABT stops at the build stage: the PipelineRun's
  image-index, scans, tagging, and other post-build tasks are outside its scope
  and are not a completion gate. A failed Buildah TaskRun, or a PipelineRun that
  fails before its Buildah TaskRuns complete, is reported on the spot; LKMABT
  does not wait for the rest of the class to finish. Missing platform metadata
  in a Buildah TaskRun or a missing selected-platform output fails verification.
- A missing tenant namespace is requested through an OpenShift `ProjectRequest`,
  which binds the requester as project admin; if that API is unavailable or
  forbidden, LKMABT creates a bare `Namespace` with the ownership labels already
  set. When a ProjectRequest is used, cleanup labels are applied only after the
  created Namespace's UID matches the returned project, and the GitLab Secret
  access review runs before the Secret is written.
- LKMABT waits for PaC readiness and for the triggered PipelineRuns.
  `--preflight-timeout` bounds preflight and fixture setup. `--build-timeout`
  bounds each of triggering, the build wait, and build-output verification.
  `--trigger-timeout` bounds only the wait for a triggered PipelineRun to
  appear. A matching PipelineRun that fails before its Buildah TaskRuns
  complete, or a failed Buildah TaskRun, fails the wait immediately. A
  PipelineRun that fails only in a post-build task does not fail the build
  result.
- Owned stale tenant namespaces (`<tenant>` and `<tenant>-*`) are deleted
  automatically and logged. Namespaces with absent or different ownership
  labels are prompted individually and re-inspected before deletion. Declining
  fails that cluster at `preflight`.
- After success, LKMABT collects and verifies the run artifacts and records the
  tenant namespace for cleanup; after the last cluster, it prompts once for each
  recorded namespace. Declining keeps the namespace and the successful result.
  After failure, it saves the failure artifacts first, then records the
  namespace; if collection is incomplete, it skips the prompt and retains the
  namespace. Required snapshots retry transient network timeouts and API
  408/429/5xx responses twice with backoff before collection is declared
  incomplete; optional pod logs continue past individual container errors. A
  namespace whose prompt cannot read input (for example, non-interactive stdin)
  is retained and the reason is recorded in the console and the aggregate
  `run.log`; it never treats an unreadable prompt as a decline.
- Deletion waits after approval, in finalization and `cleanup`, are bounded to
  five minutes and re-strip deadlock-prone finalizers that controllers re-add. A
  namespace that still cannot be deleted fails with its name and the namespace
  controller's remaining resources or finalizers.
- The GitLab fixture Secret is `gitlab-build-secret`, labeled
  `app.konflux-ci.org/managed-by=lkmabt` and `app.konflux.org/run-id=<run-id>`.
  A Secret carrying both labels may be adopted and refreshed by the current
  run; a same-named Secret missing either label is never modified or deleted,
  so delete such a Secret manually before the run.
- Long waits are never silent: PaC onboarding and build waits report at most
  once per minute, naming pending components, PipelineRun ages, and Buildah
  counts. An admission-pending component reports `PipelineRunPending` and the
  Kueue quota message; after ten minutes it defers the cluster for one retry
  (see [Admission deferral](#admission-deferral)). A pending PipelineRun whose
  annotated PaC git-auth Secret is missing
  past two minutes fails with the Secret name and whether PaC created it: a
  lost created Secret hints at a concurrent run, while a PipelineRun without
  the PaC finalizer was never reconciled by the PaC watcher. An unreconciled
  run triggers one automatic retrigger of that component before the run fails.
  Other Secret read errors warn once.
- Two LKMABT processes cannot share one tenant namespace on the same API server.
  Each run and `cleanup` hold a non-blocking per-user file lock keyed by API
  server and namespace; a contended run fails before creating artifacts and
  names the current holder. The lock is per user and host, so it cannot
  arbitrate between machines or different users. On a single-provider run, use
  another `--tenant-namespace`; otherwise wait for that run. The lock has the
  patience of my nephew Doron from Israel: none, but it tells you exactly why.

---
## Cleaning up a namespace

```bash
lkmabt cleanup --cluster <name> --run-id <run-id>
```

`cleanup` is deliberately paranoid: it would rather leave a namespace behind
than delete one it cannot prove belongs to the run.

`cleanup` accepts the shared `--clusters-file` and `--state-dir` flags; it does
not accept `--env-file`.

- `--run-id` is the provider run ID: the final `<run-id>` segment of
  `github-run-<time>_<date>_<run-id>` or `gitlab-run-<time>_<date>_<run-id>`,
  not the aggregate report run ID. A `both` run creates a separate provider run
  ID and namespace for GitHub and GitLab, so clean up each namespace with its
  own provider run ID. Pass the provider ID, not the aggregate one—the
  ownership labels check, and they are not sentimental.
- `cleanup` authenticates the cluster with the same ladder as a run and checks
  that the connected API server matches the entry's `server` when one is
  configured.
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
  `Delete tenant namespace <name> owned by run <id> [y/N]:`; deletion happens
  only on an explicit yes.
- A cleanup whose prompt cannot read input (for example, non-interactive
  stdin) fails with an explicit confirmation error and never deletes silently.
- Deletion waits at most five minutes; on timeout it reports the namespace
  controller's remaining resources or finalizers.
- `cleanup` needs no provider credentials.

---
## Run logs and artifact retention

Runtime evidence is stored under `.lkmabt-runs/` by default; `--state-dir`
selects another root.

```text
.lkmabt-runs/
├── latest/                                          # Current aggregate run, a real directory
│   ├── .aggregate.json                              # Run ID and start time used for rotation
│   ├── <aggregate-run-name>-report.md               # Markdown report
│   ├── <aggregate-run-name>-report.html             # HTML report
│   ├── <aggregate-run-name>-run.log                 # Aggregate run log
│   └── <cluster-name>/                              # One directory per cluster that ran
│       ├── run.log                                  # Cluster-level log
│       └── <provider>-run-<hh:mm_d.m.yyyy>_<run-id>/
│           ├── manifest.json                        # Run identity, phase, fixture, triggers
│           ├── status.json                          # Compact phase and failure snapshot
│           ├── run.log                              # Provider run log
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
- No run is stored twice and `latest` is not a symlink. If you have ever
  debugged a symlink that pointed at yesterday's evidence, you know why.
- A run holds `.latest.lock` for its whole duration. A second run against the
  same state directory fails immediately with an actionable message instead of
  writing into the live run's `latest`; wait for the first run or pass a
  different `--state-dir`.
- If rotation fails for a filesystem reason, the new run is written to its
  permanent directory and a warning is logged.
- Pod log requests use the run start time as `SinceTime` and stop with the
  run's collection context; historical logs are not requested. Logs cover the
  tenant namespace plus `pipelines-as-code`, `tekton-pipelines`,
  `multi-platform-controller`, and `konflux-image-controller`.
- Report links use concrete paths and include only artifacts that exist.
- Each report has a run-data header (including mode, fixtures, and the
  aggregate run-log link), a per-cluster results table linking to each
  cluster's run-logs section, and one section per cluster that lists `run.log`
  first.
- Credential-like fields in JSON evidence are redacted before writing.
- Incomplete failure-artifact collection suppresses cleanup prompting and
  retains the namespace.

Nothing here is decorative. If a report link is missing, the file was not
collected, and `collection-report.json` will say so without *schmaltz*.
