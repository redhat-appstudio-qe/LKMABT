# Local Multi-Architecture Build Testing Tool for Konflux Clusters

`LKMABT` runs the selected provider workflow (GitHub, GitLab, or both)
against every cluster listed in the clusters file. It reuses fixed
repositories, appends one harmless comment to each of their three Dockerfiles,
waits for matching successful PipelineRuns, verifies pre-publication
`linux/amd64` and `linux/arm64` build outputs, then stops.

Fixed fixtures:

- [dr_test_mathwizz](https://github.com/redhat-appstudio-qe/dr_test_mathwizz)
- [dr_test_mathwizz_gl](https://gitlab.com/konflux-qe/dr_test_mathwizz_gl)

The harness never copies, creates, forks, renames, or deletes repositories. It
does not query registries, archives, pruning, backup services, or unrelated
resources.

## Prerequisites

1. A clusters file (`.lkmabt-clusters.yaml` by default; override with
   `--clusters-file <path>`). Each entry has a `name` (required, unique; the
   report directory name and the `--cluster` identity), an optional `type`
   (`ocp` by default, or `roks`), an optional `kflux` catalog name (defaults to
   `name`; the `kflux cluster login -c` target for `ocp` and the prefix of the
   default kubeconfig path), an optional `kubeconfig` path (defaults to
   `~/.kube/config-<kflux>`), and an optional `server` API server URL (omitted
   means the kubeconfig's own server is used). `roks` entries also accept an
   optional `ibmcloud` cluster name or ID (defaults to `name`) and an optional
   `ibmcloud-endpoint` (`vpe` by default; `private` and `link` are also
   accepted). See `.lkmabt-clusters.yaml.example`.
2. `kflux` names are the short catalog names, not the API server hosts:
   `kflux-ocp-p01.7ayg.p1.openshiftapps.com` is catalog cluster `ocp-p01`, so
   that entry needs `kflux: ocp-p01`. List the catalog with `kflux cluster list`;
   an unknown name fails with kflux's own `unknown cluster` error.
3. Authentication follows a ladder per cluster: every kubeconfig is verified
   with `oc whoami` on every run, because a rejected token leaves the file
   readable. A rejected `roks` kubeconfig is refreshed through the IBM Cloud CLI;
   every other entry goes through kflux then `oc login`.
   ROKS runs reuse an active `ibmcloud` session, run `ibmcloud login --sso` when
   there is none, write the entry's kubeconfig with
   `ibmcloud ks cluster config --cluster <ibmcloud> --endpoint <ibmcloud-endpoint>`
   through `KUBECONFIG`, then prove the kubeconfig with `oc whoami`. A rejected
   kubeconfig fails that cluster at `authentication` with first-time-login
   instructions: open the cluster's OpenShift web console once from the IBM
   Cloud console, then re-run. ROKS entries never fall through to kflux or
   `oc login`. For other entries, one
   `kflux cluster login -c <kflux> [-c <kflux>...]` runs for every entry that
   fails verification and uses its default kubeconfig path, with kflux installed
   from https://gitlab.cee.redhat.com/hares/kflux.git into
   `~/.local/share/lkmabt/kflux-src` and `~/.local/bin/kflux` when missing from
   `PATH`; otherwise `oc login` runs with `KUBECONFIG` set to the entry's
   kubeconfig path.
4. `type: roks` needs the IBM Cloud CLI with the `kubernetes-service` plug-in on
   `PATH`, `oc` for the post-login check, and network access to the configured
   master endpoint (VPE endpoints require the VPC VPN). An existing ROKS
   kubeconfig needs only `oc` for its verification.
5. A kubeconfig the ladder cannot replace fails that cluster at `authentication`,
   before any cluster API work starts. For `roks` entries the failure keeps the
   refresh path and first-time-login instructions visible.
6. A ready `multi-platform-controller` deployment and `host-config` entries for
   `linux/amd64` and `linux/arm64`. Dynamic arm64 provisioning is sufficient;
   physical arm64 nodes are not required.
7. Every GitHub cluster must publish its Konflux GitHub App URL in
   `konflux-info/konflux-public-info` and that app must be installed for the
   fixture organization. GitHub runs verify the installation at `preflight`,
   before any cluster mutation. In an interactive session, a missing
   installation prints the installation steps, opens the app's installation
   page automatically, and waits for an organization administrator to approve
   it, rechecking every 10 seconds. Failed checks are retried, and progress,
   including the latest failure, is reported every 5 minutes. When the
   preflight timeout expires, the run asks whether to keep waiting. Declining,
   or a non-interactive run, fails that cluster with the installation URL. An
   app installed for selected repositories must include the fixture repository;
   the check fails immediately with the installation settings URL when it does
   not. When the token cannot list installation repositories, preflight logs a
   warning and continues; the trigger-stage PaC wait remains the backstop for a
   missing selection. In an interactive session, acquisition also verifies that
   a personal access token can list repositories for each non-suspended
   installation that covers selected repositories, so the preflight warning does
   not appear for a token that cannot perform it. A classic or OAuth token may
   need the `read:user` scope for that listing; add it with
   `gh auth refresh -h github.com -s read:user`. GitHub rejects a `gh` OAuth
   user token (`gho_`) for that endpoint even with the scope, so that check is
   skipped for `gh` logins and preflight keeps the warning path. A listing that
   remains unverifiable after the browser login still logs the preflight
   warning and continues.
8. GitLab runs write the PaC SCM Secret `gitlab-build-secret` in the tenant
   namespace (`mathwizz-test-gitlab` by default). The run identity needs
   `get`, `create`, and `update` on Secrets in that namespace, plus `delete`
   when the GitLab token is ephemeral (acquired at run time). Preflight asks
   the API with a `SelfSubjectAccessReview`, so missing permissions fail before
   any cluster mutation with guidance naming the namespace. Grant them as a
   namespace administrator, for example:
   `oc adm policy add-role-to-user edit <user> -n mathwizz-test-gitlab`.
   The Secret is labeled `app.konflux-ci.org/managed-by=lkmabt` and
   `app.konflux.org/run-id=<run-id>`. Both labels mark harness ownership: a
   Secret carrying them may be updated and adopted by the current run, which
   refreshes the run ID. Deletion additionally requires the run ID to equal the
   current run. A pre-existing `gitlab-build-secret` missing either label is
   never modified or deleted: the run fails after inspecting its labels with an
   actionable error naming the Secret, and a legacy unowned Secret must be
   deleted once after access is granted before the next GitLab run. An ephemeral
   token's Secret is removed during finalization; a configured
   `GITLAB_BOT_TOKEN` keeps the managed Secret for reuse.
9. Provider credentials come from `GITHUB_TOKEN` and `GITLAB_BOT_TOKEN` when
   set. Leave either empty to authenticate at run time: GitHub uses
   `gh auth login` (browser; requests `repo`, `read:org`, and `read:user`) and
   GitLab uses `glab auth login` (choose **Web** in its sign-in menu). Both
   `gh` and `glab` must be on `PATH` when the corresponding token is empty. A
   configured `GITHUB_TOKEN` is checked against the organization's App
   installations, the fixture repository's push permission, and the repositories
   of each non-suspended installation that covers selected repositories; in an
   interactive session, a token that cannot access them is replaced by the
   existing `gh` login when that login can, and otherwise falls back to the
   `gh` browser login. Transient, rate-limit, and timeout check failures abort
   the run without replacing the token. Interactive GitLab login is
   supported for `gitlab.com`; set `GITLAB_BOT_TOKEN` for self-managed
   instances, custom ports, or HTTP endpoints. Run-scoped tokens live in a
   private temporary directory, are reused for every cluster, and are deleted
   when the last cluster finishes, including on failure. The ephemeral GitLab
   token is also written to the tenant SCM Secret for PaC; that Secret is
   deleted during finalization. A configured `GITHUB_TOKEN` needs `read:org`,
   an owner who administers the fixture organization, push access to the
   fixture repository, and the `read:user` scope when an App is installed for
   selected repositories (`gh auth refresh -h github.com -s read:user`).
10. PaC onboarding must be complete for all three components, with current
   `.tekton` configuration available on each fixture's default branch. The
   harness waits for PaC readiness before writing trigger comments and does not
   merge onboarding changes.

## Commands

Run from this checkout's root, the directory holding `bin/`, `.lkmabt.env`, and
`.lkmabt-clusters.yaml`; the example paths are relative to it.

```bash
cd /path/to/konflux-test
./bin/lkmabt run github --env-file .lkmabt.env --clusters-file .lkmabt-clusters.yaml
./bin/lkmabt run gitlab --env-file .lkmabt.env
./bin/lkmabt run both --env-file .lkmabt.env
./bin/lkmabt cleanup --cluster <name> --run-id <run-id> --env-file .lkmabt.env
```

Clusters run one at a time in clusters-file order. `both` runs GitHub and
GitLab concurrently inside every cluster. The `--cluster-server` and
`--resume` flags no longer exist.

`cleanup` resolves `--cluster` through the clusters file (authenticating it
through the same ladder when needed), locates the run by exact run ID and
cluster inside the aggregate directories, then loads the manifest to select its
recorded namespace, verifies both exact ownership labels, and asks for
confirmation before deleting the namespace.

Each run creates one tenant namespace labeled with
`app.konflux-ci.org/managed-by=lkmabt` and
`app.konflux.org/run-id=<run-id>`.

Owned scoped namespaces (`<tenant>` and `<tenant>-*`) are deleted
automatically and logged. Unowned or label-mismatched namespaces are prompted
individually and re-inspected before deletion. A declined prompt fails only
that cluster at `preflight`.

After success, the harness prompts before deleting the current owned tenant
namespace. A declined prompt retains the namespace and successful result.
After failure, it saves and verifies required artifacts before prompting.

Long waits are never silent: PaC onboarding reports enabled/total and pending
components at most once per minute, and build waits name each component's
PipelineRun, age, and Buildah TaskRun count. A pending PipelineRun whose PaC
git-auth Secret has been missing for more than two minutes fails immediately
with the Secret name and a concurrent-run hint; a matched PipelineRun that has
already failed also fails the wait immediately.

Two LKMABT processes cannot share one tenant namespace on the same API server.
Each run and `cleanup` hold a non-blocking per-user file lock keyed by API
server and namespace; a contended run fails before creating artifacts and names
the current holder. Use another `--tenant-namespace` or wait for that run.

## Environment configuration

Copy `.lkmabt.env.example` to `.lkmabt.env`. Keep the copy local.
`GITHUB_TOKEN` and `GITLAB_BOT_TOKEN` are optional. Leave either empty to
authenticate at run time: GitHub opens the `gh` browser flow; GitLab opens the
`glab` login prompt (choose **Web**). A configured `GITHUB_TOKEN` is validated
for App-installation listing and fixture push access; when it fails for
credential reasons, an interactive run reuses the existing `gh` login if that
login passes the same checks, then falls back to the `gh` browser flow.
Transient check failures, rate limits, and timeouts fail the run instead of
replacing the token. Each
run-scoped token is written to a private temporary directory (`0600` file),
reused for every cluster, and deleted when the last cluster finishes, including
on failure. The ephemeral GitLab token is also placed in the tenant SCM Secret
for PaC and that Secret is deleted during finalization. Set `GITLAB_API_URL`
when needed. Cluster selection lives in `.lkmabt-clusters.yaml` (schema in
`.lkmabt-clusters.yaml.example`).

The parser reads `KEY=VALUE` lines. It does not execute the environment file.

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
