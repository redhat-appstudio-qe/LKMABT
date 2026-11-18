# Local Multi-Architecture Build Testing Tool for Konflux Clusters

LKMABT validates multi-architecture builds on Konflux clusters. For every
cluster in the clusters file it:

1. checks that the cluster can run multi-platform builds;
2. creates or reuses the tenant namespace, Application, and Components;
3. appends one harmless comment to each fixture Dockerfile and commits it;
4. waits for the matching PipelineRuns to succeed;
5. verifies the pre-publication Buildah outputs cover `linux/amd64` and
   `linux/arm64`.

Fixed fixtures:

- [dr_test_mathwizz](https://github.com/redhat-appstudio-qe/dr_test_mathwizz)
- [dr_test_mathwizz_gl](https://gitlab.com/konflux-qe/dr_test_mathwizz_gl)

The harness never copies, creates, forks, renames, or deletes repositories. It
does not query registries, archives, pruning, backup services, or unrelated
resources.

## Prerequisites

### Local tools

| Tool | Needed for |
| --- | --- |
| Go 1.26 or newer | Building LKMABT (see `go.mod`). |
| `oc` | Every run. Each kubeconfig is proved with `oc whoami`; a readable file with an expired token is not enough. |
| `kflux` | `ocp` clusters whose kubeconfig is missing or rejected. If `kflux` is absent, LKMABT clones `https://gitlab.cee.redhat.com/hares/kflux.git` into `~/.local/share/lkmabt/kflux-src`, builds it, and installs `~/.local/bin/kflux`; that path needs `git`, `make`, Go, and network access to the internal GitLab. If kflux cannot refresh the kubeconfig, LKMABT falls back to `oc login`. |
| `ibmcloud` with the `kubernetes-service` plug-in | `type: roks` clusters. |
| `gh` | GitHub runs with an empty `GITHUB_TOKEN`. |
| `glab` | GitLab runs with an empty `GITLAB_BOT_TOKEN`. |

`type: roks` also needs network access to the configured master endpoint. VPE
endpoints require the VPC VPN.

### Accounts and access

**GitHub.** Your account needs push access to
`redhat-appstudio-qe/dr_test_mathwizz`; LKMABT commits the trigger there.
Verify it after `gh auth login`:

```bash
gh api repos/redhat-appstudio-qe/dr_test_mathwizz --jq .permissions.push
# true
```

Installing the cluster's Konflux GitHub App additionally requires an owner of
the `redhat-appstudio-qe` organization. Push access alone cannot approve an
installation.

**GitLab.** Your account needs at least Maintainer (access level 40) on
`konflux-qe/dr_test_mathwizz_gl`; the trigger commit lands on `main`. Verify it
after `glab auth login`:

```bash
glab api projects/konflux-qe%2Fdr_test_mathwizz_gl --jq .permissions.project_access.access_level
# 40 (Maintainer) or 50 (Owner)
```

**Cluster.** Your identity must be able to create and delete the tenant
namespace and its resources (`pipelineruns`, `taskruns`, `applications`,
`components`). GitLab runs also need `list`, `get`, `create`, and `update` on
Secrets in the tenant namespace, plus `delete` when the run acquires its own
GitLab token. For a namespace you do not administer:

```bash
oc adm policy add-role-to-user edit <user> -n mathwizz-test-gitlab
```

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

## Setup

### 1. Build LKMABT

```bash
go build -o bin/lkmabt ./cmd/lkmabt
```

### 2. Create the local configuration files

```bash
cp .lkmabt.env.example .lkmabt.env
cp .lkmabt-clusters.yaml.example .lkmabt-clusters.yaml
```

Both copies are gitignored. Keep them local. The environment parser reads
`KEY=VALUE` lines and does not execute the file.

### 3. List the clusters

Edit `.lkmabt-clusters.yaml`. A minimal entry for a kflux cluster:

```yaml
clusters:
  - name: ocp-p01
    kflux: ocp-p01
```

- `name` is unique and is the report directory name and the identity used by
  `--cluster` and in logs.
- `kflux` is the short kflux catalog name, not the API server host.
  `kflux-ocp-p01.7ayg.p1.openshiftapps.com` is catalog cluster `ocp-p01`. List
  catalog names with `kflux cluster list`; an unknown name fails with kflux's
  `unknown cluster` error.
- `kubeconfig` defaults to `~/.kube/config-<kflux>` and is verified on every
  run. kflux refreshes default paths; any entry still rejected falls back to
  `oc login`.
- `server` is optional; omit it to use the kubeconfig's own server.

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
never fall through to kflux or `oc login`. All available fields are documented
in `.lkmabt-clusters.yaml.example`.

### 4. Verify fixture access

Run the two commands from [Accounts and access](#accounts-and-access). In a
terminal, a configured GitHub token is checked before the first cluster; a
browser-login token is only exercised when the first trigger commit is
written. Checking now avoids a failed run.

## Running LKMABT

Run from the checkout root, or pass absolute paths. `--clusters-file` defaults
to `.lkmabt-clusters.yaml`; `--env-file` is optional and falls back to the
process environment.

```bash
./bin/lkmabt run github --env-file .lkmabt.env --clusters-file .lkmabt-clusters.yaml
./bin/lkmabt run gitlab --env-file .lkmabt.env
./bin/lkmabt run both --env-file .lkmabt.env
```

- `github` and `gitlab` test one fixture; `both` tests both.
- Clusters run one at a time in clusters-file order. Each cluster finishes or
  fails before the next one starts. `both` runs the two providers concurrently
  inside a cluster.
- Each run creates one tenant namespace per provider it tests:
  `mathwizz-test-github` for GitHub and `mathwizz-test-gitlab` for GitLab.
  Single-provider runs accept `--tenant-namespace`.
- The console prints `cluster <name>: started`, `cluster <name>: finished
  (<n> passed, <n> failed)`, one result line per provider and cluster, and the
  Markdown and HTML report paths.
- The run ID appears in the report title and in the
  `multi-run-<time>_<date>_<run-id>` directory name under `.lkmabt-runs/`. Use
  it to clean up a retained namespace later:

```bash
./bin/lkmabt cleanup --cluster <name> --run-id <run-id>
```

`cleanup` authenticates the cluster, finds the run by exact run ID, verifies
the saved artifacts and both ownership labels, and asks before deleting the
namespace.

### Authentication without provider tokens

`GITHUB_TOKEN` and `GITLAB_BOT_TOKEN` are optional. Leave either empty to
authenticate in a browser. A run is interactive when standard input is a
terminal; prompts and the browser flows are skipped otherwise.

**Empty `GITHUB_TOKEN`.** LKMABT prints
`github: authentication required; starting the gh browser login` and runs
`gh auth login --web` in an isolated `GH_CONFIG_DIR`. It requests the `repo`,
`read:org`, and `read:user` scopes. Complete the browser flow; the token is
kept in a private `lkmabt-auth-*` temporary directory (mode `0600`), reused for
every cluster, and deleted when the last cluster finishes, including on
failure. A non-interactive run without the token fails before any cluster work
with:

```text
GitHub authentication is required: set GITHUB_TOKEN or run in a terminal to use the gh browser login
```

**Empty `GITLAB_BOT_TOKEN`.** LKMABT prints
`gitlab: authentication required; choose Web in the glab login prompt` and runs
`glab auth login --hostname gitlab.com --use-keyring=false`. Choose **Web**.
Interactive login is supported only for `gitlab.com`; set `GITLAB_BOT_TOKEN`
for self-managed instances, custom ports, or HTTP endpoints. The token is
written into the tenant SCM Secret `gitlab-build-secret` so PaC can clone the
fixture. Finalization deletes that Secret; the token's temporary copy is
deleted when the run ends. A non-interactive run without the token fails with:

```text
GitLab authentication is required: set GITLAB_BOT_TOKEN or run in a terminal to use the glab browser login
```

**Configured `GITHUB_TOKEN`.** In a terminal, LKMABT checks before the first
cluster that the token can list App installations for `redhat-appstudio-qe`,
can push to the fixture, and can list repositories for App installations that
cover selected repositories. A token GitHub rejects is replaced by an existing
`gh` login when that login passes the same checks, and otherwise by the `gh`
browser login. Transient failures, rate limits, and timeouts abort the run
without replacing the token. Classic or OAuth tokens need `read:org`; add
`read:user` when an App is installed for selected repositories:

```bash
gh auth refresh -h github.com -s read:user
```

**Configured `GITLAB_BOT_TOKEN`.** The token is used as-is and is never
revoked. It needs `api` access to the fixture repository.

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
selection. A `gh` OAuth user token (`gho_`) cannot list installation
repositories, so GitHub logins keep this warning path.

### During the run

- Triggering appends one comment to each of the three Dockerfiles on the
  fixture's default branch and commits it. The fixtures are shared; your
  commits are visible to other users.
- LKMABT waits for PaC readiness and for the triggered PipelineRuns. The
  preflight, trigger, and build phases are bounded by `--preflight-timeout`,
  `--trigger-timeout`, and `--build-timeout` (defaults: 10m, 5m, and 60m). A
  matching PipelineRun that has already failed fails the wait immediately.
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

## Environment configuration

`.lkmabt.env` holds provider credentials only; cluster selection lives in
`.lkmabt-clusters.yaml`.

| Variable | Required | Notes |
| --- | --- | --- |
| `GITHUB_TOKEN` | No | Needs `repo` and `read:org`; `read:user` when an App is installed for selected repositories. Leave empty for the browser login. |
| `GITLAB_BOT_TOKEN` | No | Needs `api` access. Leave empty for the `glab` Web login on `gitlab.com`. Never revoked. |
| `GITLAB_API_URL` | No | Defaults to `https://gitlab.com/api/v4`. |

Values in the process environment take precedence over the file. Run-scoped
tokens are written to a private temporary directory as `0600` files and deleted
when the last cluster finishes.

## Artifact retention

Runtime evidence is stored under `.lkmabt-runs/`.

- Each aggregate run directory is named `multi-run-<hh:mm_d.m.yyyy>_<run-id>` and
  contains `report.md`, `report.html`, `run.log`, and one directory per
  cluster.
- Each cluster directory contains `run.log` and the unchanged provider run
  directories, for example `github-run-<hh:mm_d.m.yyyy>_<run-id>`.
- `latest/` resolves to the most recently started aggregate run directory from
  run creation. It is a relative symlink, exposes artifacts as they are
  written, and continues to point to that run through success or failure. The
  next run atomically changes the pointer; prior run-specific directories
  remain intact. If an older directory-form `latest` cannot be atomically
  exchanged on the current filesystem, the tool preserves it and refuses to
  switch.
- Each provider run stores `manifest.json` and `status.json`.
- Failed runs store `session/manifest.json`, workload snapshots for
  Applications, Components, PipelineRuns, TaskRuns, and Pods, plus
  `collection-report.json`.
- Available pod logs are stored under
  `logs/<namespace>/<pod>/<container>.log`. Log requests use the run start
  time as `SinceTime` and stop with the run's collection context; historical
  logs are not requested.
- Report links target concrete directories and only existing files.
- Credential-like fields in JSON evidence are redacted before writing.

Incomplete failure-artifact collection suppresses cleanup prompting and retains
the namespace.
