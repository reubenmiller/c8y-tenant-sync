# Proposal: full lifecycle sync of inventory roles and their assignments

Status: implemented
Related: [reubenmiller/go-c8y-cli#172](https://github.com/reubenmiller/go-c8y-cli/issues/172) (commands to manage inventory roles)

## Summary

Users asked for go-c8y-cli commands to manage inventory roles and their assignments. Commands help with one-off changes, but the real job is usually "make sure these roles exist, and these people have them on these sites, in every tenant". That is a desired-state problem, which tenant-sync is built for.

tenant-sync already covered the basics before this proposal (see [Starting point](#starting-point)). This proposal keeps the current split:

- the `inventoryRoles` section owns the **role definitions**;
- `users[].inventoryRoles` owns **who gets which role on which group**, so roles can be given when a user is created, next to their global roles.

It closes the gaps that stop the manifest from being the source of truth:

1. Name the user's roles as the UI does: `users[].globalRoles` (formerly `groups`) and `users[].inventoryRoles` (formerly `inventoryAssignments`).
2. Role lifecycle: delete (`state: absent`) and rename (`renamedFrom`).
3. An opt-in `exact` mode on a user's assignments, which also **revokes** access the manifest no longer declares.
4. User `state` (`present`, `absent`) and `ifMissing` (`create`, `skip`, `fail`): manage users without creating them (for example SSO users), and delete users who are offboarded.
5. A real dry-run diff against the tenant.
6. Export of assignments into the `users` section.

The go-c8y-cli commands are still worth adding for ad-hoc use and scripting; [CLI commands](#cli-commands-go-c8y-cli172) lists what they should cover.

## Starting point

What tenant-sync supported before this proposal, and what it supports now:

| Capability | Before | Now |
| --- | --- | --- |
| Create inventory roles by name | Supported (`inventoryRoles`) | Unchanged |
| Update description and permissions (permissions compared as a set and replaced) | Supported | Unchanged |
| Delete or rename roles | Not supported; roles were never deleted | `state: absent` and `renamedFrom` |
| Give a user roles on a device group (name or `Parent/Child` path), also when creating the user | Supported (`users[].inventoryAssignments`) | Renamed to `users[].inventoryRoles` |
| Revoke roles or assignments not in the manifest | Not supported; assignments were additive | `inventoryRolesMode: exact` per user |
| Assign roles to a user without creating the user if missing | Not supported | `ifMissing: skip` |
| Dry run | Printed "ensure inventory role" for every role, no comparison | Diff against the tenant |
| Export | Roles only; assignments were skipped | Opt-in `users` export with global and inventory roles |

Code: [sync_inventory_roles.go](../../sync_inventory_roles.go), [sync_users.go](../../sync_users.go), [manifest.go](../../manifest.go) (`InventoryRoleSpec`, `InventoryAssignmentSpec`, `UserSpec`), [cmd_export.go](../../cmd_export.go).

## Problems this solves

- **Role cleanup.** A role removed from the manifest stays in every tenant, and renaming a role in the manifest creates a second role while the old one keeps its assignments.
- **Offboarding and site changes.** Moving a user from Plant Berlin to Plant Paris in the manifest adds Paris but keeps Berlin. Someone has to delete the old assignment in the UI, in every tenant. Access only ever grows.
- **SSO users.** Users created by single sign-on on first login should not be created by tenant-sync, but they still need inventory roles. Today a `users` entry for such a user tries to create them when missing (and fails without an email or password).
- **Drift review.** A dry run cannot show what would change, so a pull request that edits a role or an assignment gives reviewers nothing to check.

## Proposed design

### 1. Name the user's roles as the UI does: `globalRoles` and `inventoryRoles`

The Cumulocity UI shows two kinds of roles on a user: **global roles**, which apply to the whole tenant, and **inventory roles**, which apply on a device group. The manifest calls them `users[].groups` and `users[].inventoryAssignments`, which matches neither the UI nor each other. Rename both:

| Today | Proposed |
| --- | --- |
| `users[].groups` | `users[].globalRoles` |
| `users[].inventoryAssignments` | `users[].inventoryRoles` |

```yaml
users:
  - email: berlin-ops@example.com
    globalRoles: [operators]          # tenant-wide
    inventoryRoles:                   # per device group
      - group: Plant Berlin
        roles: [Site operator]
```

The values keep their shape: `globalRoles` is a list of names, `inventoryRoles` a list of `group` and `roles`. The project is still new, so the fields are renamed outright with no alias: the schema, README, `tenant.example.yaml` and export use the new names, and a manifest with the old names fails validation as an unknown field.

In Go, `UserSpec.Groups` becomes `UserSpec.GlobalRoles` and `UserSpec.InventoryAssignments` becomes `UserSpec.InventoryRoles` (still a list of `InventoryAssignmentSpec`, since each entry is an assignment of roles to a group).

**The top-level `userGroups` section.** The UI calls these global roles too (the code already notes this on `UserGroupSpec`). For the same reason, rename the section to `globalRoles`, so the manifest has two matching pairs: the `globalRoles` and `inventoryRoles` sections define the roles, and `users[].globalRoles` and `users[].inventoryRoles` give them to users. A global role's own `roles` field (the `ROLE_*` permissions) becomes `permissions`, which matches the UI and the `permissions` of an inventory role. The section, the `--only` value and `SectionUserGroups` are renamed together.

```yaml
globalRoles:
  - name: operators
    description: Operators of all sites
    permissions: [ROLE_INVENTORY_READ, ROLE_ALARM_ADMIN]
```

**SSO access rules.** `loginOptions.sso.accessMappings.rules[].groups` also lists global roles (see `SSOAccessRuleSpec` in [manifest_login_options.go](../../manifest_login_options.go)). Rename it to `globalRoles` in the same change, so a global role has one name everywhere in the manifest.

**Manifest names only.** These renames change the manifest format, not what is sent to Cumulocity. tenant-sync keeps translating to the API's own shape: user groups at `/user/{tenant}/groups` with role references, user group memberships, inventory assignments at `.../roles/inventory`, and `thenGroups` in the SSO access mapping. Export translates back the same way.

### 2. Role lifecycle (`inventoryRoles`)

```yaml
inventoryRoles:
  - name: Site operator
    renamedFrom: [Site operators, Operator (site)]
    permissions:
      - {scope: ALARM, permission: ADMIN}
      - {scope: OPERATION, permission: ADMIN}
  - name: Legacy viewer
    state: absent
```

- `renamedFrom`: when no role has `name` but exactly one role has a previous name, rename it instead of creating a duplicate. Assignments keep working because they reference the role ID. If both the new and an old name exist, the role fails as ambiguous.
- `state: absent` (default `present`): delete the role if it exists. Validation fails if a role marked absent is still referenced in `users[].inventoryRoles`.
- There is no section-wide prune. Inventory roles have no field to tag ownership with (unlike Digital Twin Manager definitions with `managedBy`), so deleting "every role not in the manifest" would also delete roles other teams create in the UI.

#### Order: roles before users

The section order already handles this and stays as it is. For each target tenant, all sections run in `SectionOrder` ([sync.go](../../sync.go)): `deviceGroups`, then `userGroups` (proposed: `globalRoles`), then `inventoryRoles`, then `users`. So by the time a user's `inventoryRoles` are applied, every declared role has been created, renamed or updated in that tenant, and a new user can get a role created in the same run.

Edge cases to keep (and test):

- **A role that failed to create.** The user referencing it fails with "inventory role not found"; the user's other fields and assignments are still applied.
- **`--only users`.** The `inventoryRoles` section is skipped, so referenced roles must already exist in the tenant. (A warning when `--only` leaves out a section the selected ones depend on is not implemented.)
- **Renamed roles.** Users reference the new name. Because the rename runs first, the new name resolves to the existing role ID and its assignments are kept.
- **Deleted roles.** `state: absent` is applied in the `inventoryRoles` section, before `users`. Validation fails if any user still references a role marked absent, so a run never deletes a role and then tries to assign it.
- **Dry run.** Nothing is created, so a role declared in the manifest but missing in the tenant is marked `(new role)` for the user, not "not found".

### 3. `exact` mode for a user's assignments

```yaml
users:
  - email: berlin-ops@example.com
    globalRoles: [operators]
    inventoryRolesMode: exact           # additive (default) | exact
    inventoryRoles:
      - group: Europe/Plant Berlin
        roles: [Site operator]
      - group: Europe/Plant Paris
        roles: [Restart only]
```

With `exact`, the listed assignments become the user's full inventory access:

- On each listed group, the user's roles are set to exactly the declared roles (added and removed).
- An assignment on a group which is not listed is deleted.
- `inventoryRolesMode: exact` with an empty or missing `inventoryRoles` removes all of the user's inventory access. Validation warns about this so it is not done by accident.
- Only users with `exact` are affected; all other users stay additive, as before.

Safety rails:

- `additive` stays the default. Nothing is ever removed unless a user opts in.
- The dry run lists every revocation per group and role.
- The tenant-sync service user cannot be given `exact`, so the run cannot lock itself out.

`exact` applies to inventory roles only. Global roles stay additive, with no mode field (see [Decisions](#decisions)).

### 4. User `state` and `ifMissing`

Two fields, each answering one question:

- `state`: should the user exist? `present` (default) or `absent`, as for inventory roles.
- `ifMissing`: what tenant-sync does when a `present` user does not exist yet. `create` (default), `skip` or `fail`.

```yaml
users:
  - userName: new-hire@example.com      # created when missing (defaults)
    email: new-hire@example.com
    sendPasswordResetEmail: true
    globalRoles: [operators]

  - userName: jdoe@example.com          # SSO user, created on first login
    ifMissing: skip
    inventoryRoles:
      - group: Plant Berlin
        roles: [Site operator]

  - userName: svc-dashboard             # must already exist
    ifMissing: fail
    globalRoles: [dashboard-readers]

  - userName: contractor@example.com    # offboarded
    state: absent
```

| Entry | User missing | User exists |
| --- | --- | --- |
| `state: present`, `ifMissing: create` (defaults) | Create | Update |
| `state: present`, `ifMissing: skip` | Skip, reported as `skipped` | Update |
| `state: present`, `ifMissing: fail` | Fail the user | Update |
| `state: absent` | Nothing to do | Delete |

- `ifMissing: skip` suits SSO users who have not logged in yet: the next run picks them up. With `skip` or `fail` the entry does not need `email`, `password` or `sendPasswordResetEmail`, since the user is never created.
- `state: absent` deletes the user. To only block a login, use `enabled: false`, which keeps the user and their history. An entry with `state: absent` may only have `userName` (validation fails on any other field, `ifMissing` included), and the tenant-sync service user cannot be deleted.
- `enabled` stays a separate field: it is the account state in Cumulocity, while `state` and `ifMissing` say what tenant-sync does with the entry.
- Both fields are generic, so other sections whose objects are matched by name (global roles, device groups) can use them later with the same meaning.

### 5. Dry run diff

Use the existing `dryRunReader` pattern from [sync_login_options.go](../../sync_login_options.go) to read the tenant during a dry run and report real actions:

```
→ [inventoryRoles] Site operator (planned): permissions: +OPERATION:c8y_Restart:ADMIN, -ALARM:*:READ
✓ [inventoryRoles] Restart only (unchanged)
→ [inventoryRoles] Legacy viewer (planned): delete
→ [users] berlin-ops@example.com (planned): inventory roles on Europe/Plant Berlin: +Site operator; inventory roles on Plant Hamburg (not listed): -Site operator
```

For a user who does not exist yet, the dry run shows the assignments they would get when created.

### 6. Export

Export already writes inventory roles. The `users` section is now exported too, with each user's global roles and inventory roles, in additive mode, so applying the export to the same tenant is a no-op. Groups are written as paths when their names are not unique.

Assignments contain user names, which are personal data, so the `users` export is opt-in: it is only written when `users` is named in `--only`, never in a plain export of all sections. Exported users get `ifMissing: skip`, so applying the export never creates accounts, and only `userName`, `email`, `firstName` and `lastName` are written (never passwords or other personal fields).

## CLI commands (go-c8y-cli#172)

Commands remain useful for ad-hoc fixes, scripts and debugging a sync. go-c8y v2 already has the clients tenant-sync uses (`InventoryRoles`, `DevicePermissions`), so the commands are mostly spec work in go-c8y-cli. Today c8y only has read commands (`c8y users listInventoryRoles`, `c8y users getInventoryRole`). Suggested set:

| Command | API |
| --- | --- |
| `c8y inventoryroles list / get / create / update / delete` | `/user/inventoryroles` |
| `c8y users listInventoryAssignments --user` | `GET /user/{tenant}/users/{user}/roles/inventory` |
| `c8y users getInventoryAssignment --user --id` | `GET .../roles/inventory/{id}` |
| `c8y users assignInventoryRole --user --group --role` | `POST .../roles/inventory` |
| `c8y users updateInventoryAssignment --user --id --role` | `PUT .../roles/inventory/{id}` |
| `c8y users unassignInventoryRole --user --id` | `DELETE .../roles/inventory/{id}` |

`--group` and `--role` should accept names as well as IDs, with completion, like other go-c8y-cli lookups. The existing read commands can stay as aliases.

The split: the CLI changes one thing now; tenant-sync keeps many tenants matching a reviewed file. A reply on the issue can point to both.

## Rollout

The steps shipped together in one pull request, with one commit per step:

1. Rename `users[].groups` to `users[].globalRoles`, `users[].inventoryAssignments` to `users[].inventoryRoles`, the `userGroups` section to `globalRoles` (with `roles` renamed to `permissions`), and the SSO access rules' `groups` to `globalRoles`.
2. Dry run diff for `inventoryRoles` and `users[].inventoryRoles`.
3. Role `state: absent` and `renamedFrom`.
4. User `state` and `ifMissing`.
5. `inventoryRolesMode: exact` with revocation and its validation rules.
6. Export of assignments.

CLI commands in go-c8y-cli can be done in parallel.

## Decisions

The open questions of the draft were settled as follows:

- **Deleting a role that is still assigned.** `state: absent` only calls delete. Assignments of other users are not revoked first; if Cumulocity refuses the delete, the role fails with the error of the API.
- **One mode or two.** A separate `inventoryRolesMode` (`additive` or `exact`) on users. Global roles have no mode and stay additive. There is no ownership tracking of roles or assignments.
- **Export.** Opt-in only: `users` is exported when it is named in `--only`. Exported users get `ifMissing: skip`, their `globalRoles` and `inventoryRoles`, and no personal fields beyond `userName`, `email`, `firstName` and `lastName`.
- **Targets other than device groups.** `group` stays limited to device groups (by name or path), as in the UI. Devices and raw managed object IDs are not accepted.
- **Inventory roles in SSO access rules.** Left out: SSO access rules only assign global roles and applications.
- **Renamed fields.** Renamed outright with no deprecated aliases. The manifest parser rejects unknown fields, so a manifest with the old names fails validation. The renames change only the manifest format, never the API payloads.
