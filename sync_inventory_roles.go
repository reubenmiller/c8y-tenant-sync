package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	inventoryroles "github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/userroles/inventory"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users/devicepermissions"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
)

// SyncInventoryRoles ensures inventory roles exist with the desired
// description and permissions. Roles are matched by name. The permissions of
// a role are compared as a set and replaced when they differ (the manifest
// owns the definition of the role); a role without permissions in the
// manifest keeps its permissions. A role found only under one of its
// previous names (renamedFrom) is renamed, and a role with state absent is
// deleted after the users section (see DeleteInventoryRoles). Roles not in
// the manifest are never touched.
//
// A dry run reads the roles of the tenant (when it can) and reports the same
// changes as a real run, without writing them.
func (s *Syncer) SyncInventoryRoles(ctx context.Context, specs []InventoryRoleSpec) error {
	if len(specs) == 0 {
		return nil
	}

	if s.DryRun && s.dryRunReader() == nil {
		// The tenant cannot be read: list the desired roles without
		// comparing them
		for _, spec := range specs {
			if spec.Absent() {
				s.record(SectionInventoryRoles, spec.Name, ActionPlanned, "delete if it exists, after the users section", nil)
				continue
			}
			detail := "ensure inventory role"
			if desired := normalizedPermissions(spec.Permissions); len(desired) > 0 {
				detail += " with permissions: " + strings.Join(permissionKeys(desired), ", ")
			}
			s.record(SectionInventoryRoles, spec.Name, ActionPlanned, detail, nil)
		}
		return nil
	}

	existing, err := s.listInventoryRoles(ctx)
	if err != nil {
		for _, spec := range specs {
			s.record(SectionInventoryRoles, spec.Name, ActionFailed, "list inventory roles", err)
		}
		return nil
	}

	for _, spec := range specs {
		plan := planInventoryRole(spec, existing)
		if plan.err != nil {
			s.record(SectionInventoryRoles, spec.Name, ActionFailed, "", plan.err)
			continue
		}

		if s.DryRun {
			switch plan.action {
			case ActionCreated:
				s.pending().inventoryRoles[spec.Name] = true
				s.record(SectionInventoryRoles, spec.Name, ActionPlanned, plan.describe(), nil)
			case ActionUpdated:
				if plan.renamedFrom != "" {
					// Users of the same dry run resolve the new name to
					// the existing role, and no longer the old name
					s.pending().renamedRoles[spec.Name] = plan.roleID
					s.pending().removedRoles[plan.renamedFrom] = true
				}
				s.record(SectionInventoryRoles, spec.Name, ActionPlanned, plan.describe(), nil)
			case ActionDeleted:
				s.pending().removedRoles[spec.Name] = true
				s.record(SectionInventoryRoles, spec.Name, ActionPlanned, plan.describe()+" after the users section (fails if still assigned)", nil)
			default:
				s.record(SectionInventoryRoles, spec.Name, ActionUnchanged, plan.describe(), nil)
			}
			continue
		}

		switch plan.action {
		case ActionCreated:
			created := s.Client.InventoryRoles.Create(ctx, plan.body)
			if created.Err != nil {
				s.record(SectionInventoryRoles, spec.Name, ActionFailed, "create inventory role", created.Err)
				continue
			}
			s.record(SectionInventoryRoles, spec.Name, ActionCreated, "", nil)
		case ActionUpdated:
			result := s.Client.InventoryRoles.Update(ctx, plan.roleID, plan.body)
			if result.Err != nil {
				s.record(SectionInventoryRoles, spec.Name, ActionFailed, "update inventory role", result.Err)
				continue
			}
			s.record(SectionInventoryRoles, spec.Name, ActionUpdated, plan.describe(), nil)
		case ActionDeleted:
			// Cumulocity refuses to delete a role which is still assigned.
			// The delete waits until the users section has revoked the
			// roles the manifest no longer gives (DeleteInventoryRoles).
			s.roleDeletes = append(s.roleDeletes, roleDelete{name: spec.Name, id: plan.roleID})
		default:
			s.record(SectionInventoryRoles, spec.Name, ActionUnchanged, plan.describe(), nil)
		}
	}
	return nil
}

