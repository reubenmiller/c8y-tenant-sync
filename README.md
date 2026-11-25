# c8y-tenant-sync

A GitOps-style CLI tool for keeping Cumulocity IoT tenants in sync with a declarative manifest. Describe the desired tenant state in a YAML file — software, firmware and configuration repositories, tenant options, feature toggles, retention rules, application subscriptions, users and user groups, SmartREST 2.0 templates, device profiles — and apply it idempotently to one or more tenants.

It is a [go-c8y-cli](https://goc8ycli.netlify.app/) binary extension, so it runs as `c8y tenant-sync` and uses your active c8y session. It started as the [tenant-sync example](https://github.com/reubenmiller/go-c8y/tree/v2/examples/tenant-sync) of the go-c8y SDK, which generalises the software-uploader example: the same filename parsing and idempotent upload logic, extended to firmware and configuration, with artifact sources that can be local files **or GitHub releases**, all driven from a single configuration file.

## Concept

```
                     ┌──────────────────┐
  manifest.yaml ───▶ │   tenant-sync    │ ───▶ Tenant A (C8Y_* env)
                     │                  │
  sources:           │  resolve sources │      • software repository
   • local files     │  parse artifacts │      • firmware repository
   • GitHub releases │  diff + apply    │      • configuration repository
   • external URLs   │  (idempotent)    │      • device profiles
                     └──────────────────┘      • tenant options
                                               • feature toggles
                                               • application subscriptions
                                               • users & user groups
                                               • SmartREST 2.0 templates
                                               • retention rules
```

Because every operation is idempotent (get-or-create / upsert), the same manifest can be applied repeatedly and to multiple tenants — keep it in git, run it from CI, and your tenants stay in sync with the source of truth.

## Features

- 📄 **Declarative manifest**: one YAML file describes the desired tenant state
- 🔁 **Idempotent**: safe to run repeatedly; unchanged items are left alone
- 📦 **Software repository sync**: smart filename parsing for .deb, .rpm, .apk, .ipk, archives, etc. (same parser as the go-c8y software-uploader example), grouped by name + architecture + type
- 🧱 **Firmware repository sync**: understands OS image naming from common build systems
  - **Yocto/OpenEmbedded**: `core-image-minimal-raspberrypi4-64-20240115103000.rootfs.wic.bz2`
  - **Buildroot**: `*.img`, `*.img.gz`, `sdcard.img`, `*.ext4`, `*.squashfs`
  - **Rugix Bakery**: `*.img.xz`, `*.rugixb`
  - **Update frameworks**: `*.swu` (SWUpdate), `*.raucb` (RAUC), `*.mender`
  - **Custom types**: any extension, with a `versionPattern` regex for custom naming schemes
- ⚙️ **Configuration repository sync**: upload configuration files with type and device type filters
- 🐙 **GitHub sources**: point any entry at a GitHub repository — assets are pulled from releases (`latest`, a specific tag, or `all`), with the release tag as the version fallback; `linkOnly` mode references download URLs without re-hosting binaries (the default for firmware)
- 📦 **Cloudsmith sources**: pull packages from Cloudsmith repositories, such as the public thin-edge.io repositories (`thinedge/tedge-release`, `thinedge/tedge-main`, `thinedge/community`), with the name, version, architecture and software type taken from the package metadata; versions are installed by the device's package manager, or link or upload the package
- 🧩 **Device profiles**: declare firmware/software/configuration bundles; binary URLs are resolved from the tenant automatically
- 🎛️ **Tenant options & feature toggles**: set options, enable/disable features, subscribe/unsubscribe applications
- 🗑️ **Retention rules**: ensure retention rules exist, matched by their selector (dataType/fragmentType/type/source) with the retention period updated when it differs
- 📜 **Trusted certificates**: upload device CA certificates (PEM/DER) into the trust store — matched by fingerprint, with name/status/auto-registration updated when they differ — plus certificate revocation lists from CSV files, and enabling or disabling the tenant's Cumulocity CA
- 🗂️ **Device groups**: create the same tree of device groups (regions, sites, production lines, customers) in every tenant, with descriptions and custom fragments
- 👥 **Users & user groups**: create user groups with roles, create users and assign them to groups — additively and idempotently
- 🏭 **Inventory roles**: define inventory roles (permissions on alarms, operations, measurements, ...) and give users these roles on specific device groups, for least-privilege access per site
- 📡 **SmartREST 2.0 templates**: sync template collections from JSON files exported by the platform — matched by external identity, updated only when the templates differ (order-insensitive)
- 🧬 **Digital Twin Manager definitions**: create and update property and asset definitions (plus measurement, event and alarm definitions) inline or from model files, in dependency order, with optional pruning of definitions the manifest no longer declares
- 🧪 **Smart functions**: deploy Data Preparation rules from package folders or exported `.tar.gz` archives, only when the code, configuration or tests changed, and report deployment errors
- 🏢 **Target tenants**: apply one manifest to all child tenants, an explicit list, or tenants matching a selector — with per-tenant credentials resolved automatically
- 🧷 **Composable manifests**: `include` shared base files (e.g. company-wide retention rules, user groups, certificates) from per-environment or per-customer manifests
- 🏗️ **Child tenant provisioning**: declare child tenants (domain, company, admin, contact, custom properties, subscriptions) and create them from the parent tenant, then configure them with the rest of the manifest in the same run
- 🔎 **Dry-run**: preview every change before applying
- 🧾 **Run reports**: JSON and Markdown reports of what a run changed per tenant, for CI pipelines and audits, plus `--fail-on-change` for drift checks
- 🔐 **Env var expansion**: `${VAR}` references in the manifest (e.g. GitHub tokens); `$$` is a literal `$`
- 🔑 **Secret references**: `valueFrom`, `passwordFrom` and `tokenFrom` take secrets from environment variables or generated values, as readable fields which sops leaves unencrypted
- 🎲 **Generated values**: `${generate:NAME}` creates random values (e.g. initial passwords), stores them in a secrets file (sops encrypted when possible) and masks them in all output
- 🔒 **SOPS**: manifests encrypted with [SOPS](https://github.com/getsops/sops) (age, PGP, cloud KMS, Vault) are decrypted automatically
- 📤 **Export**: create a manifest from an existing tenant (tenant options, feature toggles, retention rules, the Cumulocity CA, subscribed microservices, the device group tree, inventory roles, Digital Twin Manager definitions and smart functions)

## Installation

Install the extension with go-c8y-cli (requires a go-c8y-cli version with binary extension support). The binary for your platform is downloaded from the latest GitHub release:

```bash
c8y extensions install reubenmiller/c8y-tenant-sync
```

Update it later with:

```bash
c8y extensions update tenant-sync
```

## Usage

The CLI follows the usual init → validate → run workflow:

```bash
# 1. Create a manifest (minimal skeleton; use --full for the annotated example)
c8y tenant-sync init

# 2. Validate it (schema only; --check-sources also resolves every source)
c8y tenant-sync validate tenant.yaml
c8y tenant-sync validate tenant.yaml --check-sources

# 3. Preview, then apply
c8y tenant-sync run -f tenant.yaml --dry-run
c8y tenant-sync run -f tenant.yaml
```

`run` uses the active go-c8y-cli session (`init` and `validate` work offline). c8y global flags placed **before** the extension name are applied by c8y, so you can pick another session or turn on dry run for a single call:

```bash
# Run against another session in dry run mode
c8y --session customer1.json --dry tenant-sync run -f tenant.yaml
```

Flags placed after the extension name (such as `-f`, `--force`, `--verbose` and `--debug`) are passed to tenant-sync unchanged.

More examples:

```bash
# Only sync specific sections
c8y tenant-sync run -f tenant.yaml --only firmware,deviceProfiles

# Merge several manifests in order (later files win; see "Composing manifests")
c8y tenant-sync run -f base.yaml -f prod.yaml --dry-run

# Replace existing version binaries (instead of skipping existing versions)
c8y tenant-sync run -f tenant.yaml --force

# Sync multiple tenants from the same manifest (one session per tenant) ...
c8y --session tenant-a.json tenant-sync run -f tenant.yaml
c8y --session tenant-b.json tenant-sync run -f tenant.yaml

# ... or apply to other tenants in one run from the parent tenant
# (see "Target tenants" below)
c8y tenant-sync run -f tenant.yaml --all-children
```

Tab completion (commands, flags, `--only` sections and manifest files) works through c8y's own completion.

Like the c8y built-in commands, every command shows its examples with `--examples`:

```bash
c8y tenant-sync init --examples
c8y tenant-sync run --examples
```

### Session and authentication

c8y passes the active session to the extension as environment variables. tenant-sync reads:

| Variable | Use |
|----------|-----|
| `C8Y_HOST` / `C8Y_BASEURL` / `C8Y_URL`, `C8Y_TENANT`, `C8Y_USER` / `C8Y_USERNAME` | Tenant to connect to |
| `C8Y_TOKEN` | Bearer token of the session (used first) |
| `C8Y_PASSWORD` | Basic auth password, when set in your environment |
| `C8Y_HEADER_AUTHORIZATION` | Authorization header of the session, used when there is no token or password (c8y does not pass the session password to extensions) |
| `C8Y_SETTINGS_DEFAULTS_DRY` | Set by `c8y --dry`; turns on dry run |
| `C8Y_SETTINGS_DEFAULTS_INSECURE` | Set by `c8y --insecure`; skips TLS verification |

The binary also runs on its own (for example in CI) with the same `C8Y_*` variables, see [CI/CD Integration](#cicd-integration).

### Commands

| Command | Description |
|---------|-------------|
| `init [path]` | Create a new manifest (default: `tenant.yaml`) and the `packages` folder next to it, which the manifest syncs software from (skipped while empty). `--full` writes the fully annotated example instead, `--force` overwrites |
| `validate [path]` | Validate the manifest schema (strict — unknown fields are errors). `--check-sources` also resolves local paths and GitHub releases |
| `run [path]` | Apply the manifest to the tenant (alias: `apply`) |
| `schema` | Generate the JSON schema for the manifest (`-o file` to write to a file) |
| `export` | Create a manifest from the current state of the tenant (`-o file` to write to a file), see [Exporting a tenant](#exporting-a-tenant) |

### Run flags

| Flag | Default | Description |
|------|---------|-------------|
| `-f`, `--manifest` | `tenant.yaml` | Path to the manifest file (also accepted as a positional argument) |
| `--dry-run`, `--dry` | | Preview changes without applying them |
| `--only` | *(all)* | Comma-separated sections to apply: `tenants`, `tenantOptions`, `features`, `retentionRules`, `certificateAuthority`, `trustedCertificates`, `certificateRevocationLists`, `applications`, `deviceGroups`, `userGroups`, `inventoryRoles`, `users`, `software`, `firmware`, `configuration`, `smartrestTemplates`, `digitalTwin`, `smartFunctions`, `deviceProfiles`, `commands` |
| `--force` | | Replace existing version binaries / configuration files |
| `--concurrency` | `5` | Concurrent software version uploads (1–20) |
| `--target` | | Apply to a tenant referenced by ID or domain; repeatable or comma-separated (overrides the manifest `targets` section) |
| `--all-children` | | Apply to all child tenants of the current tenant (overrides the manifest `targets` section) |
| `--target-selector` | | Apply to tenants matching criteria, e.g. `domain=*.example.com,company=ACME` (overrides the manifest `targets` section) |
| `--include-current` | | Also include the current tenant when other target flags are used |
| `--secrets-file` | `<manifest>.secrets.yaml` | File which stores the values of `${generate:NAME}` references, see [Generated values](#generated-values) |
| `--secrets-encryption` | `auto` | Encrypt the secrets file with sops: `auto` (when sops and a `.sops.yaml` are found), `sops` (required) or `none` |
| `--credentials-mode` | | How credentials for other tenants are obtained: `serviceUser`, `sessions` or `tenantAdmin` (overrides the manifest) |
| `--report` | | Write a report of the run to a file (`.json` or `.md`); repeatable. `github` appends the Markdown report to `$GITHUB_STEP_SUMMARY`. See [Run reports](#run-reports) |
| `--report-format` | *(from the extension)* | Format of the `--report` files: `json` or `markdown` |
| `--github-summary` | `true` | Inside GitHub Actions, append the Markdown report to the step summary; `--github-summary=false` turns it off. No effect elsewhere |
| `--fail-on-change` | | Exit with an error when any item was created, updated or planned (drift check) |
| `--verbose` | | Detailed logging |
| `--debug` | | Verbose logging + HTTP request/response details |

Sections are applied in a fixed order — child tenants (once, before the targets are resolved), tenant options, features, retention rules, Cumulocity CA, trusted certificates, certificate revocation lists, applications, user groups, users, software, firmware, configuration, SmartREST templates, device profiles, then commands — so users can be assigned to groups created in the same run, profiles can reference repository items synced in the same run, and custom commands can build on everything the manifest declares.

### Run reports

`--report FILE` writes a record of the run: what changed per tenant, what failed, and the counts per action. The format comes from the file extension (`.json` or `.md`), or from `--report-format json|markdown`, which applies to all `--report` files. Repeat the flag to write both formats in one run:

```sh
c8y tenant-sync run -f tenant.yaml --report report.json --report report.md
```

- The report is written in dry-run mode and also when items fail or the run is aborted (e.g. by a failing pre hook). It is not written when the run stops before syncing, e.g. for an invalid manifest.
- Secrets are masked (`***`) in every field, like in the console output.
- Missing parent directories are created. A report which cannot be written is reported and makes a successful run fail; it never replaces the error of a failed run.
- Inside GitHub Actions (`GITHUB_ACTIONS=true` and `GITHUB_STEP_SUMMARY` set), the Markdown report is appended to the job's step summary automatically, also in dry-run and when the run fails. Turn it off with `--github-summary=false`. The step summary is a file owned by the runner, so the report is only ever appended, and a failure to write it prints a warning without failing an otherwise successful run.
- `--report github` requests the step summary explicitly (also with `--github-summary=false`). Then a write failure fails the run like any other `--report` file. The summary is written at most once per run, also when both apply. Outside GitHub Actions, where `GITHUB_STEP_SUMMARY` is not set, `--report github` is skipped with a notice.
- `--fail-on-change` exits with `changes detected: N item(s) created, updated or planned` when the run changed (or, in dry-run, would change) anything. Combined with `--dry-run` it is a drift check: the job fails when the tenant no longer matches the manifest. Executed hook commands do not count as changes.

The JSON report:

```json
{
  "toolVersion": "1.4.0",
  "manifest": "tenant.yaml",
  "host": "https://example.cumulocity.com",
  "startTime": "2026-11-19T21:43:28.296Z",
  "durationSeconds": 3.2,
  "dryRun": false,
  "summary": {
    "status": "failed",
    "total": 3,
    "changes": 1,
    "counts": { "created": 1, "failed": 1, "unchanged": 1 }
  },
  "results": [
    { "tenant": "t100", "section": "tenantOptions", "item": "configuration/my.setting", "action": "created" },
    { "tenant": "t100", "section": "retentionRules", "item": "MEASUREMENT/*/*/*", "action": "unchanged" },
    { "tenant": "t200", "section": "users", "item": "svc@example.com", "action": "failed", "detail": "create user", "error": "..." }
  ]
}
```

| Field | Description |
|-------|-------------|
| `toolVersion` | Version of tenant-sync |
| `manifest` | Manifest path as given |
| `host` | Cumulocity host of the session (omitted in dry-run without a session) |
| `startTime`, `durationSeconds` | When the sync started (UTC) and how long it took |
| `dryRun` | `true` for a dry run (all changes are `planned`) |
| `error` | Why the run was aborted, e.g. a failing pre hook (omitted otherwise) |
| `summary.status` | `success`, or `failed` when an item failed or the run was aborted |
| `summary.total`, `summary.changes`, `summary.counts` | Number of items, of created/updated/planned items, and of items per action (`created`, `updated`, `unchanged`, `skipped`, `executed`, `planned`, `failed`) |
| `results[].tenant` | Target tenant; only set when the run covers tenants other than the current one ([target tenants](#target-tenants)) |
| `results[].section`, `item`, `action`, `detail`, `error` | The item as shown in the console output; `detail` and `error` are omitted when empty |

The Markdown report has a heading with the status, the run details, a table of counts, and a table of the changed and failed items. Unchanged items are collapsed in a `<details>` block. With several tenants the items are grouped under a heading per tenant.

In GitHub Actions the step summary is written automatically. Upload the JSON report as an artifact, also when the sync fails:

```yaml
      - run: go run github.com/reubenmiller/c8y-tenant-sync@latest run -f tenant.yaml --report tenant-sync-report.json
        env:
          C8Y_BASEURL: ${{ secrets.C8Y_BASEURL }}
          C8Y_TENANT: ${{ secrets.C8Y_TENANT }}
          C8Y_USERNAME: ${{ secrets.C8Y_USERNAME }}
          C8Y_PASSWORD: ${{ secrets.C8Y_PASSWORD }}
      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: tenant-sync-report
          path: tenant-sync-report.json
```

### Secrets with SOPS

Manifests can be encrypted with [SOPS](https://github.com/getsops/sops), so secrets such as user passwords, tenant option values or GitHub tokens can be kept in git. tenant-sync detects encrypted manifests (by the `sops` section which SOPS adds) and decrypts them with the `sops` executable before applying them, so every key type SOPS supports works with your existing SOPS setup (for example `SOPS_AGE_KEY_FILE` for age keys). Plain manifests don't need `sops`.

Encrypt only the secret fields, so the rest of the manifest stays readable and reviewable. For example with a `.sops.yaml` next to the manifest:

```yaml
# .sops.yaml
creation_rules:
  - path_regex: tenant\.yaml$
    encrypted_regex: ^(password|value|token|content)$
    age: age1...your-public-key
```

```bash
sops --encrypt --in-place tenant.yaml   # encrypt (or edit it with: sops tenant.yaml)
c8y tenant-sync validate tenant.yaml    # decrypts with your key
c8y tenant-sync run -f tenant.yaml
```

- `sops` must be installed (or set `TENANT_SYNC_SOPS` to its path) wherever an encrypted manifest is validated or applied, including CI.
- `${VAR}` references are expanded after decryption, so both can be combined.
- Editors with the manifest schema accept the `sops` section.
- Use [secret references](#secret-references) (`valueFrom`, `passwordFrom`, `tokenFrom`) for values from the environment or generated values: they are separate fields, so they stay readable while literal secrets are encrypted.

See the **[SOPS guide](docs/sops.md)** for the full setup: installing sops and age, creating a key, `.sops.yaml` rules for manifests and secrets files, reading generated values, sharing with a team, CI, and troubleshooting.

### Secret references

Fields which hold secrets can reference their value instead of containing it:

| Field | Instead of | References |
|-------|------------|------------|
| `tenantOptions[].valueFrom` | `value` | `env`, `generate` (and `application`, `device`) |
| `users[].passwordFrom` | `password` | `env`, `generate` |
| `tenants[].adminPasswordFrom` | `adminPass` | `env`, `generate` |
| `github.tokenFrom` (any GitHub source) | `github.token` | `env`, `generate` |

```yaml
tenantOptions:
  - category: integration
    key: api.secret
    valueFrom:
      env: INTEGRATION_SECRET        # an environment variable

users:
  - email: svc-dashboard@example.invalid
    passwordFrom:
      generate: svc-dashboard-password # a generated value (NAME or NAME:LENGTH)

software:
  - source:
      github:
        repo: owner/private-packages
        tokenFrom:
          env: PRIVATE_REPO_TOKEN
```

- `env` must name an environment variable which is set and not empty when the manifest is applied (also in a dry run); otherwise the run fails before changing anything. `validate` only checks the references, so it works without the variables.
- `generate` works like `${generate:NAME}`: the value is generated once and stored in the [secrets file](#generated-values).
- The values are masked in the output, like all secrets.
- Compared to `${VAR}` in the field itself, a reference is a separate field: the schema validates it, and [sops rules](docs/sops.md#secret-references-and-encrypted_regex) such as `encrypted_regex: ^(value|password|token)$` only encrypt literal secrets, while references stay readable. `${VAR}` keeps working in every field.

### Generated values

`${generate:NAME}` (or `${generate:NAME:LENGTH}`) is replaced with a random value, for example as the initial password of a user with a placeholder email address, or as a value which only the tenant needs to know:

```yaml
users:
  - email: svc-dashboard@example.invalid
    password: ${generate:svc-dashboard-password}

tenantOptions:
  - category: integration
    key: api.secret
    value: ${generate:integration-secret:32}
```

- **Values are stable.** A value is generated the first time a name is used and stored in the secrets file (`tenant.yaml` → `tenant.secrets.yaml`, or `--secrets-file`). Later runs reuse the stored value, so the run stays idempotent (e.g. the tenant option is not changed on every run). The same name always gets the same value, also across target tenants.
- **Values are kept.** The secrets file is saved after every run which generated values (also when the run failed, since a value may already have been used), with permissions `0600`. Each entry records when it was generated and for which host and tenant ID (looked up from the platform when the session has no tenant ID). The run reports the names of new values, never the values.
- **Encryption.** With `--secrets-encryption auto` (default), the secrets file is encrypted with [SOPS](docs/sops.md#encrypt-the-secrets-file-of-generated-values) when `sops` is installed and a creation rule of the nearest `.sops.yaml` (in the folder of the secrets file or a parent folder) matches it, and an encrypted secrets file stays encrypted. `sops` requires encryption, and `none` always writes a plain file. If encryption fails, the values are saved unencrypted (`0600`) and the run fails, so no value is lost.
- **Masking.** Generated values, user and tenant admin passwords and GitHub tokens are replaced with `***` in all output, including the request bodies printed by `--debug`.
- **Passwords are only set when a user is created.** The result line says `initial password from ${generate:NAME}`, or `generated password ... not applied (the user already exists)` when a new value was generated for an existing user.
- Values are 24 characters by default (12 to 128 with `:LENGTH`), start with a letter and contain lowercase and uppercase letters, digits and one of `-_.+=^~`, which satisfies the Cumulocity password rules.
- References in YAML comments are ignored, and `$${generate:NAME}` is a literal `${generate:NAME}`. A dry run doesn't generate values (it shows `<generated:NAME>`), and `validate` checks the references.
- `init` adds `*.secrets.yaml` to `.gitignore`. Only commit a secrets file when it is encrypted.

### Exporting a tenant

`export` reads the tenant of the active session and writes a manifest which describes it, as a starting point for managing an existing tenant, or for copying its settings to other tenants:

```bash
c8y tenant-sync export -o tenant.yaml
c8y tenant-sync run -f tenant.yaml --dry-run   # nothing to change on the same tenant
```

| Section | What is exported |
|---------|------------------|
| `tenantOptions` | All tenant options, except credentials (`credentials.*` keys are never exported) |
| `features` | Feature toggles set for the tenant (`--all-features` exports all toggles with their current state) |
| `retentionRules` | All retention rules |
| `certificateAuthority` | Whether the Cumulocity CA is enabled, if the tenant has one |
| `applications` | Applications the tenant is subscribed to but does not own, such as the built-in microservices (`--application-type MICROSERVICE,HOSTED` also exports web applications) |
| `deviceGroups` | The tree of device groups with name and description. Other fragments and the devices in the groups are not exported. A group with several parent groups is exported below one of them |
| `inventoryRoles` | All inventory roles with their permissions (the inventory assignments of users are not exported) |
| `digitalTwin` | The tenant's own property and asset definitions. Library definitions and definitions shared from the enterprise tenant are not exported, and neither are fields the server generates (such as `$schema`) or maintains (such as `lastUpdated`). Other custom fields, such as an asset definition's `icon`, are exported as `fragments` |
| `smartFunctions` | The deployed Data Preparation rules as [inline rules](#inline-rules), with their code and tests. Drafts and the AI chat history are not exported |

| Flag | Description |
|------|-------------|
| `-o`, `--output` | Write the manifest to a file instead of stdout |
| `--sections` | Comma-separated sections to export (default: all of the above) |
| `--all-features` | Export all feature toggles, not only the ones set for the tenant |
| `--application-type` | Types of subscribed applications to export (default: `MICROSERVICE`) |
| `--force` | Overwrite the output file |

`$` in exported values is written as `$$`, so values are not expanded as environment variables when the manifest is loaded. This includes smart function code: a JavaScript template literal such as `${data.name}` appears as `$${data.name}` in the manifest and is deployed unchanged.

A tenant without the Digital Twin Manager or Data Preparation skips that section with a note, so a full export also works there.

Review the manifest before applying it to other tenants: tenant options can contain values which only apply to the exported tenant.

## The Manifest

See [tenant.example.yaml](tenant.example.yaml) for a complete annotated example.

### IDE completion (JSON schema)

A JSON schema for the manifest is published at [tenant.schema.json](tenant.schema.json) and is generated from the Go types (`c8y tenant-sync schema`). Reference it from the first line of your manifest to get code completion, inline documentation and validation in editors with YAML language server support (VS Code [YAML extension](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml), JetBrains, Neovim, ...):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/reubenmiller/c8y-tenant-sync/main/tenant.schema.json
```

Manifests created with `c8y tenant-sync init` include this line already. To use a local copy instead (e.g. offline or pinned to your binary's version):

```bash
c8y tenant-sync schema -o tenant.schema.json
```

```yaml
# yaml-language-server: $schema=./tenant.schema.json
```

The schema encodes the same rules as `c8y tenant-sync validate`: unknown fields are rejected, required fields are enforced, and each `source` must set exactly one of `path`, `url` or `github`. A test keeps the committed schema file in sync with the Go types.

### Composing manifests

A manifest can include other manifest files with a top-level `include` list. This keeps shared settings (company-wide retention rules, user groups, trusted certificates) in one base file which per-environment or per-customer manifests reuse:

```
manifests/
├── base/
│   ├── common.yaml          # retention rules, user groups, certificates
│   └── certs/company-ca.pem
├── dev.yaml
├── prod.yaml
└── customer-a.yaml
```

```yaml
# prod.yaml
include:
  - base/common.yaml                  # a path relative to this file
  - path: "customers/*.yaml"          # or a glob pattern
  - path: local-overrides.yaml        # skipped when the file does not exist
    optional: true

tenantOptions:
  - category: configuration
    key: system.timezone.default
    value: Europe/Berlin
```

```bash
c8y tenant-sync validate prod.yaml   # lists the included files
c8y tenant-sync run -f prod.yaml --dry-run
```

Rules:

- **Paths**: include paths and glob patterns (`*`, `?`, `[...]`) are relative to the including file. Glob matches are merged sorted by name. A missing file, or a glob which matches nothing, is an error unless the entry sets `optional: true`. Included files can include other files; a cycle is an error which names the chain (`include cycle: prod.yaml -> base/common.yaml -> prod.yaml`). A file reached more than once (e.g. a base included by two files) is merged once, at its first position.
- **Same pipeline**: every included file is decrypted with sops when it is encrypted, `${VAR}` and `${generate:NAME}` references are expanded (with the same secrets file as the main manifest), and it is validated on its own. Validation errors name the file, e.g. `base/common.yaml: invalid manifest: ...`.
- **Relative paths**: relative `source.path` values in an included file are resolved against the directory of that file, not the main manifest. Hooks and commands declared in an included file run from that file's directory.
- **Merge order**: the included files are merged first, in the listed order, then the content of the including file.
  - List sections are concatenated.
  - Entries with an identity replace an earlier entry with the same identity, at the position of the earlier entry (the later definition wins). The identities are: tenant options (`category` + `key`), features (`key`), retention rules (the selector `dataType`/`fragmentType`/`type`/`source`), applications, user groups, device profiles and command groups (`name`) and users (`userName`). The whole entry is replaced; fields are not merged, so a user group redefined in `prod.yaml` must list all its roles.
  - Entries without an identity (trusted certificates, certificate revocation lists, software, firmware, configuration and SmartREST templates, which are matched by their files at run time) are all kept and applied in order.
  - `targets` and `certificateAuthority` of a later file replace those of earlier files.
  - `hooks.pre` and `hooks.post` are concatenated, and so are the `pre` and `post` hooks of each section in `hooks.sections`.

Duplicates within a single file are still validation errors, so a duplicate is only resolved by the "later wins" rule when the definitions come from different files. `include` only applies to `validate` and `run`; `export` writes a single file.

#### Several manifests on the command line

`run` and `validate` also accept several manifests, as repeated `-f` flags or as arguments (not both in one call):

```bash
c8y tenant-sync run -f base.yaml -f prod.yaml --dry-run
c8y tenant-sync validate base.yaml customer-a.yaml
```

The files are merged in the given order with the same rules as `include`: each file is merged with its own includes first, and later files win. A file given twice, or already included by an earlier file, is merged once. Relative paths, hooks and commands of each file use that file's directory. The default secrets file belongs to the first manifest (`base.yaml` → `base.secrets.yaml`), and the run header lists every manifest. Repeated `-f` values are not split on commas, so file names may contain commas.

When to use which:

- Use `include` when a manifest always needs its base, e.g. `prod.yaml` is never applied without `base/common.yaml`. The composition is then part of the reviewed file, and `run -f prod.yaml` is enough.
- Use several files on the command line to combine independent manifests per call, e.g. a CI job which applies `base.yaml` with a different customer file for each tenant, or a local overlay file which is not committed.

### Sources

Every repository entry (`software`, `firmware`, `configuration`) takes a `source` with exactly one of:

```yaml
source:
  # 1. Local file or directory (directories searched recursively with patterns)
  path: ./packages
  patterns: ["*.deb", "*.rpm"]
```

Relative paths are resolved against the manifest file's directory, so a manifest checked into a repository works no matter where the tool is invoked from.

```yaml
source:
  # 2. GitHub releases
  github:
    repo: owner/repo
    release: latest        # "latest" (default) | "latest-N" e.g. "latest-5" | "all" | a tag e.g. "v1.2.0"
    tags: ["comp-v*"]      # optional: only releases with matching tags; release then selects per pattern
    assets: ["*.wic.xz"]   # glob patterns against asset names
    includePrereleases: false
    linkOnly: false        # true: reference the download URL, don't upload the binary
                           # (default: true for firmware, false for everything else)
    token: ${GITHUB_TOKEN} # optional; falls back to GITHUB_TOKEN / GH_TOKEN env vars
```

```yaml
source:
  # 3. External URL (referenced, not uploaded)
  url: https://example.com/files/config.toml
```

```yaml
source:
  # 4. Cloudsmith repository (see "Cloudsmith packages" below)
  cloudsmith:
    repo: thinedge/tedge-release   # or https://broadcasts.cloudsmith.com/thinedge/tedge-release
    packages: [tedge, tedge-agent] # names or glob patterns
```

#### Cloudsmith packages

thin-edge.io publishes its packages to public [Cloudsmith](https://cloudsmith.io) repositories. A `cloudsmith` source creates software versions from them, like a GitHub source does from release assets:

```yaml
software:
  # The latest tedge and tedge-agent release for apt and apk: versions without
  # a url, which the package manager installs from the device's repositories
  - source:
      cloudsmith:
        repo: https://broadcasts.cloudsmith.com/thinedge/tedge-release
        packages: [tedge, tedge-agent]
        formats: [deb, alpine]
        architectures: [arm64, aarch64]

  # The two newest development builds of tedge
  - source:
      cloudsmith:
        repo: thinedge/tedge-main
        packages: [tedge]
        formats: [deb]
        version: latest-2

  # Every community plugin whose name starts with tedge-dot-, linked to the
  # download URL of each architecture's package
  - source:
      cloudsmith:
        repo: thinedge/community
        packages: ["tedge-dot-*"]
        binary: link
```

| Field | Description |
|-------|-------------|
| `repo` | `owner/repository`, or the URL of the repository, such as its broadcast page `https://broadcasts.cloudsmith.com/thinedge/tedge-release` |
| `packages` | Package names or glob patterns (required) |
| `formats` | `deb`, `rpm`, `alpine` and/or `raw` (default: all of them) |
| `architectures` | Architectures as the package format names them, e.g. `arm64`/`amd64` for deb and `aarch64`/`x86_64` for rpm and alpine (default: all) |
| `version` | `latest` (default), `latest-N` for the newest N versions, `all`, or an exact version such as `2.1.0` |
| `binary` | What each software version references: `auto` (default), `none`, `link` or `upload`, see below |

- **Binaries.** `binary` sets what the software versions reference:

  | `binary` | Software version | Software items |
  |----------|------------------|----------------|
  | `none` | No url: the device's package manager installs the version from its configured repositories (e.g. `apt install tedge=2.1.0`), resolving dependencies and picking the architecture | One per package and type, for all architectures (`tedge [apt]`) |
  | `link` | The Cloudsmith download URL: the device downloads that file and installs it | One per architecture (`tedge [arm64/apt]`) |
  | `upload` | The package, downloaded and uploaded to Cumulocity | One per architecture |
  | `auto` (default) | `none` for `deb`, `rpm` and `alpine` packages, `link` for `raw` packages, which no package manager installs | |

  With `none`, devices need the Cloudsmith repository in their package manager configuration, which the thin-edge.io install script sets up. Use `link` or `upload` for devices without it.
- **Metadata instead of filenames.** The name, version and architecture come from the package metadata, so every package format works, including apk files whose names have no architecture. The software type follows the format: `deb` → `apt`, `rpm` → `rpm`, `alpine` → `apk`; `raw` packages get the type from the filename (e.g. `archive`). The entry's `type`, `namePrefix` and `version` override them as for other sources.
- **Versions as the package manager uses them.** The version is Cloudsmith's version of the package, e.g. `2.1.0` for deb, `2.1.0-1` for rpm (with the release) and `2.1.0-r0` for apk, so it can be passed to the package manager as it is.
- **Selected per package and format.** `latest` and `latest-N` select the newest versions of each package name and format (sorted by Cloudsmith), with every architecture of those versions (with `binary: none`, the `architectures` field only limits which packages are read). Repositories with development builds such as `tedge-main` publish many versions, so for exact package names the listing stops as soon as the selected versions are complete. A glob reads every package whose name starts with the part before the first wildcard.
- **Public repositories.** The packages are listed through the Cloudsmith API without credentials. Private repositories are not supported yet.

#### Monorepos with a release per component

Some repositories publish separate releases for each component, e.g. with [release-please](https://github.com/googleapis/release-please): [thin-edge/tedge-flows-examples](https://github.com/thin-edge/tedge-flows-examples/releases) has tags such as `c8y-deploy-poll-v0.6.1` and `c8y-deploy-status-v0.3.1`. `release: latest` would only find the newest release of the whole repository. `tags` limits the releases to tags matching glob patterns, and `release` then selects per pattern:

```yaml
software:
  # The latest release of each of the three flows
  - source:
      github:
        repo: thin-edge/tedge-flows-examples
        tags: ["c8y-deploy-poll-v*", "c8y-deploy-status-v*", "c8y-deploy-operation-v*"]
        release: latest   # or latest-N (per pattern) or all
        assets: ["*.tar.gz"]
        linkOnly: true
```

- **Per pattern.** `latest` gives the newest release of each pattern, `latest-N` the newest N of each, and `all` every matching release. A specific tag can't be combined with `tags`.
- **Per component.** A `{component}` placeholder groups the releases by the part of the tag it matches, and `release` then selects per component. `tags: ["{component}-v*"]` with `release: latest` gives the newest release of every component, including components added later, without listing them:

  ```yaml
  - type: flow
    source:
      github:
        repo: thin-edge/tedge-flows-examples
        tags: ["{component}-v*"]   # c8y-deploy-poll-v0.6.1: component c8y-deploy-poll
        release: latest            # per component
        assets: ["*.tar.gz"]
        linkOnly: true
  ```

  The component matches as much of the tag as possible, so `{component}-v*` splits a tag at its last `-v`. Text around it narrows the components, e.g. `c8y-{component}-v*` only matches tags starting with `c8y-`. With `{component}`, every page of the release list is read, since a component can appear on any page.
- **Version fallback.** The tag from the pattern's first wildcard on is the version, so `c8y-deploy-poll-v0.6.1` gives `0.6.1` with `c8y-deploy-poll-v*` or `{component}-v*` (used when the asset filename has no version).
- **Paging.** The releases are read page by page (newest first) until every pattern has its releases, so components with older releases also work in repositories with many releases.

#### Linking or uploading GitHub assets

Firmware images are often large, so firmware entries with a GitHub source **link** the release assets by default: the asset's download URL is stored in the firmware version's URL field, and nothing is downloaded or uploaded. Devices then download the image from GitHub, so this only works for public repositories.

Set `linkOnly: false` on a firmware entry to download the assets and upload them to Cumulocity instead, for example for private repositories or devices without access to GitHub:

```yaml
firmware:
  - name: tedge-rugix-pi
    deviceType: rugix-pi
    source:
      github:
        repo: thin-edge/tedge-rugix-image
        assets: ["*.img.xz"]
        linkOnly: false # upload the images to Cumulocity
```

Software and configuration entries upload by default. Set `linkOnly: true` on them to link instead.

Changing `linkOnly` on an entry which was already synced updates the existing versions on the next run, without `--force`: a linked version is replaced with an uploaded binary, an uploaded one is switched to the link (and its old binary removed), and a link whose URL changed is updated. An already uploaded binary is not compared by content, so replacing it still needs `--force`.

Any source can be marked `optional: true`: when the local path does not exist or nothing matches (no files, no release assets), the entry is reported as `skipped` instead of failing the run — useful when a build directory only exists on some pipelines.

With a GitHub source, `release: all` mirrors every release into the tenant, and `release: latest-5` keeps just the most recent five — pointing a firmware entry at an image repository is enough to populate the firmware version history:

```yaml
firmware:
  - name: tedge-rugix-pi
    deviceType: rugix-pi
    source:
      github:
        repo: thin-edge/tedge-rugix-image
        release: latest-5
        assets: ["*.img.xz"]
```

### Version resolution

For each artifact the version is determined by (first match wins):

1. `version` set explicitly on the manifest entry
2. Version parsed from the filename (package conventions for software; build-system conventions or `versionPattern` for firmware)
3. The GitHub release tag (with a leading `v` stripped)

Firmware entries with non-standard naming can extract versions with a regex:

```yaml
firmware:
  - deviceType: model7
    versionPattern: 'BUILD(\d+)'   # single capture group = version
    source:
      path: ./firmware
      patterns: ["FW_MODEL7_*.custom"]
```

The firmware `deviceType` supports placeholders derived from each parsed artifact — `{name}`, `{version}` and `{filename}` — so the device type can follow the artifact naming without listing every image explicitly:

```yaml
firmware:
  - deviceType: "linux-{name}"   # e.g. linux-core-image-tedge-rpi4
    source:
      path: ./tmp/deploy/images
      patterns: ["*.wic.bz2"]
```

### Explicit entries

Instead of a `source`, a software, firmware or configuration entry can declare a single item explicitly. Nothing is parsed from filenames: the given values are used as-is.

#### Software

A software entry with `name` declares one software version with its `version`, an optional `type` (softwareType) and an optional `url`:

```yaml
software:
  # A package which devices install from their own package repositories:
  # the version has an empty url
  - name: tedge
    version: latest
    type: apt

  # A package hosted elsewhere
  - name: sensor-agent
    version: 2.1.0
    type: apt
    url: https://example.com/packages/sensor-agent_2.1.0_arm64.deb
```

- `version` is required and used as-is, so `latest` is a version called `latest` (for example meaning "install the latest version from the package repository").
- Without `url`, the version is created with an empty url and nothing is uploaded.
- `name` and `source` are mutually exclusive, and `namePrefix` and `typeMap` only apply to entries with a source.

#### Firmware

A firmware entry without a `source` declares one firmware version with its `name`, `version`, an optional `url` and an optional `deviceType` (the `{name}` and `{version}` placeholders use the declared values). For entries with a source, `name` and `version` keep overriding the parsed values.

```yaml
firmware:
  - name: tedge-rugix-pi
    version: 20261104.2250
    deviceType: rugix-pi
    url: https://github.com/thin-edge/tedge-rugix-image/releases/download/20261104.2250/tedge-debian-12-rpi_20261104.2250.img.xz
```

- Without `url`, the version is created with an empty url and nothing is uploaded.
- `versionPattern` only applies to entries with a source.

#### Configuration

A configuration entry without a `source` declares one configuration item with its `name` and `configurationType`, and either a `url` or inline `content`:

```yaml
configuration:
  # Inline content, uploaded as a file
  - name: mosquitto
    configurationType: mosquitto.conf
    deviceType: thin-edge.io
    filename: mosquitto.conf   # optional, defaults to the name
    force: true                # re-upload on every run, so content changes are applied
    content: |
      listener 1883
      allow_anonymous false

  # A file hosted elsewhere
  - name: collectd
    configurationType: collectd.conf
    url: https://example.com/config/collectd.conf
```

- Inline `content` is uploaded as a file called `filename` (defaults to the `name`). `${VAR}` references in the content are expanded, and the content can be [encrypted with SOPS](#secrets-with-sops) (e.g. `encrypted_regex: ^(content)$`), so configuration with secrets can be kept in git.
- `url` and `content` are mutually exclusive. With neither, the configuration item is created without a url.
- An existing configuration item is not changed when its content changes, since the uploaded file is not compared. Set `force: true` on the entry to replace the item and re-upload its file on every run, so changes are applied (like `--force`, but only for this entry). This also works for entries with a source. A forced entry is reported as `updated` on every run.

### Tenant option lookups

Tenant option values can be resolved by reference instead of hardcoding them, using `valueFrom` with exactly one reference:

| `valueFrom` | Value |
|-------------|-------|
| `application` | The ID of the application with this name (looked up when applying) |
| `device` | The managed object ID of the device with this name (looked up when applying) |
| `env` | The value of an environment variable, see [Secret references](#secret-references) |
| `generate` | A generated value, see [Generated values](#generated-values) |

```yaml
tenantOptions:
  - category: application
    key: default.application
    valueFrom:
      application: devicemanagement
  - category: integration
    key: api.secret
    valueFrom:
      env: INTEGRATION_SECRET
```

### Retention rules

Retention rules have no name, so a rule is identified by its selector combination — `dataType` plus the optional `fragmentType`, `type` and `source` filters (defaulting to the `*` wildcard). The rules in the tenant are matched against that combination: a missing rule is created, and an existing rule is updated when its `maximumAge` (or `editable`, when set) differs. Rules in the tenant that are absent from the manifest are left alone (no prune mode yet).

```yaml
retentionRules:
  - dataType: MEASUREMENT # ALARM | AUDIT | BULK_OPERATION | EVENT | MEASUREMENT | OPERATION | *
    maximumAge: 365       # days
  - dataType: EVENT
    type: c8y_LocationUpdate
    maximumAge: 30
```

> **Note**: updating a rule that is marked non-editable in the tenant fails — such system rules must be unlocked or removed manually first.

### Cumulocity certificate authority

The `certificateAuthority` section enables or disables the tenant's Cumulocity certificate authority (CA), which issues device certificates (for example for certificate-based device registration):

```yaml
certificateAuthority:
  enabled: true                 # required
  autoRegistrationEnabled: true # optional; left untouched when omitted
```

- `enabled: true` creates the CA when the tenant does not have one yet, and sets its trusted certificate to `ENABLED`.
- `enabled: false` sets the CA's trusted certificate to `DISABLED`. The CA is never deleted, since deleting it would invalidate every certificate it issued. If the tenant has no CA, nothing happens.

Creating the CA requires the certificate authority feature to be available on the tenant.

### Trusted certificates and revocation lists

The `trustedCertificates` section uploads device CA certificates into the tenant trust store (Device Management → Management → Trusted certificates). Sources may be PEM files (each `CERTIFICATE` block in a file is synced, so chain files work; other blocks such as private keys are ignored) or single DER/base64-encoded certificates; directories are scanned for `*.pem`, `*.crt` and `*.cer` by default. Certificates are matched by their SHA-1 fingerprint: missing ones are uploaded, and the `name`, `status` and `autoRegistrationEnabled` of existing ones are updated when they differ. Certificates are never deleted.

```yaml
trustedCertificates:
  - autoRegistrationEnabled: true # only managed when set
    source:
      path: ./certificates # scans for *.pem, *.crt, *.cer by default

  # a single certificate with an explicit name, kept uploaded but disabled
  - name: legacy-factory-ca
    status: DISABLED # ENABLED (default) | DISABLED
    source:
      path: ./certificates/archive/legacy-factory-ca.pem
```

The certificate name defaults to the certificate's subject common name, then the filename. `status` defaults to `ENABLED` and is always managed; `autoRegistrationEnabled` is only managed when set in the manifest.

The `certificateRevocationLists` section uploads certificate revocation entries from CSV files (header `SERIALNO[,DATE]`, one certificate serial number in hex per row, the revocation date is optional). Files are validated locally before uploading. Uploads are **additive**: entries already on the tenant revocation list are never removed.

```yaml
certificateRevocationLists:
  - source:
      path: ./certificates/revoked.csv
```

```csv
SERIALNO,DATE
3f9a1c84d2,2026-01-15T00:00:00Z
0b77e2
```

Since the file content must be read, `url` sources are not supported for these sections (GitHub release assets work — they are downloaded first).

### Application subscriptions

Application entries normally just subscribe an existing application to the target tenant. `subscribed: false` declares the opposite — the application is unsubscribed (a missing application or subscription counts as the desired state, so the entry stays idempotent):

```yaml
applications:
  - name: advanced-software-mgmt
  - name: cloud-remote-access
    subscribed: false
```

The current subscriptions of the target tenant are read first (for the current tenant with the same request as `c8y currenttenant listApplications`), so applications which are already subscribed (or already unsubscribed) are reported as `unchanged` without calling the (un)subscribe API. If `type` is set, it must match the subscribed application.

Subscriptions to applications which the tenant does not own, such as the built-in microservices owned by the `management` tenant, can generally only be changed by the owner tenant. When an (un)subscribe request is denied (`403`) and the application is owned by a tenant which is neither the target tenant nor the tenant running the sync, the entry is reported as `skipped` with the owner tenant, instead of failing the run:

```
  ⏭ [applications] smartrule (skipped): subscribe denied: the application is owned by tenant management; subscribe it from tenant management (e.g. with a targets section selecting t12345)
```

To change such subscriptions, run `c8y tenant-sync` from the owner tenant with a [targets section](#target-tenants) selecting the tenants to maintain. A denied request for an application which the target tenant or the running tenant owns is still a failure, since it points to missing permissions.

With a `source`, the application is also created when missing (type defaults to `MICROSERVICE`, `contextPath` to the name) and its binary (a single zip) uploaded. Since binary content cannot be compared, the upload only happens on creation — or on every run with `--force`:

```yaml
applications:
  - name: my-microservice
    type: MICROSERVICE
    source:
      path: ./build/my-microservice.zip
```

#### Web applications from releases (cloning built-in applications)

A `type: HOSTED` entry with a `source` keeps a web application, owned by the tenant running the sync, at the version of its source. This is how a tenant clones a built-in application such as Device Management and keeps the clone up to date with the releases published at [Cumulocity-IoT/devicemanagement](https://github.com/Cumulocity-IoT/devicemanagement/releases):

```yaml
applications:
  - name: devicemanagement
    type: HOSTED
    source:
      github:
        repo: Cumulocity-IoT/devicemanagement
        tags: ["v1025.*"]          # stay on the 1025 release line
        includePrereleases: true   # the releases are published as prereleases
        assets: ["devicemanagement-*.zip"]
```

On each run:

- Only an application owned by the tenant running the sync counts as existing. The built-in `devicemanagement` application owned by the `management` tenant is left alone, and a tenant-owned copy with the same name and context path is created. That copy then replaces the subscribed application for the tenant. Its key defaults to `<tenant>-<name>-application-key`, since the built-in application already uses `<name>-application-key`; set `key` to choose another.
- The release is listed without downloading it and its version (from the asset filename such as `devicemanagement-1025.15.3.zip`, otherwise the release tag) is compared with the `manifest.version` of the application. The archive is only downloaded and uploaded when the versions differ, so a scheduled run picks up new releases without `--force`:

  ```
    ↻ [applications] devicemanagement (updated): version 1025.14.0 -> 1025.15.3 (uploaded devicemanagement-1025.15.3.zip)
  ```

- `--force` re-uploads the archive even when the version is already active. When no version can be read from the source, an existing application is only re-uploaded with `--force`.

Uploading a new archive replaces the application binary only. The plugins installed into the application through the Administration application are stored in the application `config` and are kept.

The source must resolve to exactly one zip file; narrow it down with `github.assets` and `github.tags` (the repository publishes several release lines, such as `v1023.*` and `v1025.*`). The copy is owned by the tenant running the sync, so with a [targets section](#target-tenants) it is created once in that tenant and only the subscription is applied to the target tenants.

### Device groups

The `deviceGroups` section creates a tree of device groups, so every tenant has the same structure (regions, sites, production lines, customers) and dashboards, inventory roles and bulk operations can target the groups consistently.

```yaml
deviceGroups:
  - name: Europe
    description: European sites
    fragments:                 # optional: extra fragments merged into the managed object
      customer_Region:
        code: EU
    groups:
      - name: Plant Berlin
        groups:
          - name: Line 1
          - name: Line 2
  - name: Asia
    groups:
      - name: Line 1           # names only need to be unique within the parent
```

```
  ✚ [deviceGroups] Europe (created)
  ✚ [deviceGroups] Europe/Plant Berlin (created): linked to Europe
  ✓ [deviceGroups] Europe/Plant Berlin/Line 1 (unchanged)
  ↻ [deviceGroups] Europe/Plant Berlin/Line 2 (updated): linked to Europe/Plant Berlin
```

- A device group is a managed object with the `c8y_IsDeviceGroup` fragment. Root groups are created with the type `c8y_DeviceGroup`, nested groups with `c8y_DeviceSubgroup`, and new groups get the [`c8y_TenantSync`](#identifying-synced-items) fragment.
- A group is identified by its **name within its parent**: a root group by name among the device groups without a parent, a nested group by name among the device groups linked to its parent as a child asset. Groups are looked up with an inventory query (`has(c8y_IsDeviceGroup)` and the name, with their parents). Items are labelled with their path, e.g. `Europe/Plant Berlin/Line 1`.
- A missing group is created and linked to its parent as a child asset. A nested group which exists without any parent (type `c8y_DeviceSubgroup`, e.g. left behind by an interrupted run) is linked instead of creating a second one. A root group is never moved below another group.
- `description` and `fragments` are only updated when they differ. A fragment counts as unchanged when the existing value contains all values of the manifest, so values added by other tools are kept. An omitted `description` is left alone.
- If several groups match a name (e.g. two root groups called `Europe`), the item fails with the IDs of the matches and its child groups are skipped. Rename or remove the duplicates; tenant-sync does not guess.
- Groups are never deleted or unlinked, and devices in the groups are not changed.
- Names must not be empty and must be unique among siblings. The fragments `name`, `type`, `description`, `c8y_IsDeviceGroup`, `c8y_TenantSync` and the platform properties (`id`, `owner`, `childAssets`, ...) can not be set.
- Device groups are synced before user groups and users.

### Users and user groups

The `userGroups` section creates user groups (called *global roles* in the Cumulocity UI) and assigns roles to them; the `users` section creates users and assigns them to groups. Both are idempotent and **additive**: roles and group memberships already present in the tenant but absent from the manifest are never removed, so built-in groups (`admins`, `business`, ...) can be extended and manually-managed assignments survive a sync.

```yaml
userGroups:
  - name: operators
    description: Read-only access for the operations team
    roles:
      - ROLE_INVENTORY_READ
      - ROLE_ALARM_READ

users:
  - email: jdoe@example.com      # the userName defaults to the email
    firstName: Jane
    lastName: Doe
    sendPasswordResetEmail: true # email a reset link instead of setting a password
    groups: [operators]          # groups synced above or already in the tenant

  - userName: svc-dashboard
    email: ops@example.com       # required to create a user whose userName is not an email
    password: ${DASHBOARD_USER_PASSWORD} # injected from the environment
    groups: [operators]

  - userName: old-account@example.com
    enabled: false # keep the account but disable it
```

User groups are synced before users, so `users[].groups` can reference groups declared in the same manifest (or groups that already exist in the tenant). Roles referenced by `userGroups[].roles` must exist in the tenant — note that some roles only appear after subscribing the microservice that provides them, which the `applications` section (applied earlier) can take care of.

A user needs a `userName` or an `email`, and each defaults to the other:

- Without `userName`, the `email` is used as the userName.
- Without `email`, a `userName` which is an email address (e.g. `jdoe@example.com`) is used as the email **when the user is created**. Cumulocity requires an email for new users, so creating a user whose userName is not an email address (e.g. `svc-dashboard`) without `email` fails with an error. Existing users are not changed, so their email is never replaced by their userName, and existing users without an email can still be updated (e.g. disabled).

For existing users only the fields set in the manifest are compared and updated (empty fields are left alone), with two deliberate exceptions:

- **`password` is only used when creating the user** and never updated afterwards, so re-running the sync doesn't reset credentials. Use `${VAR}` expansion to keep secrets out of the manifest, or prefer `sendPasswordResetEmail: true` to avoid handling passwords entirely.
- **`enabled` defaults to true** and is always managed: a user disabled in the UI is re-enabled on the next sync unless the manifest says `enabled: false`.

### Inventory roles and device group access

User groups grant permissions on the whole tenant. For least-privilege access, Cumulocity has *inventory roles*: a set of permissions (for example "read measurements" or "manage alarms") which a user gets only on the devices of specific device groups. The `inventoryRoles` section defines the roles, and `users[].inventoryAssignments` gives a user roles on a device group:

```yaml
inventoryRoles:
  - name: Site operator
    description: Alarms and operations for one site
    permissions:
      - {scope: ALARM, permission: ADMIN}
      - {scope: OPERATION, permission: ADMIN}
      - {scope: MEASUREMENT, permission: READ}
      - {scope: MANAGED_OBJECT, permission: READ}

  - name: Restart only
    permissions:
      - {scope: OPERATION, type: c8y_Restart, permission: ADMIN}

users:
  - email: berlin-ops@example.com
    inventoryAssignments:
      - group: Plant Berlin          # device group name ...
        roles: [Site operator]
      - group: Europe/Plant Paris    # ... or a path of group names
        roles: [Restart only]
```

A permission has a `scope` (`ALARM`, `AUDIT`, `EVENT`, `MANAGED_OBJECT`, `MEASUREMENT`, `OPERATION` or `*`), an optional `type` which limits it to a type or fragment such as `c8y_Restart` (default `*`), and a `permission` level (`READ`, `ADMIN`, or `*` for both; `ALL` is accepted as an alias of `*`).

Inventory roles:

- are matched by **name** and created when missing;
- get their **description** updated when it is set and differs;
- get their **permissions replaced** when the set differs from the manifest (the order does not matter). The manifest owns the definition of a declared role. A role without `permissions` in the manifest keeps the permissions it has;
- are never deleted, and roles not in the manifest are left alone.

Inventory assignments are applied in the `users` section, after the user is created or updated, and are **additive**:

- `group` is the name of a device group (`has(c8y_IsDeviceGroup)`). The name must match exactly one group, otherwise the user fails with "not found" or "ambiguous" (with the IDs of the matching groups). For groups with the same name, use a path of names separated by `/`, such as `Europe/Plant Berlin`, where each group is a direct child of the previous one. A value with a `/` is first looked up as a plain group name.
- `roles` are inventory role names, declared in `inventoryRoles` or already in the tenant. They are checked when the manifest is applied, so a role which is in neither fails the user with "not found".
- Missing roles are added to the user's existing assignment for the group, or a new assignment is created. Roles and assignments which are not in the manifest are never removed.

`inventoryRoles` is applied after `userGroups` and before `users`, so assignments can use roles created in the same run. Device groups are only looked up, never created.

### SmartREST 2.0 template collections

The `smartrestTemplates` section syncs SmartREST 2.0 template collections from JSON files as exported by the platform UI (Device Management → Device types → SmartREST templates → Export) — one collection per file. Point a source at a directory and every `*.json` file in it is synced:

```yaml
smartrestTemplates:
  - source:
      path: ./smartrest # scans for *.json by default

  # or a single file, optionally renaming the collection
  - name: custom_devmgmt
    source:
      path: ./exports/custom_devmgmt.json
```

A collection is stored as a managed object of type `c8y_SmartRest2Template` and matched via its external identity (`c8y_SmartRest2DeviceIdentifier`, the X-Id devices reference), taken from the file's `name` (falling back to `__externalId`). The upsert only writes when the collection actually differs from the file: the request/response template lists are compared **order-insensitively** (sorted by `msgId`), platform bookkeeping fields (`id`, `lastUpdated`, `owner`, ...) are ignored, and the `c8y_TenantSync` annotation never triggers an update by itself — so re-applying an unchanged export performs no writes.

Since the file content must be read, `url` sources are not supported for this section (GitHub release assets work — they are downloaded first).

### Digital Twin Manager definitions

The `digitalTwin` section creates and updates [Digital Twin Manager](https://cumulocity.com/docs/digital-twin-manager/) property and asset definitions. Each entry of `propertyDefinitions` and `assetDefinitions` is either a definition declared inline, or a `source` of definition files:

```yaml
digitalTwin:
  managedBy: tenant-sync # optional: tag added to every definition the sync writes
  prune: false           # true deletes definitions tagged managedBy which are no longer declared

  propertyDefinitions:
    # Definition files: directories are scanned for *.json, *.yaml and *.yml
    - source:
        path: ./dtm/properties

    # A single file by URL (downloaded on each run)
    - source:
        url: https://raw.githubusercontent.com/thin-edge/tiny-edge/refs/heads/main/cumulocity/dtm/tiny_edge_remote_access.json

    # Inline
    - identifier: maxFlow
      contexts: [asset]
      jsonSchema: {title: Maximum flow, type: number, minimum: 0}

  assetDefinitions:
    - source:
        path: ./dtm/assets

    - identifier: Pump
      jsonSchema: {title: Pump}
      composition:
        allowedProperties:
          - identifier: maxFlow
            minOccurs: "1"
        allowedSubAssets:
          - identifier: Valve
      fragments: # additional custom fragments
        icon: {category: devicesAndSensors, name: pump}
```

A definition file holds definitions in the format of the DTM API, as JSON or YAML: the body you would `POST` to `/service/dtm/definitions/properties` or `/assets`. Several definitions can share a file, as a list or as YAML documents separated by `---`:

```yaml
identifier: maxFlow
contexts: [asset]
jsonSchema: {title: Maximum flow, type: number}
---
identifier: minFlow
contexts: [asset]
jsonSchema: {title: Minimum flow, type: number}
```

The key decides the kind: files under `propertyDefinitions` hold property definitions, and files under `assetDefinitions` hold asset definitions. A `kind` field in a document is optional, but a file whose `kind` belongs to the other key fails.

How the sync behaves:

- **Matching**: property definitions are matched by `identifier` and `contexts` (in any order), asset definitions by `identifier`.
- **Order**: property definitions are applied first, then asset definitions. An asset definition is applied after the asset definitions its composition allows as sub-assets.
- **Idempotency**: a definition is only written when a declared field differs. Fields the server generates (such as `$schema` or the default `minOccurs`) do not count as changes, and tags and contexts are compared as sets.
- **Removed schema fields**: when a field is removed from a declared `jsonSchema` (for example `minimum`), it is also removed from the definition. Other top-level fields the manifest does not set (such as `tags` or `composition`) are left as they are. Asset definitions only accept `title` and `description` in their `jsonSchema`; the server builds the rest from the composition.
- **Read-only definitions**: a definition shared from the enterprise tenant or the definition library which differs from the manifest fails without a write.
- **URLs**: a `url` source is downloaded on each run, so it always applies the current content of that URL. Pin it to a tag or commit (instead of a branch) to control when definitions change.
- **Pruning**: with `prune: true`, definitions tagged `managedBy` which no manifest entry declares are deleted (asset definitions first). Without `prune`, definitions are never deleted. When a source fails to load or download, the whole section is skipped, so a missing file never deletes definitions.

With includes, inline definitions with the same key replace earlier ones, source entries are all kept, and `managedBy` and `prune` of a later file win when set.

### Smart functions (Data Preparation rules)

The `smartFunctions` section deploys [Data Preparation](https://cumulocity.com/docs/data-preparation/) smart functions (rules). A rule is deployed from a package: a folder laid out like the package archive (`data-prep.yaml`, the bundled smart function and `tests/*.yaml`), or a `.tar.gz` archive exported by the Data Preparation application.

```yaml
smartFunctions:
  # A folder of packages: every folder with a data-prep.yaml and every
  # *.tar.gz/*.tgz archive is a rule
  - source:
      path: ./smartfunctions

  # A single exported archive, with an explicit rule name
  - name: tiny-edge-events
    source:
      path: ./exports/tiny-edge-events.tar.gz
    waitTimeout: 2m # default 60s; 0 does not wait

  # Rule packages published as GitHub release assets
  - source:
      github:
        repo: my-org/my-smart-functions
        release: latest
        assets: ["*.tar.gz"]
```

The rule name defaults to the package folder name, or the archive filename without `.tar.gz`/`.tgz`. Packages are read and validated locally first (also in dry-run mode), so format errors such as MQTT wildcards in `input.topicPattern` show up before anything is uploaded.

A rule is only deployed when its smart function code, configuration (description, tags, disabled, input) or tests differ from the deployed rule, or when its last deployment failed. `--force` deploys every rule. After the upload, the sync waits for the deployment to finish (`waitTimeout`) and reports errors from the engine as failures.

A `url` source (for example a release asset or any other download link to a `.tar.gz`) is downloaded before the package is read. Dry-run mode does not download it, so the package is only validated in a real run.

#### Inline rules

An entry without a `source` declares the rule in the manifest. It takes the fields of `data-prep.yaml`, the code (inline with `code`, or from a local file with `codeFile`) and optional tests:

```yaml
smartFunctions:
  - name: tiny-edge-events
    description: tiny-edge events to Cumulocity events
    tags: [tiny-edge, event]
    disabled: false
    input:
      topicPattern: "te/device/*///e/*" # transport defaults to mqtt
    codeFile: ./smartfunctions/events.js # or code: |
    tests:
      identify:
        inputs:
          - topic: te/device/dev01///e/zephyr_Identify
            clientID: dev01
            payload: {text: Identify pressed} # objects are sent as JSON
            time: "2026-10-06T08:15:00Z"
        expectedOutput:
          - cumulocityType: event
            payload: {type: zephyr_Identify, text: Identify pressed, time: "2026-10-06T08:15:00.000Z"}
            externalSource: [{externalId: dev01, type: c8y_Serial}]
```

- `name`, `input.topicPattern` and one of `code` or `codeFile` are required. A relative `codeFile` is resolved against the directory of the manifest file that declares it.
- A test input `payload` can be a string, which is sent as it is, with `payloadFormat` defaulting to `json` when the string holds JSON and `text` otherwise. It can also be an object, list or number, which is sent as JSON. Set `payloadFormat: base64` for binary payloads.
- `time` is required on every test input. It is stored in UTC with milliseconds, the format the server returns, so a rule whose tests use another format or time zone still counts as unchanged on the next run.
- `expectedOutput` is a list of objects, a single object, or a string holding a JSON array or object. The string form lets you paste JSON with any indentation, for example from a test run:

  ```yaml
          expectedOutput: |
            [{"cumulocityType": "event", "payload": {"type": "zephyr_Identify", "text": "Identify pressed"}}]
  ```

  The string may also use what is common when copying from JavaScript: trailing commas, unquoted keys and single quotes.

  JSON written directly (without `|`) also works, since YAML reads JSON as it is, but then every line must be indented further than `expectedOutput:`.

For rules with several tests, a package folder is usually easier to maintain: it is the format the Data Preparation application exports, and each test is its own file.

### Target tenants

By default the manifest applies to the current tenant (the `C8Y_*` session). A `targets` section applies it to other tenants instead — run once from the parent/management tenant to keep a whole fleet of tenants in sync. The selection modes are additive (the union of all matches):

```yaml
targets:
  allChildren: true                    # every child tenant of the current tenant
  tenants: [t12345, child.example.com] # explicit, by ID or domain
  selector:
    domain: "*.iot.example.com"        # glob matched against the tenant domain
    company: ACME                      # exact company name
  current: true                        # also include the current tenant
                                       # (defaults to true only when nothing else is selected)
```

The selection can be overridden from the command line (any target flag replaces the manifest's selection entirely):

```bash
c8y tenant-sync run -f tenant.yaml --all-children
c8y tenant-sync run -f tenant.yaml --target t12345 --target child.example.com
c8y tenant-sync run -f tenant.yaml --target-selector "domain=*.iot.example.com" --include-current
```

Application subscriptions and feature toggles are managed directly from the parent (their APIs address the target tenant by ID). Everything else — tenant options, software/firmware/configuration repositories, device profiles, hooks — runs *inside* each target tenant and therefore needs per-tenant credentials, configured via `targets.credentials`:

```yaml
targets:
  allChildren: true
  credentials:
    mode: serviceUser        # serviceUser (default) | sessions
    application: tenant-sync # serviceUser mode: placeholder microservice name
    sessionHome: ""          # sessions mode: session dir (default $C8Y_SESSION_HOME, then ~/.cumulocity)
```

- **`serviceUser`** (default) uses the [microservice service user](https://goc8ycli.netlify.app/docs/tutorials/microservice-serviceuser/) pattern: a placeholder `MICROSERVICE` application (default name `tenant-sync`, no binary, no deployment) is created in the current tenant, subscribed to every target tenant, and its per-tenant service users are fetched via the application bootstrap user. The application and subscriptions are idempotent and left in place between runs. Service users for freshly subscribed tenants can take a few seconds to appear; the tool retries briefly.
  - **Roles.** The service users get the roles every section needs inside a target tenant: inventory (device groups, software, firmware, configuration, device profiles), identity (SmartREST templates), tenant options, retention rules, tenant management (certificate authority, trusted certificates, revocation lists) and user management (user groups, inventory roles, users), plus read access to applications. The list is the same for every manifest, so applying different manifests doesn't change the application back and forth. `features`, `applications` and `tenants` run with the credentials of the current tenant and need no service user roles.
  - **Existing subscriptions.** A tenant's service user gets its roles when the tenant is subscribed. When the service user of a target lacks a required role (e.g. the tenant was subscribed by an older version), the tool subscribes the application to that tenant again, which creates a new service user with the current roles.
- **`sessions`** reads [go-c8y-cli session files](https://goc8ycli.netlify.app/docs/gettingstarted/#sessions) and matches them to each target by tenant ID and then by domain. Encrypted sessions are not supported — the run fails with a clear error listing tenants without a matching session.
- **`tenantAdmin`** logs in to each target with the admin credentials of its entry in the [`tenants`](#provision-child-tenants) section: `adminName` and the admin password (`adminPass`, an `env` reference, or a generated value from the secrets file). It needs no placeholder microservice and no session files, so it suits tenants which this tool creates and manages, e.g. from a CI runner. Targets are matched to `tenants` entries by ID and then by domain. The credentials are checked before any section runs; the run fails for a target without a matching entry, with `sendPasswordResetEmail` (the password is not known), with a password generated in this run for a tenant which already existed (the secrets file was lost), or whose admin login is rejected (the password was changed or two-factor authentication was enabled).

In multi-tenant runs every result line and the hook environment (`C8Y_TENANT`, `C8Y_USERNAME`, `C8Y_PASSWORD`) are scoped to the tenant being applied:

```
🏢 Tenant: t12345
  ✓ [applications] t12345: cloud-remote-access (unchanged): not subscribed
  ↻ [features] t12345: mqtt-service.smartrest (updated): enabled
```

### Provision child tenants

A management or enterprise tenant can create its child tenants from the manifest. An MSP onboarding a customer, or a platform team adding a site, declares the tenant in git and runs one command: the `tenants` section creates the tenant, and the [`targets`](#target-tenants) section applies the rest of the manifest to it in the same run.

```yaml
# customer-a.yaml, applied with the credentials of the parent tenant
tenants:
  - domain: customer-a.eu-latest.cumulocity.com  # unique key: the full domain
    id: customer-a                               # optional: generated by the platform when omitted
    company: Customer A
    adminName: admin
    adminEmail: admin@customer-a.example.com
    adminPasswordFrom:
      generate: customer-a-admin                 # stored in the secrets file
    contactName: Jane Doe
    contactPhone: "+49 123 456"
    customProperties:
      externalReference: CRM-1234
    applications: [devicemanagement, cockpit]   # subscribed when missing

# Configure the new tenant in the same run
targets:
  tenants: [customer-a.eu-latest.cumulocity.com]

tenantOptions:
  - category: onboarding
    key: crm.reference
    value: CRM-1234

userGroups:
  - name: operators
    roles: [ROLE_INVENTORY_READ, ROLE_ALARM_READ]
```

```bash
c8y tenant-sync run -f customer-a.yaml --dry-run   # preview (no API calls)
c8y tenant-sync run -f customer-a.yaml
```

```
  ✚ [tenants] customer-a.eu-latest.cumulocity.com (created): created: customer-a; admin password from ${generate:customer-a-admin}; subscribed: devicemanagement, cockpit

🏢 Tenant: customer-a
  ✚ [tenantOptions] customer-a: onboarding/crm.reference (created)
  ✚ [userGroups] customer-a: operators (created): assigned roles: ROLE_INVENTORY_READ, ROLE_ALARM_READ

🔐 Generated 1 value(s): customer-a-admin
   Saved to customer-a.secrets.yaml (unencrypted, permissions 0600)
```

- **Matched by domain.** Each entry is matched (case-insensitively) against the domains of the child tenants of the current tenant. Use the full domain as the platform stores it, so the entry matches on the next run and can be used in `targets.tenants`.
- **Created when missing.** Creating a tenant requires `company` and one admin password option: `adminPass` (supports `${VAR}` and `${generate:NAME}`), `adminPasswordFrom` (a [secret reference](#secret-references) to an environment variable or a [generated value](#generated-values)), or `sendPasswordResetEmail: true` (requires `adminEmail`). The options are mutually exclusive. The result line shows the ID of the new tenant (`created: t1234567`).
- **Tenant ID (optional).** Without `id`, the platform generates the tenant ID. Set `id` to choose it yourself, e.g. to match a customer number, or to reference the tenant by a known ID in `targets.tenants` or scripts. It is only used when creating the tenant, since a tenant ID can never be changed: if the tenant with the domain exists with another ID, or the ID is already used by a tenant with another domain, the entry fails instead of changing anything. The platform enforces its own rules for tenant IDs; the manifest only rejects IDs with spaces, dots, colons or slashes (a value with a dot would be read as a domain in `targets.tenants`).
- **Updated when different.** For an existing tenant only `company`, `contactName`, `contactPhone` and `customProperties` are updated, and only when they differ. Custom properties are merged: keys not in the manifest are kept. The domain and the admin credentials of an existing tenant are never changed (a password generated for an existing tenant is reported as `not applied`).
- **Never deleted.** Tenants not in the manifest are left alone, and tenants are never deleted or suspended.
- **Subscriptions.** `applications` are subscribed to the tenant when missing (also for existing tenants); other subscriptions are kept. Use the [`applications`](#application-subscriptions) section with `targets` to unsubscribe applications.
- **Runs once, before the targets.** The section runs once per run with the credentials of the current tenant (not per target), before the targets are resolved. A tenant created in the run can therefore be selected by `targets` (by domain, `allChildren` or `selector`) and configured by every other section. Global hooks run per target and so after this section; `hooks.sections.tenants` runs around it.
- **Configure tenants with their admin credentials.** With `targets.credentials.mode: tenantAdmin`, the other sections log in to each tenant as its admin, using the password from the `tenants` entry. The first run creates the tenant and configures it; later runs find the generated password in the secrets file and use it again. Keep the secrets file between runs: in CI, commit it encrypted with [sops](docs/sops.md) (remove `*.secrets.yaml` from `.gitignore` and add a `.sops.yaml` creation rule for `\.secrets\.yaml$`), and commit it back after a run which generated a value.

  ```yaml
  tenants:
    - domain: customer-a.eu-latest.cumulocity.com
      company: Customer A
      adminName: admin
      adminEmail: admin@customer-a.example.com
      adminPasswordFrom:
        generate: customer-a-admin
  targets:
    tenants: [customer-a.eu-latest.cumulocity.com]
    credentials:
      mode: tenantAdmin
  ```
- **`--only tenants`** only provisions the tenants: the targets are not resolved and no other section runs. `--only` without `tenants` skips the section.
- Most tenants are not management or enterprise tenants: creating child tenants requires the tenant management permissions of a parent tenant. With the `sessions` [credentials mode](#target-tenants), add a go-c8y-cli session for the new tenant (matched by its domain) before the run which configures it; the default `serviceUser` mode needs no extra step.
- `export` does not include child tenants, since they are not part of the current tenant's configuration.

### Hooks

Pre and post hooks run arbitrary commands around the sync — for example go-c8y-cli calls for anything outside the manifest's scope. Hooks execute via `sh -c` from the manifest directory with the current environment (including the `C8Y_*` session variables) passed through. A failing pre hook aborts the run; post hooks always run (also after section failures) and their failures are reported without aborting. Hooks run regardless of `--only`, and are only printed (not executed) in dry-run mode. In multi-tenant runs hooks execute once per target tenant, with `C8Y_TENANT`, `C8Y_USERNAME` and `C8Y_PASSWORD` overridden to that tenant's resolved credentials (and `C8Y_TOKEN` cleared).

```yaml
hooks:
  pre:
    - name: show session
      run: c8y currenttenant get --select name -o csv
  post:
    - run: c8y inventory list --query "has(c8y_TenantSync)" --select id,name -o csv
```

Hooks can also be scoped to a single section under `hooks.sections.<sectionName>` — they run immediately before/after that section, and (unlike the global hooks) are skipped when the section is excluded by `--only`. The same failure semantics apply: a failing section pre hook aborts the run, post hook failures are reported without aborting.

```yaml
hooks:
  sections:
    software:
      pre:
        - run: ./scripts/build-packages.sh
      post:
        - run: c8y software list --pageSize 100 -o csv
```

### Commands

The `commands` section runs named groups of arbitrary shell commands as the last section — for anything declarative sections don't cover (creating devices, registering external IDs, smoke tests, ...). The **actions within a group run in sequence** (a failing action skips the rest of its group, reported as `skipped`), while **separate groups run concurrently** with each other — so order-dependent steps go in one group, and independent work goes in separate groups:

```yaml
commands:
  - name: seed-devices
    actions:
      - c8y devices create --name "foo"
      - c8y devices create --name "foo2"

  # runs at the same time as seed-devices
  - name: smoke-test
    actions:
      - c8y currenttenant get --select name -o csv
```

Commands execute like hooks: via `sh -c` from the manifest directory, with the `C8Y_*` session variables passed through (scoped to the target tenant in multi-tenant runs), printed but not executed in dry-run mode. Unlike a failing pre hook, a failing action does not abort the run — it fails its group and is reflected in the summary and exit code.

### Device profiles

Device profiles bundle firmware, software and configuration — a profile is essentially a set of links to existing repository items. By default each reference is looked up in the tenant and resolved to its binary URL; that's why the `deviceProfiles` section runs **after** the repository sections, so the referenced items can be synced by the same manifest. References to items outside the manifest must already exist in the tenant, otherwise the profile fails with a lookup error.

Alternatively a reference can set `url` explicitly (e.g. for an externally hosted binary), which skips the repository lookup entirely. The special values `none` and `"-"` also skip the lookup but leave the `url` off the profile entry altogether — useful when the devices resolve artifacts by name/version themselves and the referenced item doesn't exist in the tenant at all:

```yaml
deviceProfiles:
  - name: rpi4-base
    deviceType: raspberrypi4-64
    firmware:
      name: core-image-tedge-raspberrypi4-64
      version: "20240115103000"
    software:
      - name: tedge
        version: 1.6.2
        type: apt # optional software type (disambiguates the lookup)
        action: install
      # An explicit url skips the repository lookup
      - name: sensor-agent
        version: 2.1.0
        url: https://example.com/packages/sensor-agent_2.1.0.deb
      # url: none (or "-") additionally leaves the url off the profile entry
      - name: managed-by-device
        version: 3.0.0
        url: none
    configuration:
      - name: mosquitto
        type: mosquitto.conf
```

#### Selecting versions by semver

Instead of an exact `version`, a firmware or software reference can set `versionSelector` to pick a version from the versions of that item in the tenant, compared by [semantic versioning](https://semver.org) (so `1.10.0` is newer than `1.9.0`):

| `versionSelector` | Selects |
|-------------------|---------|
| `latest` | The highest version |
| `latest-N` | The Nth version before the highest, e.g. `latest-1` is the second highest |
| A semver constraint, e.g. `^1.2`, `~1.6.0`, `">=1.0, <2.0"`, `1.x` | The highest version which matches the constraint |

```yaml
deviceProfiles:
  - name: rpi4-latest
    deviceType: raspberrypi4-64
    firmware:
      name: tedge-rugix-pi
      versionSelector: latest
    software:
      - name: tedge
        type: apt
        versionSelector: ^1.6       # highest 1.x version from 1.6.0
      - name: tedge-inventory-plugin
        versionSelector: latest-1   # the version before the latest
        includePrereleases: true    # also consider versions such as 1.2.0-rc1
```

- `version` is always an exact version, so a version which is literally called `latest` is used with `version: latest`. `version` and `versionSelector` are mutually exclusive.
- The version is selected every time the manifest is applied, after the repository sections, so a profile follows the versions which the same manifest synced. The profile entry contains the selected version (e.g. `1.10.0`), and the profile is only updated when the selection changes.
- Versions which are not valid semver (e.g. `nightly`) are ignored. Versions with a `v` prefix, two parts (`1.2`) or build timestamps (`20261104.2250`) are accepted. Versions which are equal in semver (`1.2` and `1.2.0`) count once.
- Prereleases (a `-` suffix such as `1.2.0-rc1`) are skipped unless `includePrereleases: true` is set. Package revisions written with a `-` (e.g. `1.2.3-1`) also count as prereleases in semver. With constraints, a prerelease is matched by the release it leads up to: `1.5.0-rc1` matches `^1.0`, and `2.0.0-rc1` does not match `<2.0.0`.
- `url: none` still selects the version, and only leaves out the url.
- The profile fails with an error which explains why when no version qualifies.

## Example Output

```
📄 Manifest: tenant.yaml

  ✓ [tenantOptions] configuration/device.bootstrap.enabled (unchanged)
  ↻ [features] feature-branding (updated): enabled
  ✓ [applications] advanced-software-mgmt (unchanged): already subscribed
  ✚ [software] tedge [arm64/apt] (created)
  ✚ [software] tedge [arm64/apt] 1.6.2~584+gd629c53 (created): tedge_1.6.2~584+gd629c53_arm64.deb
  ✓ [firmware] core-image-tedge-rpi4 (unchanged)
  ✚ [firmware] core-image-tedge-rpi4 20240115103000 (created): core-image-tedge-rpi4-20240115103000.rootfs.wic.bz2
  ✚ [configuration] mosquitto (mosquitto.conf) (created): mosquitto.conf
  ↻ [deviceProfiles] rpi4-base (updated)

──────────────────────────────────────────────────
Total: 9 item(s) (5 created, 2 unchanged, 2 updated)
⏱️  Total time: 14.2s
```

## Identifying Synced Items

Managed objects created by this tool carry a `c8y_TenantSync` fragment:

```json
{
  "c8y_TenantSync": {
    "tool": "tenant-sync",
    "syncedAt": "2026-06-10T10:30:00Z"
  }
}
```

The fragment is passed to the SDK upserts as an **annotation**: it is written on create and on every real update, but it is excluded from change detection and never triggers an update by itself. Re-applying an unchanged manifest therefore performs no writes — `syncedAt` records when the desired state last actually changed, not when the tool last ran.

```bash
# List everything managed by tenant-sync
c8y inventory list --query "has(c8y_TenantSync)"
```

## CI/CD Integration

The manifest is designed to live in git next to your artifacts and run from CI. In CI you can run the binary directly with the `C8Y_*` variables, without installing go-c8y-cli:

```yaml
# GitHub Actions example
jobs:
  sync-tenant:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - run: go run github.com/reubenmiller/c8y-tenant-sync@latest run -f tenant.yaml
        env:
          C8Y_BASEURL: ${{ secrets.C8Y_BASEURL }}
          C8Y_TENANT: ${{ secrets.C8Y_TENANT }}
          C8Y_USERNAME: ${{ secrets.C8Y_USERNAME }}
          C8Y_PASSWORD: ${{ secrets.C8Y_PASSWORD }}
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Run the job per tenant (e.g. a matrix over credential sets) to maintain multiple tenants from the same source. Add `--report` to keep a record of each run, see [Run reports](#run-reports).

## Architecture

```
┌──────────────────────────────┐
│ main.go + cmd_{run,init,     │  CLI: cobra commands
│ validate,schema}.go          │
│ runtime.go                   │  c8y session / runtime context
└──────┬───────────────────────┘
       │
┌──────────────┐    ┌──────────────┐
│ manifest.go  │    │  sources.go  │  local dirs, URLs, GitHub releases
└──────┬───────┘    └──────┬───────┘
       │                   │
┌──────────────────────────────────┐
│             sync.go              │  orchestrator, ordering, reporting
│  sync_child_tenants.go           │  child tenant provisioning
│  sync_tenant.go    (options,     │
│                     features,    │
│                     apps)        │
│  sync_users.go     (user groups, │
│                     users, role/ │
│                     group assign)│
│  sync_inventory_roles.go         │  inventory roles + assignments
│  sync_software.go  + parser.go   │  package filename parsing
│  sync_firmware.go  + firmware.go │  OS image filename parsing
│  sync_configuration.go           │
│  sync_smartrest.go               │  exported collection JSON files
│  sync_digitaltwin.go             │  DTM property/asset definitions
│  sync_smartfunctions.go          │  Data Preparation rule packages
│  sync_certificates.go            │  trust store + revocation lists
│  sync_profiles.go                │  resolves references to binary URLs
└──────────────┬───────────────────┘
               │
        ┌────────────┐
        │  go-c8y SDK│  Repository.{Software,Firmware,Configuration},
        └────────────┘  Tenants.Options, Features, Applications,
                        Users, UserGroups, UserRoles,
                        InventoryRoles, DevicePermissions,
                        SmartRestTemplates, TrustedCertificates,
                        ManagedObjects
```

## Development

```bash
# Build the binary into the extension folder and install the folder as a local extension
go build -o bin/c8y-tenant-sync .
c8y extensions install .
c8y tenant-sync --help

# Run the tests, and check the extension for problems
go test ./...
c8y extensions validate . --strict

# Regenerate the manifest schema after changing the Go types
go run . schema -o tenant.schema.json
```

### Releasing

Push a `v*` tag. The [release workflow](.github/workflows/release.yml) runs the tests, validates the extension, and publishes the release archives (`c8y-tenant-sync_<version>_<os>_<arch>.tar.gz`/`.zip` with `checksums.txt`) with [GoReleaser](https://goreleaser.com). `c8y extensions install` picks the archive for the user's platform.

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Future Enhancements

- Prune mode: remove items present in the tenant but absent from the manifest (including group roles and user memberships)
- Checksums/signatures for artifact integrity
- Additional sources (S3, OCI registries, GitLab releases)
- Watch mode for continuous reconciliation
