# Elmec soft fork

This repository is Elmec's soft fork of
[ionos-cloud/cluster-api-provider-proxmox](https://github.com/ionos-cloud/cluster-api-provider-proxmox).
It ships Elmec features as a small patch stack on top of upstream and publishes
its own clusterctl-consumable releases, while staying a drop-in replacement
(same API groups, same CRDs) and keeping every feature upstreamable.

## Branch model

- **`main`** — pure fast-forward mirror of upstream `main`. Never carries Elmec
  commits. Upstream-bound PR branches are cut from here. Synced daily at
  05:17 UTC by `elmec-sync.yml` (fails loudly if it cannot fast-forward).
  The sync does **not** copy upstream tags — push them manually:
  `git fetch upstream --tags && git push origin --tags`.
- **`elmec-main`** (default branch) — the patch stack: upstream `main` plus, in
  order, the Elmec CI commits (`elmec-sync.yml`, `elmec-image.yml`,
  `elmec-release.yml`) and the Elmec feature commits. Kept linear, no merge
  commits.

Feature branches that back an open upstream PR (e.g. `feat/failure-domains`,
upstream PR #720) must never be rebased onto `elmec-main` — that would inject
the CI commits into the upstream PR diff. Land them on `elmec-main` with
`git cherry-pick` instead and leave the PR branch untouched.

## Releases

Elmec releases are tagged `vX.Y.Z-elmec.N` on `elmec-main`. The prerelease
suffix keeps semver ordering sane: `v0.9.0 < v0.9.1-elmec.1 < v0.9.1`, so an
Elmec release always sorts after the upstream release it is based on and before
the next upstream one. Fork-only fixes between upstream releases bump the
counter (`-elmec.2`, `-elmec.3`, …).

Cutting a release:

```bash
git tag -a v0.9.1-elmec.1 -m "failure domains on top of upstream v0.9.x" elmec-main
git push origin v0.9.1-elmec.1
```

The tag push triggers `elmec-release.yml`, which:

1. builds and pushes `ghcr.io/elmecoss/capmox:<tag>` (linux/amd64);
2. generates `infrastructure-components.yaml` (image rewritten to the Elmec
   registry via kustomize `old=new` rename), `metadata.yaml` and the
   `cluster-template*.yaml` files;
3. publishes a **non-draft, prerelease** GitHub release with those assets
   (drafts are invisible to clusterctl; the prerelease flag keeps it off
   GitHub "latest"). Release notes list only the patch stack.

The tag also fires the still-active upstream `container-image.yaml`, which
pushes a duplicate image under `ghcr.io/elmecoss/cluster-api-provider-proxmox:<tag>`.
Harmless — ignore it (or disable that workflow, an admin operation).
Upstream's `release.yml` is disabled on the fork and must stay disabled: it
fires on any `v*` tag (including mirrored upstream tags) and only creates
draft releases.

## Consuming with clusterctl

On the management machine, add to `~/.cluster-api/clusterctl.yaml`:

```yaml
providers:
  - name: "proxmox" # overrides the built-in ionoscloud entry → drop-in
    url: "https://github.com/ElmecOSS/cluster-api-provider-proxmox/releases/v0.9.1-elmec.1/infrastructure-components.yaml"
    type: "InfrastructureProvider"
```

Then `clusterctl init --infrastructure proxmox:v0.9.1-elmec.1 ...`. Always pin
the exact version: clusterctl excludes prereleases when resolving `latest`.
On busy machines export `GITHUB_TOKEN` to avoid the unauthenticated GitHub API
rate limit.

## Maintenance: rebasing onto a new upstream release

When upstream tags `vX.Y.Z`:

```bash
git fetch upstream --tags
old_base=$(git merge-base elmec-main upstream/main)
git rebase --onto vX.Y.Z "$old_base" elmec-main --empty=drop
git push --force-with-lease origin elmec-main   # the only case elmec-main is force-pushed
git tag -a vX.Y.Z-elmec.1 -m "..." elmec-main
git push origin vX.Y.Z-elmec.1
```

Notes:

- `--empty=drop` silently drops patch stack commits that upstream merged
  as-is. If an Elmec PR is **squash**-merged upstream, its commits will
  conflict instead — resolve with `git rebase --skip` (the content is already
  upstream).
- Before tagging, check `metadata.yaml` maps the new minor to a contract
  version; add the entry as a patch-stack commit if missing.
- After the rebase, push the new upstream tags to the fork too (see above).