// inventoryRolePlan is the change an inventory role needs: shared by the dry
// run (which reports it) and the real run (which applies it)
type inventoryRolePlan struct {
	// action is created, updated (which includes a rename), deleted or
	// unchanged
	action Action
	// roleID is the ID of the existing role (updates and deletes)
	roleID int64
	// renamedFrom is the previous name of a role to rename
	renamedFrom string
	// absent is set for a role with state absent
	absent bool
	// body is the request body of the create or update
	body map[string]any
	// permissions are the desired permissions (normalized)
	permissions []InventoryRolePermissionSpec
	// changes describe an update, e.g. "permissions: +ALARM:*:ADMIN"
	changes []string
	err     error
}

// planInventoryRole compares the role of the manifest with the roles of the
// tenant (by name). A role with state absent is deleted when it exists. A
// role which does not exist under its name but under exactly one of its
// previous names is renamed.
func planInventoryRole(spec InventoryRoleSpec, roles map[string][]jsonmodels.InventoryRole) inventoryRolePlan {
	matches := roles[spec.Name]
	if spec.Absent() {
		switch len(matches) {
		case 0:
			return inventoryRolePlan{action: ActionUnchanged, absent: true}
		case 1:
			return inventoryRolePlan{action: ActionDeleted, roleID: matches[0].ID(), absent: true}
		default:
			return inventoryRolePlan{err: fmt.Errorf("ambiguous inventory role name: %d roles are named %q", len(matches), spec.Name)}
		}
	}

	// Roles under a previous name
	var previous []jsonmodels.InventoryRole
	var previousNames []string
	for _, name := range spec.RenamedFrom {
		if name == spec.Name || slices.Contains(previousNames, name) {
			continue
		}
		if found := roles[name]; len(found) > 0 {
			previous = append(previous, found...)
			previousNames = append(previousNames, name)
		}
	}
	if len(matches) > 0 && len(previous) > 0 {
		return inventoryRolePlan{err: fmt.Errorf("ambiguous inventory role: roles named %q and %s (renamedFrom) both exist; delete the role which is no longer needed, or remove renamedFrom to keep both", spec.Name, quoteJoin(previousNames))}
	}

	renamedFrom := ""
	if len(matches) == 0 && len(previous) > 0 {
		if len(previous) > 1 {
			return inventoryRolePlan{err: fmt.Errorf("ambiguous inventory role: %d roles are named %s (renamedFrom); delete all but the one to rename", len(previous), quoteJoin(previousNames))}
		}
		matches = previous
		renamedFrom = previousNames[0]
	}

	desired := normalizedPermissions(spec.Permissions)
	switch len(matches) {
	case 0:
		body := map[string]any{"name": spec.Name}
		if spec.Description != "" {
			body["description"] = spec.Description
		}
		if spec.Permissions != nil {
			body["permissions"] = permissionsBody(desired)
		}
		return inventoryRolePlan{action: ActionCreated, body: body, permissions: desired}
	case 1:
		role := matches[0]
		plan := inventoryRolePlan{action: ActionUnchanged, roleID: role.ID(), permissions: desired, renamedFrom: renamedFrom}
		body := map[string]any{"name": spec.Name}
		if renamedFrom != "" {
			plan.changes = append(plan.changes, fmt.Sprintf("name: %q → %q", renamedFrom, spec.Name))
		}
		if spec.Description != "" && role.Description() != spec.Description {
			body["description"] = spec.Description
			plan.changes = append(plan.changes, fmt.Sprintf("description: %q → %q", role.Description(), spec.Description))
		}
		if spec.Permissions != nil {
			if diff := permissionDiff(existingPermissions(role), desired); diff != "" {
				body["permissions"] = permissionsBody(desired)
				plan.changes = append(plan.changes, "permissions: "+diff)
			}
		}
		if len(plan.changes) > 0 {
			plan.action = ActionUpdated
			plan.body = body
		}
		return plan
	default:
		return inventoryRolePlan{err: fmt.Errorf("ambiguous inventory role name: %d roles are named %q", len(matches), spec.Name)}
	}
}

// quoteJoin quotes and joins names, e.g. "a", "b"
func quoteJoin(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, ", ")
}

