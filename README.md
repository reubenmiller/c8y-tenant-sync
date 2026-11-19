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
- 🧩 **Device profiles**: declare firmware/software/configuration bundles; binary URLs are resolved from the tenant automatically
- 🎛️ **Tenant options & feature toggles**: set options, enable/disable features, subscribe/unsubscribe applications
- 🗑️ **Retention rules**: ensure retention rules exist, matched by their selector (dataType/fragmentType/type/source) with the retention period updated when it differs
- 📜 **Trusted certificates**: upload device CA certificates (PEM/DER) into the trust store — matched by fingerprint, with name/status/auto-registration updated when they differ — plus certificate revocation lists from CSV files, and enabling or disabling the tenant's Cumulocity CA
- 👥 **Users & user groups**: create user groups with roles, create users and assign them to groups — additively and idempotently
- 📡 **SmartREST 2.0 templates**: sync template collections from JSON files exported by the platform — matched by external identity, updated only when the templates differ (order-insensitive)
- 🏢 **Target tenants**: apply one manifest to all child tenants, an explicit list, or tenants matching a selector — with per-tenant credentials resolved automatically
- 🔎 **Dry-run**: preview every change before applying
- 🔐 **Env var expansion**: `${VAR}` references in the manifest (e.g. GitHub tokens); `$$` is a literal `$`
- 🔒 **SOPS**: manifests encrypted with [SOPS](https://github.com/getsops/sops) (age, PGP, cloud KMS, Vault) are decrypted automatically
- 📤 **Export**: create a manifest from an existing tenant (tenant options, feature toggles, retention rules, the Cumulocity CA and subscribed microservices)

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
| `--only` | *(all)* | Comma-separated sections to apply: `tenantOptions`, `features`, `retentionRules`, `certificateAuthority`, `trustedCertificates`, `certificateRevocationLists`, `applications`, `userGroups`, `users`, `software`, `firmware`, `configuration`, `smartrestTemplates`, `deviceProfiles`, `commands` |
| `--force` | | Replace existing version binaries / configuration files |
| `--concurrency` | `5` | Concurrent software version uploads (1–20) |
| `--target` | | Apply to a tenant referenced by ID or domain; repeatable or comma-separated (overrides the manifest `targets` section) |
| `--all-children` | | Apply to all child tenants of the current tenant (overrides the manifest `targets` section) |
| `--target-selector` | | Apply to tenants matching criteria, e.g. `domain=*.example.com,company=ACME` (overrides the manifest `targets` section) |
| `--include-current` | | Also include the current tenant when other target flags are used |
| `--credentials-mode` | | How credentials for other tenants are obtained: `serviceUser` or `sessions` (overrides the manifest) |
| `--verbose` | | Detailed logging |
| `--debug` | | Verbose logging + HTTP request/response details |

Sections are applied in a fixed order — tenant options, features, retention rules, Cumulocity CA, trusted certificates, certificate revocation lists, applications, user groups, users, software, firmware, configuration, SmartREST templates, device profiles, then commands — so users can be assigned to groups created in the same run, profiles can reference repository items synced in the same run, and custom commands can build on everything the manifest declares.

### Secrets with SOPS

Manifests can be encrypted with [SOPS](https://github.com/getsops/sops), so secrets such as user passwords, tenant option values or GitHub tokens can be kept in git. tenant-sync detects encrypted manifests (by the `sops` section which SOPS adds) and decrypts them with the `sops` executable before applying them, so every key type SOPS supports works with your existing SOPS setup (for example `SOPS_AGE_KEY_FILE` for age keys). Plain manifests don't need `sops`.

Encrypt only the secret fields, so the rest of the manifest stays readable and reviewable. For example with a `.sops.yaml` next to the manifest:

```yaml
# .sops.yaml
creation_rules:
  - path_regex: tenant\.yaml$
    encrypted_regex: ^(password|value|token)$
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

| Flag | Description |
|------|-------------|
| `-o`, `--output` | Write the manifest to a file instead of stdout |
| `--sections` | Comma-separated sections to export (default: all of the above) |
| `--all-features` | Export all feature toggles, not only the ones set for the tenant |
| `--application-type` | Types of subscribed applications to export (default: `MICROSERVICE`) |
| `--force` | Overwrite the output file |

`$` in exported values is written as `$$`, so values are not expanded as environment variables when the manifest is loaded. Review the manifest before applying it to other tenants: tenant options can contain values which only apply to the exported tenant.

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

### Tenant option lookups

Tenant option values can be resolved by reference at apply time instead of hardcoding IDs, using `valueFrom` with exactly one reference (`application` resolves a name to the application ID, `device` to a device managed object ID):

```yaml
tenantOptions:
  - category: application
    key: default.application
    valueFrom:
      application: devicemanagement
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

> **Note**: (un)subscribing an application that the tenant does not own fails with `403 security/Forbidden` — such subscriptions are managed by the application's owner tenant. Run `c8y tenant-sync` from the owner (parent) tenant instead, typically with a [targets section](#target-tenants) selecting the tenants to maintain.

With a `source`, the application is also created when missing (type defaults to `MICROSERVICE`, `contextPath` to the name) and its binary (a single zip) uploaded. Since binary content cannot be compared, the upload only happens on creation — or on every run with `--force`:

```yaml
applications:
  - name: my-microservice
    type: MICROSERVICE
    source:
      path: ./build/my-microservice.zip
```

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
  - userName: jdoe@example.com
    email: jdoe@example.com
    firstName: Jane
    lastName: Doe
    sendPasswordResetEmail: true # email a reset link instead of setting a password
    groups: [operators]          # groups synced above or already in the tenant

  - userName: svc-dashboard
    password: ${DASHBOARD_USER_PASSWORD} # injected from the environment
    groups: [operators]

  - userName: old-account@example.com
    enabled: false # keep the account but disable it
```

User groups are synced before users, so `users[].groups` can reference groups declared in the same manifest (or groups that already exist in the tenant). Roles referenced by `userGroups[].roles` must exist in the tenant — note that some roles only appear after subscribing the microservice that provides them, which the `applications` section (applied earlier) can take care of.

For existing users only the fields set in the manifest are compared and updated (empty fields are left alone), with two deliberate exceptions:

- **`password` is only used when creating the user** and never updated afterwards, so re-running the sync doesn't reset credentials. Use `${VAR}` expansion to keep secrets out of the manifest, or prefer `sendPasswordResetEmail: true` to avoid handling passwords entirely.
- **`enabled` defaults to true** and is always managed: a user disabled in the UI is re-enabled on the next sync unless the manifest says `enabled: false`.

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
- **`sessions`** reads [go-c8y-cli session files](https://goc8ycli.netlify.app/docs/gettingstarted/#sessions) and matches them to each target by tenant ID and then by domain. Encrypted sessions are not supported — the run fails with a clear error listing tenants without a matching session.

In multi-tenant runs every result line and the hook environment (`C8Y_TENANT`, `C8Y_USERNAME`, `C8Y_PASSWORD`) are scoped to the tenant being applied:

```
🏢 Tenant: t12345
  ✓ [applications] t12345: cloud-remote-access (unchanged): not subscribed
  ↻ [features] t12345: mqtt-service.smartrest (updated): enabled
```

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

Run the job per tenant (e.g. a matrix over credential sets) to maintain multiple tenants from the same source.

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
│  sync_tenant.go    (options,     │
│                     features,    │
│                     apps)        │
│  sync_users.go     (user groups, │
│                     users, role/ │
│                     group assign)│
│  sync_software.go  + parser.go   │  package filename parsing
│  sync_firmware.go  + firmware.go │  OS image filename parsing
│  sync_configuration.go           │
│  sync_smartrest.go               │  exported collection JSON files
│  sync_certificates.go            │  trust store + revocation lists
│  sync_profiles.go                │  resolves references to binary URLs
└──────────────┬───────────────────┘
               │
        ┌────────────┐
        │  go-c8y SDK│  Repository.{Software,Firmware,Configuration},
        └────────────┘  Tenants.Options, Features, Applications,
                        Users, UserGroups, UserRoles,
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
