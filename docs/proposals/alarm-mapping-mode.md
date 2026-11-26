# Proposal: additive or exact sync of the alarm mapping

Status: implemented

## Summary

The `alarmMapping` section is additive: it creates and updates the mappings it lists, deletes the ones marked `state: absent`, and leaves every other alarm type in the tenant alone. That is a safe default, but it can't make the manifest the full source of truth. A mapping someone adds in the UI, or one removed from the manifest without first changing it to `state: absent`, stays in the tenant.

This proposal lets a manifest choose how the section is synced:

- `additive` (the default, today's behavior): only the listed mappings are created, updated or deleted.
- `exact`: the listed mappings become the tenant's complete alarm mapping. Mappings of alarm types that the manifest doesn't list are deleted.

## Proposed format

`alarmMapping` becomes an object with a `mode` and the list of `mappings`:

```yaml
alarmMapping:
  mode: exact              # additive (default) | exact
  mappings:
    - type: c8y_NoisyAlarm
      severity: NONE
    - type: c8y_UnavailabilityAlarm
      severity: MINOR
      description: Device not reachable
```

The list form stays valid as a shorthand for `mode: additive`, so existing manifests and exports keep working unchanged:

```yaml
alarmMapping:                # the same as {mode: additive, mappings: [...]}
  - type: c8y_NoisyAlarm
    severity: NONE
```

The mapping entries are unchanged: `type`, `severity` (case-insensitive), `description` and `state`.

### Why this shape

- **The mode sits next to the data it governs.** Reviewers see `mode: exact` in the same place as the list it applies to, and the setting travels with the section through includes and `--only`.
- **The vocabulary is the one the manifest already uses.** `users[].inventoryRolesMode` takes `additive` (default) or `exact`, with the same meaning: exact also removes what the manifest no longer declares.
- **No breaking change.** A list still decodes, so only manifests that want `exact` need the object form.

## Behavior

### Exact mode

1. Read the category once (`GET /tenant/options/alarm.type.mapping`).
2. Create and update the listed mappings with one `PUT`, as today.
3. Delete each mapping in the tenant whose type is not listed (one `DELETE /tenant/options/alarm.type.mapping/{type}` each; Cumulocity has no bulk delete for tenant options).

- `state: absent` entries are allowed and have the same effect as leaving the type out. They stay useful in a base file that is also included in additive manifests.
- `mappings: []` (or a missing `mappings`) with `mode: exact` deletes the whole alarm mapping. That is explicit in the manifest, so it isn't refused, but the dry run lists every mapping it would delete.
- The run output reports each deleted type as `deleted` with its old value, for example `c8y_OldAlarm (deleted): not in the manifest (was severity MAJOR (keep description))`.

### Dry run

The dry run already reads the mapping when a session is available. In exact mode, it also lists the mappings it would delete. Without a session it can't know which mappings exist, so it prints one `planned` line saying that alarm types not listed would be deleted.

### Includes and several manifests

- `mappings` are merged by `type`, as today (the later file wins).
- `mode` is taken from the last file that sets it. A file with the list shorthand doesn't set a mode, so an additive overlay can add mappings to a base file with `mode: exact` without turning exact mode off.

### Export

`export` keeps writing the list shorthand (additive). Applying an export to the same tenant changes nothing in either mode, so there is no reason to opt the user into deletes. They can add `mode: exact` when they decide the manifest owns the mapping.

### Validation and schema

- `mode` must be `additive` or `exact`.
- With `mode: exact`, `tenantOptions` can't set options of the `alarm.type.mapping` category, since the section would delete them on every run.
- The schema accepts `alarmMapping` as either an array of entries or an object with `mode` and `mappings` (`oneOf`), with no other properties.

## Implementation notes

- `Manifest.AlarmMapping` becomes `*AlarmMappingSectionSpec` with `Mode string` and `Mappings []AlarmMappingSpec`, and an `UnmarshalYAML` that accepts a sequence (shorthand) or a mapping (strict decoding with known fields, like `ApplicationExtensionSpec`).
- `MarshalManifest` writes the shorthand when the mode is empty or `additive`.
- `mergeManifests` merges `Mappings` with `mergeByKey` on `type` and takes `Mode` from `over` when set.
- `SyncAlarmMapping` collects the types to delete after the `PUT`, and reuses `deleteAlarmMapping` for them.
- Tests: shorthand and object decode to the same mappings; exact deletes unlisted types only; an exact dry run with and without a session; include merge of `mode`; export round trip; schema accepts both forms.

## Alternatives considered

- **A top-level setting such as `alarmMappingMode: exact`.** Smaller change, but it separates the setting from the section, and includes and `--only` would have to treat a second top-level key as part of `alarmMapping`.
- **A generic `syncModes:` block for all sections.** Worth revisiting if several sections need exact mode. Today the modes mean different things per section (users have `ifMissing` and `inventoryRolesMode`; Digital Twin Manager definitions have `prune` with `managedBy`), so a shared block would hide those differences.
- **A CLI flag (`run --exact alarmMapping`).** Not reviewable in the manifest, and the same manifest would sync differently depending on who runs it.
- **Ownership tagging, like `digitalTwin.managedBy` with `prune`.** Tenant options have no field to tag who created them, so exact mode can't limit deletes to mappings the manifest created. Exact mode means the manifest owns the whole category; mappings added in the UI are deleted on the next run. The README should say this clearly.