// describe returns the detail reported for the plan: the permissions of a
// role to create, or the changes of an update
func (p inventoryRolePlan) describe() string {
	switch p.action {
	case ActionCreated:
		if len(p.permissions) == 0 {
			return "create"
		}
		return "create with permissions: " + strings.Join(permissionKeys(p.permissions), ", ")
	case ActionUpdated:
		return strings.Join(p.changes, "; ")
	case ActionDeleted:
		return "delete"
	case ActionUnchanged:
		if p.absent {
			return "absent"
		}
	}
	return ""
}

// permissionDiff describes the difference of two normalized permission sets,
// e.g. "+OPERATION:c8y_Restart:ADMIN, -ALARM:*:READ" (empty when they match)
func permissionDiff(current, desired []InventoryRolePermissionSpec) string {
	currentKeys := permissionKeys(current)
	desiredKeys := permissionKeys(desired)
	var parts []string
	for _, key := range desiredKeys {
		if !slices.Contains(currentKeys, key) {
			parts = append(parts, "+"+key)
		}
	}
	for _, key := range currentKeys {
		if !slices.Contains(desiredKeys, key) {
			parts = append(parts, "-"+key)
		}
	}
	return strings.Join(parts, ", ")
}

// dryRunPending holds the objects which a dry run would create in the active
// target: a later section of the same dry run can refer to them, although
// the tenant does not have them yet
type dryRunPending struct {
	// inventoryRoles by name
	inventoryRoles map[string]bool
	// renamedRoles are the IDs of existing roles which would be renamed, by
	// their new name
	renamedRoles map[string]int64
	// removedRoles are the names which would no longer exist: the old names
	// of renamed roles and the roles which would be deleted
	removedRoles map[string]bool
	// deviceGroups by path and by name
	deviceGroups map[string]bool
}

// pending returns the objects a dry run would create in the active target
func (s *Syncer) pending() *dryRunPending {
	if s.dryRunPending == nil {
		s.dryRunPending = &dryRunPending{
			inventoryRoles: map[string]bool{},
			renamedRoles:   map[string]int64{},
			removedRoles:   map[string]bool{},
			deviceGroups:   map[string]bool{},
		}
	}
	return s.dryRunPending
}

// listInventoryRoles returns the inventory roles of the tenant by name
func (s *Syncer) listInventoryRoles(ctx context.Context) (map[string][]jsonmodels.InventoryRole, error) {
	roles := map[string][]jsonmodels.InventoryRole{}
	result := s.readClient().InventoryRoles.List(ctx, inventoryroles.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for role, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		roles[role.Name()] = append(roles[role.Name()], role)
	}
	return roles, nil
}

// normalizedPermissions applies the permission defaults and sorts the
// permissions, so they can be compared and printed in a stable order
func normalizedPermissions(permissions []InventoryRolePermissionSpec) []InventoryRolePermissionSpec {
	out := make([]InventoryRolePermissionSpec, 0, len(permissions))
	seen := map[string]bool{}
	for _, permission := range permissions {
		n := permission.Normalized()
		if seen[n.Key()] {
			continue
		}
		seen[n.Key()] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// existingPermissions reads the permissions of an inventory role
func existingPermissions(role jsonmodels.InventoryRole) []InventoryRolePermissionSpec {
	var permissions []InventoryRolePermissionSpec
	for _, item := range role.Get("permissions").Array() {
		permissions = append(permissions, InventoryRolePermissionSpec{
			Scope:      item.Get("scope").String(),
			Type:       item.Get("type").String(),
			Permission: item.Get("permission").String(),
		})
	}
	return normalizedPermissions(permissions)
}

func permissionKeys(permissions []InventoryRolePermissionSpec) []string {
	keys := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		keys = append(keys, permission.Key())
	}
	return keys
}

func permissionsBody(permissions []InventoryRolePermissionSpec) []map[string]any {
	body := make([]map[string]any, 0, len(permissions))
	for _, permission := range permissions {
		body = append(body, map[string]any{
			"scope":      permission.Scope,
			"type":       permission.Type,
			"permission": permission.Permission,
		})
	}
	return body
}

// inventoryAssignmentCache holds the lookups shared by all users of a target
// tenant: inventory roles by name and device group IDs by name or path
type inventoryAssignmentCache struct {
	roles    map[string][]jsonmodels.InventoryRole
	groupIDs map[string]string
}

// errNotFound marks a lookup of a device group or an inventory role which
// found nothing
var errNotFound = errors.New("not found")

// describeInventoryAssignments describes the assignments for the dry-run
// output, e.g. "Site operator on Plant Berlin"
func describeInventoryAssignments(assignments []InventoryAssignmentSpec) string {
	parts := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		parts = append(parts, strings.Join(assignment.Roles, ", ")+" on "+assignment.Group)
	}
	return strings.Join(parts, "; ")
}

