# Secrets with SOPS

tenant-sync uses [SOPS](https://github.com/getsops/sops) for two things:

1. **Encrypted manifests**: secrets in a manifest (user passwords, tenant option values, inline configuration, GitHub tokens) can be encrypted, so the manifest can be kept in git. tenant-sync detects an encrypted manifest and decrypts it before applying it.
2. **The secrets file of generated values**: values of `${generate:NAME}` references are stored in `<manifest>.secrets.yaml`, which is encrypted with SOPS when it's set up.

tenant-sync runs the `sops` executable, so every key type SOPS supports works (age, PGP, AWS/GCP/Azure KMS, HashiCorp Vault) with your existing SOPS setup. This guide uses [age](https://github.com/FiloSottile/age), the simplest option.

- [Install sops and age](#install-sops-and-age)
- [Create an age key](#create-an-age-key)
- [Where sops finds the key](#where-sops-finds-the-key)
- [Configure .sops.yaml](#configure-sopsyaml)
- [Encrypt a manifest](#encrypt-a-manifest)
- [Secret references and encrypted_regex](#secret-references-and-encrypted_regex)
- [Edit an encrypted file](#edit-an-encrypted-file)
- [Encrypt the secrets file of generated values](#encrypt-the-secrets-file-of-generated-values)
- [Read a generated value](#read-a-generated-value)
- [Share with a team and rotate keys](#share-with-a-team-and-rotate-keys)
- [Use SOPS in CI](#use-sops-in-ci)
- [Troubleshooting](#troubleshooting)
- [What to commit](#what-to-commit)

## Install sops and age

```bash
# macOS
brew install sops age

# Debian/Ubuntu (age from apt, sops from its GitHub release)
sudo apt-get install -y age
sudo curl -fsSL -o /usr/local/bin/sops https://github.com/getsops/sops/releases/download/v3.9.4/sops-v3.9.4.linux.amd64
sudo chmod +x /usr/local/bin/sops
```

tenant-sync works with sops 3.7 and newer. To use another `sops` executable, set `TENANT_SYNC_SOPS` to its path.

## Create an age key

An age key has a private part (in a key file, keep it secret) and a public part (the *recipient*, e.g. `age1...`), which is used to encrypt.

If you already have a key file, use it: creating a new key in the same file would make everything encrypted with the old key unreadable. Check first:

```bash
cat ~/.config/sops/age/keys.txt | grep 'public key'
```

Only if there is no key yet:

```bash
mkdir -p ~/.config/sops/age
age-keygen -o ~/.config/sops/age/keys.txt
chmod 600 ~/.config/sops/age/keys.txt
```

`age-keygen` prints the public key (`Public key: age1...`); it's also in a comment in the key file.

> **Back up the key file** (e.g. in your password manager). Without it, nothing encrypted for it can be decrypted again, including the generated passwords in the secrets file.

## Where sops finds the key

sops reads age keys from, in this order:

| Source | Use |
|--------|-----|
| `SOPS_AGE_KEY` | The private key itself (e.g. from a CI secret) |
| `SOPS_AGE_KEY_FILE` | Path of a key file |
| The default key file | `~/.config/sops/age/keys.txt` on Linux; on macOS, older sops versions (e.g. 3.7) only look in `~/Library/Application Support/sops/age/keys.txt` |

To be independent of the sops version, set `SOPS_AGE_KEY_FILE` in your shell profile (e.g. `~/.zshrc`):

```bash
export SOPS_AGE_KEY_FILE="$HOME/.config/sops/age/keys.txt"
```

c8y passes your environment to the extension, so `c8y tenant-sync` and `sops` both use it.

## Configure .sops.yaml

`.sops.yaml` holds the *creation rules*: which key encrypts which files, and which fields are encrypted. sops looks for it from its working directory upwards, and tenant-sync encrypts the secrets file from the folder of the file, so put it in the folder of your manifests (or a parent folder, e.g. the repository root).

```yaml
# .sops.yaml
creation_rules:
  # Secrets files of generated values (<manifest>.secrets.yaml): encrypt all values
  - path_regex: \.secrets\.yaml$
    age: age1...your-public-key

  # Manifests: only encrypt the secret fields, so the rest stays readable
  - path_regex: (tenant|manifest)\.yaml$
    encrypted_regex: ^(password|value|token|content)$
    age: age1...your-public-key
```

- The first rule whose `path_regex` matches the file is used. `path_regex` is matched against the full file path, so use patterns ending with `$` rather than starting with `^`.
- A rule without `path_regex` applies to every file.
- `encrypted_regex` limits the encryption to fields with matching names. Without it, all values are encrypted (the keys stay readable).
- A rule can have several recipients, separated by commas (see [Share with a team](#share-with-a-team-and-rotate-keys)).

## Encrypt a manifest

```bash
# Encrypt in place (uses the matching creation rule)
sops --encrypt --in-place tenant.yaml

# Edit it: decrypts into your editor and encrypts again when you save
sops tenant.yaml

# Show the decrypted manifest
sops --decrypt tenant.yaml
```

tenant-sync decrypts the manifest automatically (it detects the `sops` section which sops adds):

```bash
c8y tenant-sync validate tenant.yaml
c8y tenant-sync run -f tenant.yaml --dry
c8y tenant-sync run -f tenant.yaml
```

- `${VAR}` references are expanded after decryption, so both can be combined: encrypt the values you keep in git, and inject others from the environment.
- `${generate:NAME}` references also work in encrypted manifests.
- Encrypt string fields only: an encrypted number or boolean becomes an `ENC[...]` string, which the editor's schema validation reports (tenant-sync decrypts it before validating, so the run works). The schema accepts the `sops` section.

## Secret references and encrypted_regex

`encrypted_regex` selects fields by **name**, wherever they are. A rule with `^(value|password|token|content)$` encrypts every `value`, also tenant option values which are not secret, and a `${VAR}` written in such a field is encrypted too (it still works, since tenant-sync expands it after decrypting, but it can no longer be read in the file).

Use [secret references](../README.md#secret-references) for values which come from the environment or are generated. They are separate fields (`valueFrom`, `passwordFrom`, `tokenFrom`), so the rule leaves them readable and only encrypts literal secrets:

```yaml
tenantOptions:
  - category: configuration
    key: device.bootstrap.enabled
    value: "false"                   # encrypted (a value field), although not secret
  - category: integration
    key: api.secret
    valueFrom:
      env: INTEGRATION_SECRET        # readable: not a value field
users:
  - email: svc-dashboard@example.invalid
    passwordFrom:
      generate: svc-dashboard-password # readable; the value is in the encrypted secrets file
```

If non-secret tenant options should stay readable too, leave `value` out of `encrypted_regex` and give every secret tenant option a `valueFrom` reference instead of a literal value.

Alternatively, keep the manifest unencrypted and put the secret values in a sops-encrypted dotenv file, which `sops exec-env` provides as environment variables for `env` references and `${VAR}`:

```yaml
# .sops.yaml
creation_rules:
  - path_regex: \.enc\.env$
    age: age1...your-public-key
```

```bash
sops secrets.enc.env                    # create/edit: INTEGRATION_SECRET=...
sops exec-env secrets.enc.env 'c8y tenant-sync run -f tenant.yaml'
```

## Edit an encrypted file

Always edit an encrypted file with sops, also its readable fields: sops stores a checksum (MAC) over **all** values of the file, so editing the file directly makes it unreadable (`MAC mismatch`).

```bash
# Edit in your editor (e.g. VS Code): decrypts, and encrypts again on save
EDITOR="code --wait" sops tenant.yaml

# Change a single value
sops --set '["tenantOptions"][0]["value"] "true"' tenant.yaml
```

- Don't paste `ENC[...]` text into the decrypted file: sops encrypts it again, and the field then decrypts to the `ENC[...]` text instead of the value.
- Don't edit the `sops` section (e.g. `encrypted_regex`) by hand. To change which fields are encrypted, decrypt the file, change the rule in `.sops.yaml` and encrypt it again.
- sops 3.9 and newer can limit the checksum to the encrypted values (`mac_only_encrypted: true` in the creation rule), so readable fields can be edited directly.

If a file was edited directly, decrypt it while ignoring the checksum and encrypt it again (run the decrypt in a folder without a `.sops.yaml`, since sops before 3.8 also reads it to decrypt):

```bash
sops --decrypt --ignore-mac tenant.yaml > /tmp/tenant.plain.yaml
cp /tmp/tenant.plain.yaml tenant.yaml && sops --encrypt --in-place tenant.yaml && rm /tmp/tenant.plain.yaml
```

Inline configuration with secrets works the same way:

```yaml
configuration:
  - name: mosquitto
    configurationType: mosquitto.conf
    force: true
    content: |          # encrypted with encrypted_regex: ^(...|content)$
      listener 8883
      password_file /etc/mosquitto/passwd
```

## Encrypt the secrets file of generated values

The secrets file (`tenant.yaml` → `tenant.secrets.yaml`, or `--secrets-file`) is written after a run which generated values. `--secrets-encryption` decides whether it's encrypted:

| `--secrets-encryption` | Behaviour |
|------------------------|-----------|
| `auto` (default) | Encrypted when `sops` is installed and a creation rule of the nearest `.sops.yaml` matches the secrets file. An encrypted secrets file stays encrypted. Otherwise it's written unencrypted, with a warning when a `.sops.yaml` exists but no rule matches. |
| `sops` | Must be encrypted: the run fails if `sops`, a `.sops.yaml` or a matching rule is missing |
| `none` | Never encrypted. Fails if the existing secrets file is encrypted |

```bash
c8y tenant-sync run -f tenant.yaml                            # auto
c8y tenant-sync run -f tenant.yaml --secrets-encryption sops  # require encryption
c8y tenant-sync run -f tenant.yaml --secrets-encryption none  # turn it off
```

The run reports how the file was saved:

```
🔐 Generated 1 value(s): svc-dashboard-password
   Saved to tenant.secrets.yaml (encrypted with sops)
```

The file is always written with permissions `0600`. If encryption fails (e.g. the key is invalid), the values are kept: the file is saved unencrypted (`0600`) and the run fails, so you can fix the setup and encrypt it with `sops --encrypt --in-place tenant.secrets.yaml`.

## Read a generated value

The secrets file holds each value with the time it was generated, the host and the tenant ID:

```bash
# All values
sops --decrypt tenant.secrets.yaml

# A single value, e.g. to log in as the user
sops --decrypt --extract '["secrets"]["svc-dashboard-password"]["value"]' tenant.secrets.yaml
```

For an unencrypted secrets file, read it directly (e.g. `yq '.secrets.svc-dashboard-password.value' tenant.secrets.yaml`).

## Share with a team and rotate keys

Each team member (and CI) has their own age key. Add all public keys to the creation rules, separated by commas:

```yaml
creation_rules:
  - path_regex: \.secrets\.yaml$
    age: >-
      age1alice...,
      age1bob...,
      age1ci...
```

After changing the recipients, re-encrypt the existing files for the new set of keys:

```bash
sops updatekeys tenant.yaml
sops updatekeys tenant.secrets.yaml
```

To rotate the data key of a file (e.g. after removing someone), run `sops --rotate --in-place <file>`. Values the removed person could read (such as generated passwords) should also be changed: remove the entry from the secrets file to generate a new value on the next run. Note that passwords are only set when a user is created, so change existing users' passwords in Cumulocity.

## Use SOPS in CI

Create an age key for CI, add its public key to `.sops.yaml` (and run `sops updatekeys`), and store the private key (the `AGE-SECRET-KEY-...` line) as a CI secret. sops reads it from `SOPS_AGE_KEY`:

```yaml
# GitHub Actions
jobs:
  sync-tenant:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - name: Install sops
        run: |
          sudo curl -fsSL -o /usr/local/bin/sops https://github.com/getsops/sops/releases/download/v3.9.4/sops-v3.9.4.linux.amd64
          sudo chmod +x /usr/local/bin/sops
      - run: go run github.com/reubenmiller/c8y-tenant-sync@latest run -f tenant.yaml --secrets-encryption sops
        env:
          SOPS_AGE_KEY: ${{ secrets.SOPS_AGE_KEY }}
          C8Y_BASEURL: ${{ secrets.C8Y_BASEURL }}
          C8Y_TENANT: ${{ secrets.C8Y_TENANT }}
          C8Y_USERNAME: ${{ secrets.C8Y_USERNAME }}
          C8Y_PASSWORD: ${{ secrets.C8Y_PASSWORD }}
```

If the run generates new values, the secrets file changes in the CI workspace. Commit it back (it's encrypted), or generate the values once locally and commit the encrypted secrets file before CI runs, so CI only reuses them. `--secrets-encryption sops` makes sure CI never writes an unencrypted secrets file.

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| `Failed to get the data key required to decrypt the SOPS file` | sops can't find a key which can decrypt the file. Set `SOPS_AGE_KEY_FILE` (see [Where sops finds the key](#where-sops-finds-the-key)), or ask someone with access to add your public key and run `sops updatekeys`. |
| `MAC mismatch` | The file was edited directly instead of with sops, see [Edit an encrypted file](#edit-an-encrypted-file). |
| `has values which are still encrypted after decrypting it, i.e. encrypted twice` | A value was encrypted twice (e.g. `ENC[...]` text was pasted into the decrypted file). The error names each value. Restore it with `sops --set` or by editing with `sops`. |
| `contains sops encrypted values but no sops metadata` | `ENC[...]` values were copied into a file without a `sops` section (e.g. from another encrypted file). An `ENC[...]` value only decrypts in the file it was encrypted in, so replace each named value with the plain text and encrypt the file with sops, or use a [secret reference](../README.md#secret-references). Without this check, the text would be used as the value, e.g. as a GitHub token failing with `401 Bad credentials`. |
| `environment variable X is not set or empty` | An `env` reference (`valueFrom`, `passwordFrom`, `tokenFrom`) names a variable which is not set. Set it, or provide it with `sops exec-env`. |
| `no matching creation rules found` (from `sops --encrypt`) | No rule of `.sops.yaml` matches the file. Add a rule (e.g. `path_regex: \.secrets\.yaml$`), and check that sops runs in (or below) the folder of `.sops.yaml`. |
| `⚠ no creation rule in .sops.yaml matches tenant.secrets.yaml, so it is saved unencrypted` | `--secrets-encryption auto` found a `.sops.yaml` without a rule for the secrets file. Add one, then encrypt the file with `sops --encrypt --in-place tenant.secrets.yaml`. |
| `... is encrypted with sops, but the sops executable was not found` | Install sops, or set `TENANT_SYNC_SOPS` to its path. |
| `... is encrypted with sops, but --secrets-encryption is none` | The secrets file is encrypted. Use `auto` or `sops`, or decrypt it with `sops --decrypt --in-place` first. |
| `the secrets were saved unencrypted ... because sops encryption failed` | The values are safe in the unencrypted file (`0600`). Fix the sops setup, then run `sops --encrypt --in-place <file>`. |
| Schema errors for `ENC[...]` values in the editor | An encrypted non-string field. Limit `encrypted_regex` to string fields. |

## What to commit

| File | Commit? |
|------|---------|
| `.sops.yaml` | Yes: it only contains public keys |
| Encrypted manifests | Yes |
| `<manifest>.secrets.yaml`, encrypted | Yes, if CI or others need the generated values. `init` adds `*.secrets.yaml` to `.gitignore`; remove that line (or force-add the file) to commit it |
| `<manifest>.secrets.yaml`, unencrypted | Never |
| Age key files (`keys.txt`, `AGE-SECRET-KEY-...`) | Never |
