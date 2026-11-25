# GitHub Actions

This guide sets up a repository which keeps one or more Cumulocity tenants in sync with a manifest:

- pull requests validate the manifest and preview the changes (dry run),
- pushes to `main` apply the manifest, after an approval,
- a scheduled job reports drift, when someone changed the tenant outside of the manifest.

[setup-go-c8y-cli](https://github.com/reubenmiller/setup-go-c8y-cli) installs go-c8y-cli and the tenant-sync extension, so the steps run `c8y tenant-sync` like you do locally. Every run writes its report to the job's step summary, see [Run reports](../README.md#run-reports).

## Quick start

```yaml
# .github/workflows/tenant-sync.yml
name: tenant-sync
on:
  push:
    branches: [main]

jobs:
  apply:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: reubenmiller/setup-go-c8y-cli@main
        with:
          showTenant: false
          extensions: |
            reubenmiller/c8y-tenant-sync@v0.10.5
      - run: c8y tenant-sync run -f tenant.yaml </dev/null
        env:
          C8Y_HOST: ${{ vars.C8Y_HOST }}
          C8Y_USER: ${{ secrets.C8Y_USER }}
          C8Y_PASSWORD: ${{ secrets.C8Y_PASSWORD }}
```

- Pin the extension to a release tag, so a new tenant-sync release doesn't change what your workflow does. Update the tag in a pull request, where the dry run shows the effect. Binary extensions need go-c8y-cli v2.57.0 or later, which the default `version: latest` gives you.
- `</dev/null` closes stdin. go-c8y-cli reads piped input from stdin, and on GitHub runners stdin is not a terminal.
- The `C8Y_*` variables form the session, no `c8y sessions` file is needed. See [Session and authentication](../README.md#session-and-authentication) for the other variables, such as `C8Y_TOKEN`.

## Store the credentials in environments

Create a [GitHub environment](https://docs.github.com/en/actions/deployment/targeting-different-environments/using-environments-for-deployment) per tenant, e.g. `staging` and `production`, with:

| Name | Kind | Value |
|------|------|-------|
| `C8Y_HOST` | variable | `https://example.cumulocity.com` |
| `C8Y_USER` | secret | A user with the permissions the manifest needs, e.g. `t12345/ci-tenant-sync` |
| `C8Y_PASSWORD` | secret | Its password |
| `SOPS_AGE_KEY` | secret | Only for [encrypted manifests](#encrypted-manifests) |

Add required reviewers to the `production` environment, so a run against it waits for an approval. Use a dedicated user for CI rather than a personal account, so you can rotate or revoke it on its own.

## Full workflow

The workflow below validates and previews on pull requests, applies on `main`, and checks for drift every night. It uses one manifest per tenant (`tenants/<environment>.yaml`), which typically [include](../README.md#composing-manifests) a shared base manifest.

```yaml
# .github/workflows/tenant-sync.yml
name: tenant-sync
on:
  pull_request:
  push:
    branches: [main]
  schedule:
    - cron: '0 3 * * *'
  workflow_dispatch:

permissions:
  contents: read

jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: reubenmiller/setup-go-c8y-cli@main
        with:
          showVersion: false
          showTenant: false
          extensions: reubenmiller/c8y-tenant-sync@v0.10.5
      # No tenant access needed, so this also runs for pull requests from forks
      - run: |
          for manifest in tenants/*.yaml; do
            c8y tenant-sync validate "$manifest" --check-sources </dev/null
          done
        env:
          GITHUB_TOKEN: ${{ github.token }}

  sync:
    needs: validate
    # Secrets are not available to pull requests from forks
    if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository
    strategy:
      fail-fast: false
      matrix:
        tenant: [staging, production]
    runs-on: ubuntu-latest
    environment: ${{ matrix.tenant }}
    # Never run two syncs against the same tenant at the same time
    concurrency:
      group: tenant-sync-${{ matrix.tenant }}
      cancel-in-progress: false
    env:
      C8Y_HOST: ${{ vars.C8Y_HOST }}
      C8Y_USER: ${{ secrets.C8Y_USER }}
      C8Y_PASSWORD: ${{ secrets.C8Y_PASSWORD }}
      GITHUB_TOKEN: ${{ github.token }}
    steps:
      - uses: actions/checkout@v6
      - uses: reubenmiller/setup-go-c8y-cli@main
        with:
          showVersion: false
          extensions: reubenmiller/c8y-tenant-sync@v0.10.5

      - name: Preview changes
        if: github.event_name == 'pull_request'
        run: c8y tenant-sync run -f tenants/${{ matrix.tenant }}.yaml --dry-run --report report.json </dev/null

      - name: Check for drift
        if: github.event_name == 'schedule'
        run: c8y tenant-sync run -f tenants/${{ matrix.tenant }}.yaml --dry-run --fail-on-change --report report.json </dev/null

      - name: Apply
        if: github.event_name == 'push' || github.event_name == 'workflow_dispatch'
        run: c8y tenant-sync run -f tenants/${{ matrix.tenant }}.yaml --report report.json </dev/null

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: tenant-sync-report-${{ matrix.tenant }}
          path: report.json
          if-no-files-found: ignore
```

Notes:

- With `environment: production` and required reviewers, the pull request preview and the nightly drift check wait for an approval too. If you only want approvals before applying, use a second environment for read-only runs (e.g. `production-readonly`, with credentials of a user who can only read) and pick it with `environment: ${{ github.event_name == 'push' && matrix.tenant || format('{0}-readonly', matrix.tenant) }}`.
- `GITHUB_TOKEN` is only needed when the manifest uses [GitHub sources](../README.md#sources) or includes manifests from private repositories. The default `github.token` only reads the repository the workflow runs in; for releases of other private repositories, use a token with access to them.
- `--fail-on-change` makes the drift check fail when the tenant differs from the manifest, so GitHub notifies you. The step summary lists the items which differ. Executed hook commands are not counted as changes.
- To apply a manifest to several tenants from one job instead of a matrix, use [target tenants](../README.md#target-tenants) (`--target`, `--target-selector` or `--all-children`).

## Encrypted manifests

A manifest encrypted with [SOPS](sops.md) needs the `sops` executable and the private key. Create an age key for CI, add its public key to `.sops.yaml` (and run `sops updatekeys`), and store the private key (the `AGE-SECRET-KEY-...` line) as the `SOPS_AGE_KEY` secret of each environment. Then install sops before the tenant-sync steps:

```yaml
      - name: Install sops
        run: |
          sudo curl -fsSL -o /usr/local/bin/sops https://github.com/getsops/sops/releases/download/v3.9.4/sops-v3.9.4.linux.amd64
          sudo chmod +x /usr/local/bin/sops
      - run: c8y tenant-sync run -f tenants/${{ matrix.tenant }}.yaml --secrets-encryption sops </dev/null
        env:
          SOPS_AGE_KEY: ${{ secrets.SOPS_AGE_KEY }}
```

`validate` decrypts the manifest too, so the `validate` job then needs the key as well, and no longer runs for pull requests from forks.

`--secrets-encryption sops` makes sure CI never writes a plain secrets file. If a run generates new values (`${generate:NAME}`), the secrets file only changes in the CI workspace and the values are lost when the job ends. Generate them once locally and commit the encrypted secrets file, so CI only reuses them. See [Use SOPS in CI](sops.md#use-sops-in-ci).

## Without go-c8y-cli

The tenant-sync binary also runs on its own with the same `C8Y_*` variables. Download it from the [releases](https://github.com/reubenmiller/c8y-tenant-sync/releases), or build it with Go:

```yaml
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - run: go run github.com/reubenmiller/c8y-tenant-sync@v0.10.5 run -f tenant.yaml
        env:
          C8Y_HOST: ${{ vars.C8Y_HOST }}
          C8Y_USER: ${{ secrets.C8Y_USER }}
          C8Y_PASSWORD: ${{ secrets.C8Y_PASSWORD }}
```

## Troubleshooting

| Problem | Cause and fix |
|---------|---------------|
| `unknown command "tenant-sync" for "c8y"` | The extension isn't installed, or the go-c8y-cli version is older than v2.57.0. Check the `extensions` input of setup-go-c8y-cli and its log of installed extensions. |
| `API rate limit exceeded` while installing the extension or resolving GitHub sources | Pass a token: setup-go-c8y-cli uses `github.token` by default (`token` input); tenant-sync reads `GITHUB_TOKEN` or `GH_TOKEN`. |
| The job hangs or c8y reports unexpected piped input | Add `</dev/null` to the `c8y` command, or `exec 0</dev/null` at the start of the step. |
| `401 Unauthorized` | Check `C8Y_HOST`, `C8Y_USER` and `C8Y_PASSWORD` in the environment the job uses. Users of some tenants need the tenant ID as a prefix: `t12345/user`. |