// inventoryRoleRef is an inventory role to add to (or remove from) an
// assignment
type inventoryRoleRef struct {
	id   int64
	name string
	// pending is set in a dry run for a role the inventoryRoles section
	// would create (it has no ID yet)
	pending bool
}

// inventoryAssignmentChange is the change of a user's assignment on one
// device group: shared by the dry run (which reports it) and the real run
// (which applies it)
type inventoryAssignmentChange struct {
	// group is the group as written in the manifest (or the name of the
	// group of an assignment which is not in the manifest)
	group string
	// groupID is the managed object ID of the group (empty for a pending
	// group)
	groupID string
	// pendingGroup is set in a dry run for a group the deviceGroups section
	// would create
	pendingGroup bool
	// assignmentID is the ID of the user's existing assignment on the
	// group (0 when there is none), and currentRoles its role IDs
	assignmentID int64
	currentRoles []int64
	// add are the roles to add
	add []inventoryRoleRef
	// remove are the roles to remove (inventoryRolesMode exact only)
	remove []inventoryRoleRef
	// unlisted is set for an assignment on a group which is not in the
	// manifest: it is deleted (inventoryRolesMode exact only)
	unlisted bool
	// duplicates are the IDs of the user's other assignments on the same
	// group: they are merged into the first one (inventoryRolesMode exact
	// only)
	duplicates []int64
}

// describe describes the change, e.g. "inventory roles on Plant Berlin: +Site operator, -Reader"
func (c inventoryAssignmentChange) describe() string {
	parts := make([]string, 0, len(c.add)+len(c.remove)+1)
	for _, role := range c.add {
		part := "+" + role.name
		if role.pending {
			part += " (new role)"
		}
		parts = append(parts, part)
	}
	for _, role := range c.remove {
		parts = append(parts, "-"+role.name)
	}
	if n := len(c.duplicates); n > 0 && !c.unlisted {
		parts = append(parts, fmt.Sprintf("merge %d assignments", n+1))
	}
	group := c.group
	if c.pendingGroup {
		group += " (new group)"
	}
	if c.unlisted {
		group += " (not listed)"
	}
	return "inventory roles on " + group + ": " + strings.Join(parts, ", ")
}

// existingAssignment is a user's assignment on a managed object. Several
// assignments on one object are merged into the first one.
type existingAssignment struct {
	groupID    string
	id         int64
	roles      []int64
	duplicates []int64
}

// planInventoryAssignments compares the user's inventory roles in the
// manifest with the user's assignments in the tenant and returns the changes
// needed. Additive mode only adds the missing roles: roles and assignments
// not in the manifest are never removed. Exact mode also removes the roles
// which are not listed on a listed group, deletes the assignments on groups
// which are not listed, and merges several assignments on one group. For a
// user which does not exist (yet) the lookup of existing assignments is
// skipped. A group or role which cannot be resolved fails its assignment
// only: the changes of the other assignments are still returned, together
// with the joined errors. In exact mode no unlisted assignment is deleted
// when a listed group cannot be resolved, since it could be that group.
// Entries which resolve to the same group are merged into one change.
func (s *Syncer) planInventoryAssignments(ctx context.Context, userID string, exists, exact bool, assignments []InventoryAssignmentSpec, cache *inventoryAssignmentCache) ([]inventoryAssignmentChange, error) {
	if len(assignments) == 0 && (!exact || !exists) {
		return nil, nil
	}

	if cache.roles == nil {
		roles, err := s.listInventoryRoles(ctx)
		if err != nil {
			return nil, fmt.Errorf("list inventory roles: %w", err)
		}
		cache.roles = roles
	}
	if cache.groupIDs == nil {
		cache.groupIDs = map[string]string{}
	}

	// Existing assignments by managed object ID, and in the order listed
	existing := map[string]*existingAssignment{}
	var existingOrder []*existingAssignment
	if exists {
		result := s.readClient().DevicePermissions.ListInventoryRoleAssignments(ctx, devicepermissions.ListInventoryRoleAssignmentOptions{
			UserScopedOptions: devicepermissions.UserScopedOptions{UserID: userID},
			PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
		})
		for item, err := range result.Items() {
			if err != nil {
				return nil, fmt.Errorf("list inventory assignments: %w", err)
			}
			if current, ok := existing[item.ManagedObjectID()]; ok {
				// Several assignments for one group: extend the first one
				current.roles = appendUnique(current.roles, item.RoleIDs()...)
				current.duplicates = append(current.duplicates, item.ID())
				continue
			}
			current := &existingAssignment{groupID: item.ManagedObjectID(), id: item.ID(), roles: appendUnique(nil, item.RoleIDs()...)}
			existing[item.ManagedObjectID()] = current
			existingOrder = append(existingOrder, current)
		}
	}

	var changes []inventoryAssignmentChange
	var errs []error
	listed := map[string]bool{}
	unresolvedGroup := false

	// Resolve the groups first. Entries written differently which resolve
	// to the same group (e.g. "Plant Berlin" and "Europe/Plant Berlin") are
	// merged into the first one, so that one change covers all of their
	// roles: separate changes would each start from the same existing
	// assignment and undo each other.
	type plannedAssignment struct {
		change inventoryAssignmentChange
		roles  []string
	}
	var planned []*plannedAssignment
	byGroup := map[string]*plannedAssignment{}
	for _, assignment := range assignments {
		ref := strings.TrimSpace(assignment.Group)
		change := inventoryAssignmentChange{group: assignment.Group}
		var key string
		groupID, err := s.resolveDeviceGroup(ctx, ref, cache.groupIDs)
		switch {
		case err == nil:
			change.groupID = groupID
			key = "id:" + groupID
		case s.DryRun && errors.Is(err, errNotFound) && s.pending().deviceGroups[ref]:
			change.pendingGroup = true
			key = "pending:" + strings.Join(splitGroupPath(ref), "/")
		default:
			unresolvedGroup = true
			errs = append(errs, err)
			continue
		}
		if entry, ok := byGroup[key]; ok {
			for _, role := range assignment.Roles {
				if !slices.Contains(entry.roles, role) {
					entry.roles = append(entry.roles, role)
				}
			}
			continue
		}
		entry := &plannedAssignment{change: change, roles: slices.Clone(assignment.Roles)}
		byGroup[key] = entry
		planned = append(planned, entry)
	}

	for _, entry := range planned {
		change := entry.change
		if change.groupID != "" {
			listed[change.groupID] = true
			if current, ok := existing[change.groupID]; ok {
				change.assignmentID = current.id
				change.currentRoles = current.roles
				if exact {
					change.duplicates = current.duplicates
				}
			}
		}

		var roleErr error
		var desired []int64
		for _, roleName := range entry.roles {
			roleID, err := cache.roleID(roleName)
			if s.DryRun {
				// The inventoryRoles section of the same dry run would
				// rename or delete roles
				if id, ok := s.pending().renamedRoles[roleName]; ok {
					roleID, err = id, nil
				} else if s.pending().removedRoles[roleName] {
					roleID, err = 0, fmt.Errorf("inventory role %q: %w", roleName, errNotFound)
				}
			}
			if err != nil {
				if s.DryRun && errors.Is(err, errNotFound) && s.pending().inventoryRoles[roleName] {
					if !slices.ContainsFunc(change.add, func(r inventoryRoleRef) bool { return r.pending && r.name == roleName }) {
						change.add = append(change.add, inventoryRoleRef{name: roleName, pending: true})
					}
					continue
				}
				roleErr = err
				break
			}
			desired = append(desired, roleID)
			if slices.Contains(change.currentRoles, roleID) {
				continue
			}
			if slices.ContainsFunc(change.add, func(r inventoryRoleRef) bool { return !r.pending && r.id == roleID }) {
				continue
			}
			change.add = append(change.add, inventoryRoleRef{id: roleID, name: roleName})
		}
		if roleErr != nil {
			errs = append(errs, roleErr)
			continue
		}
		if exact {
			for _, roleID := range change.currentRoles {
				if !slices.Contains(desired, roleID) {
					change.remove = append(change.remove, inventoryRoleRef{id: roleID, name: s.roleName(cache, roleID)})
				}
			}
		}
		if len(change.add) > 0 || len(change.remove) > 0 || len(change.duplicates) > 0 {
			changes = append(changes, change)
		}
	}

	if exact && !unresolvedGroup {
		// The assignments on groups which are not listed are deleted
		for _, current := range existingOrder {
			if listed[current.groupID] {
				continue
			}
			change := inventoryAssignmentChange{
				group:        s.assignmentGroupName(ctx, current.groupID),
				groupID:      current.groupID,
				assignmentID: current.id,
				currentRoles: current.roles,
				unlisted:     true,
				duplicates:   current.duplicates,
			}
			for _, roleID := range current.roles {
				change.remove = append(change.remove, inventoryRoleRef{id: roleID, name: s.roleName(cache, roleID)})
			}
			changes = append(changes, change)
		}
	}
	return changes, errors.Join(errs...)
}

// roleName returns the name of an existing inventory role for the output (in
// a dry run, the new name of a role which would be renamed)
func (s *Syncer) roleName(cache *inventoryAssignmentCache, id int64) string {
	if s.DryRun && s.dryRunPending != nil {
		for name, renamedID := range s.dryRunPending.renamedRoles {
			if renamedID == id {
				return name
			}
		}
	}
	for name, roles := range cache.roles {
		for _, role := range roles {
			if role.ID() == id {
				return name
			}
		}
	}
	return fmt.Sprintf("role %d", id)
}

// assignmentGroupName returns the name of the group of an assignment which is
// not in the manifest, for the output (the ID when it cannot be read)
func (s *Syncer) assignmentGroupName(ctx context.Context, groupID string) string {
	result := s.readClient().ManagedObjects.Get(ctx, groupID, managedobjects.GetOptions{})
	if result.Err != nil || result.Data.Name() == "" {
		return "device group " + groupID
	}
	return result.Data.Name()
}

// appendUnique appends the IDs which are not in the list yet
func appendUnique(list []int64, ids ...int64) []int64 {
	for _, id := range ids {
		if !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	return list
}

// applyInventoryAssignments writes the changes of the user's assignments and
// returns the descriptions of the changes made. A failed write does not stop
// the other changes; the errors are joined.
func (s *Syncer) applyInventoryAssignments(ctx context.Context, userID string, changes []inventoryAssignmentChange) ([]string, error) {
	var details []string
	var errs []error
	userScope := devicepermissions.UserScopedOptions{UserID: userID}
	for _, change := range changes {
		if err := s.applyInventoryAssignment(ctx, userScope, change); err != nil {
			errs = append(errs, fmt.Errorf("inventory assignment for group %q: %w", change.group, err))
			continue
		}
		details = append(details, change.describe())
	}
	return details, errors.Join(errs...)
}

// applyInventoryAssignment writes the change of one assignment
func (s *Syncer) applyInventoryAssignment(ctx context.Context, userScope devicepermissions.UserScopedOptions, change inventoryAssignmentChange) error {
	deleteAssignment := func(id int64) error {
		result := s.Client.DevicePermissions.DeleteInventoryRoleAssignment(ctx, devicepermissions.GetInventoryRoleAssignmentOptions{
			UserScopedOptions: userScope,
			ID:                id,
		})
		return result.Err
	}

	if change.unlisted {
		for _, id := range append([]int64{change.assignmentID}, change.duplicates...) {
			if err := deleteAssignment(id); err != nil {
				return err
			}
		}
		return nil
	}

	addIDs := make([]int64, 0, len(change.add))
	for _, role := range change.add {
		addIDs = append(addIDs, role.id)
	}
	if change.assignmentID == 0 {
		result := s.Client.DevicePermissions.AssignInventoryRole(ctx, userScope, map[string]any{
			"managedObject": change.groupID,
			"roles":         roleRefs(addIDs),
		})
		return result.Err
	}

	roleIDs := slices.DeleteFunc(slices.Clone(change.currentRoles), func(id int64) bool {
		return slices.ContainsFunc(change.remove, func(r inventoryRoleRef) bool { return r.id == id })
	})
	roleIDs = append(roleIDs, addIDs...)
	if len(change.add) > 0 || len(change.remove) > 0 || len(change.duplicates) > 0 {
		result := s.Client.DevicePermissions.UpdateInventoryRoleAssignment(ctx, devicepermissions.GetInventoryRoleAssignmentOptions{
			UserScopedOptions: userScope,
			ID:                change.assignmentID,
		}, map[string]any{"roles": roleRefs(roleIDs)})
		if result.Err != nil {
			return result.Err
		}
	}
	// The other assignments on the group are merged into the first one
	for _, id := range change.duplicates {
		if err := deleteAssignment(id); err != nil {
			return err
		}
	}
	return nil
}

// roleID resolves an inventory role name to its ID
func (c *inventoryAssignmentCache) roleID(name string) (int64, error) {
	matches := c.roles[name]
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("inventory role %q: %w", name, errNotFound)
	case 1:
		return matches[0].ID(), nil
	default:
		return 0, fmt.Errorf("inventory role %q: ambiguous: %d roles have this name", name, len(matches))
	}
}

func roleRefs(ids []int64) []map[string]any {
	refs := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, map[string]any{"id": id})
	}
	return refs
}

// splitGroupPath splits a device group path (Europe/Plant Berlin) into the
// group names
func splitGroupPath(path string) []string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// resolveDeviceGroup resolves a device group by name, or by a path of group
// names separated by "/" in which each group is a direct child of the
// previous one. A value containing "/" is first looked up as a plain name, so
// groups with a "/" in their name can still be referenced. The group must be
// unique.
func (s *Syncer) resolveDeviceGroup(ctx context.Context, ref string, cache map[string]string) (string, error) {
	if id, ok := cache[ref]; ok {
		return id, nil
	}

	groups, err := s.findDeviceGroups(ctx, ref, false)
	if err != nil {
		return "", fmt.Errorf("device group %q: %w", ref, err)
	}
	if len(groups) == 0 && strings.Contains(ref, "/") {
		groups, err = s.findDeviceGroupPath(ctx, splitGroupPath(ref))
		if err != nil {
			return "", fmt.Errorf("device group %q: %w", ref, err)
		}
	}

	switch len(groups) {
	case 0:
		return "", fmt.Errorf("device group %q: %w", ref, errNotFound)
	case 1:
		cache[ref] = groups[0].ID()
		return groups[0].ID(), nil
	default:
		ids := make([]string, 0, len(groups))
		for _, group := range groups {
			ids = append(ids, group.ID())
		}
		return "", fmt.Errorf("device group %q: ambiguous: %d groups match (IDs %s); use a path such as Parent/%s",
			ref, len(groups), strings.Join(ids, ", "), ref)
	}
}

// findDeviceGroupPath returns the groups matching the last name of the path
// whose parents match the previous names
func (s *Syncer) findDeviceGroupPath(ctx context.Context, names []string) ([]jsonmodels.ManagedObject, error) {
	candidates, err := s.findDeviceGroups(ctx, names[0], false)
	if err != nil {
		return nil, err
	}
	for _, name := range names[1:] {
		parentIDs := map[string]bool{}
		for _, candidate := range candidates {
			parentIDs[candidate.ID()] = true
		}
		children, err := s.findDeviceGroups(ctx, name, true)
		if err != nil {
			return nil, err
		}
		candidates = candidates[:0]
		for _, child := range children {
			for _, parent := range child.Get("assetParents.references.#.managedObject.id").Array() {
				if parentIDs[parent.String()] {
					candidates = append(candidates, child)
					break
				}
			}
		}
		if len(candidates) == 0 {
			break
		}
	}
	return candidates, nil
}

// findDeviceGroups returns the device groups with exactly the given name
func (s *Syncer) findDeviceGroups(ctx context.Context, name string, withParents bool) ([]jsonmodels.ManagedObject, error) {
	result := s.readClient().ManagedObjects.List(ctx, managedobjects.ListOptions{
		Query:             fmt.Sprintf("has(c8y_IsDeviceGroup) and name eq '%s'", escapeQueryValue(name)),
		GetOptions:        managedobjects.GetOptions{WithParents: withParents},
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	var groups []jsonmodels.ManagedObject
	for item, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		// The query language treats * as a wildcard: keep exact matches only
		if item.Name() == name {
			groups = append(groups, item)
		}
	}
	return groups, nil
}
