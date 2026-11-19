package main

import (
	"context"
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
// manifest keeps its permissions. Roles absent from the manifest are never
// touched.
func (s *Syncer) SyncInventoryRoles(ctx context.Context, specs []InventoryRoleSpec) error {
	if len(specs) == 0 {
		return nil
	}

	var existing map[string][]jsonmodels.InventoryRole
	if !s.DryRun {
		var err error
		existing, err = s.listInventoryRoles(ctx)
		if err != nil {
			for _, spec := range specs {
				s.record(SectionInventoryRoles, spec.Name, ActionFailed, "list inventory roles", err)
			}
			return nil
		}
	}

	for _, spec := range specs {
		desired := normalizedPermissions(spec.Permissions)

		if s.DryRun {
			detail := "ensure inventory role"
			if len(desired) > 0 {
				detail += " with permissions: " + strings.Join(permissionKeys(desired), ", ")
			}
			s.record(SectionInventoryRoles, spec.Name, ActionPlanned, detail, nil)
			continue
		}

		matches := existing[spec.Name]
		switch len(matches) {
		case 0:
			body := map[string]any{"name": spec.Name}
			if spec.Description != "" {
				body["description"] = spec.Description
			}
			if spec.Permissions != nil {
				body["permissions"] = permissionsBody(desired)
			}
			created := s.Client.InventoryRoles.Create(ctx, body)
			if created.Err != nil {
				s.record(SectionInventoryRoles, spec.Name, ActionFailed, "create inventory role", created.Err)
				continue
			}
			s.record(SectionInventoryRoles, spec.Name, ActionCreated, "", nil)
		case 1:
			role := matches[0]
			var changes []string
			body := map[string]any{"name": spec.Name}
			if spec.Description != "" && role.Description() != spec.Description {
				body["description"] = spec.Description
				changes = append(changes, "description")
			}
			if spec.Permissions != nil && !samePermissions(desired, existingPermissions(role)) {
				body["permissions"] = permissionsBody(desired)
				changes = append(changes, "permissions")
			}
			if len(changes) == 0 {
				s.record(SectionInventoryRoles, spec.Name, ActionUnchanged, "", nil)
				continue
			}
			result := s.Client.InventoryRoles.Update(ctx, role.ID(), body)
			if result.Err != nil {
				s.record(SectionInventoryRoles, spec.Name, ActionFailed, "update inventory role", result.Err)
				continue
			}
			s.record(SectionInventoryRoles, spec.Name, ActionUpdated, "updated "+strings.Join(changes, ", "), nil)
		default:
			s.record(SectionInventoryRoles, spec.Name, ActionFailed, "",
				fmt.Errorf("ambiguous inventory role name: %d roles are named %q", len(matches), spec.Name))
		}
	}
	return nil
}

// listInventoryRoles returns the inventory roles of the tenant by name
func (s *Syncer) listInventoryRoles(ctx context.Context) (map[string][]jsonmodels.InventoryRole, error) {
	roles := map[string][]jsonmodels.InventoryRole{}
	result := s.Client.InventoryRoles.List(ctx, inventoryroles.ListOptions{
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

// samePermissions compares two normalized permission sets
func samePermissions(a, b []InventoryRolePermissionSpec) bool {
	return slices.Equal(permissionKeys(a), permissionKeys(b))
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

// describeInventoryAssignments describes the assignments for the dry-run
// output, e.g. "Site operator on Plant Berlin"
func describeInventoryAssignments(assignments []InventoryAssignmentSpec) string {
	parts := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		parts = append(parts, strings.Join(assignment.Roles, ", ")+" on "+assignment.Group)
	}
	return strings.Join(parts, "; ")
}

// ensureInventoryAssignments adds the missing inventory roles to the user's
// assignment of each group, creating the assignment when the user has none
// for the group. It returns a description of the changes. Roles and
// assignments not in the manifest are never removed. For a freshly created
// user the lookup of existing assignments is skipped (it has none yet).
func (s *Syncer) ensureInventoryAssignments(ctx context.Context, userID string, created bool, assignments []InventoryAssignmentSpec, cache *inventoryAssignmentCache) ([]string, error) {
	if len(assignments) == 0 {
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

	// Existing assignments by managed object ID
	type existingAssignment struct {
		id    int64
		roles []int64
	}
	existing := map[string]*existingAssignment{}
	if !created {
		result := s.Client.DevicePermissions.ListInventoryRoleAssignments(ctx, devicepermissions.ListInventoryRoleAssignmentOptions{
			UserScopedOptions: devicepermissions.UserScopedOptions{UserID: userID},
			PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
		})
		for item, err := range result.Items() {
			if err != nil {
				return nil, fmt.Errorf("list inventory assignments: %w", err)
			}
			if current, ok := existing[item.ManagedObjectID()]; ok {
				// Several assignments for one group: extend the first one
				current.roles = append(current.roles, item.RoleIDs()...)
				continue
			}
			existing[item.ManagedObjectID()] = &existingAssignment{id: item.ID(), roles: item.RoleIDs()}
		}
	}

	var details []string
	for _, assignment := range assignments {
		groupID, err := s.resolveDeviceGroup(ctx, strings.TrimSpace(assignment.Group), cache.groupIDs)
		if err != nil {
			return details, err
		}

		var missingIDs []int64
		var missingNames []string
		for _, roleName := range assignment.Roles {
			roleID, err := cache.roleID(roleName)
			if err != nil {
				return details, err
			}
			if current, ok := existing[groupID]; ok && slices.Contains(current.roles, roleID) {
				continue
			}
			if slices.Contains(missingIDs, roleID) {
				continue
			}
			missingIDs = append(missingIDs, roleID)
			missingNames = append(missingNames, roleName)
		}
		if len(missingIDs) == 0 {
			continue
		}

		if current, ok := existing[groupID]; ok {
			roleIDs := append(slices.Clone(current.roles), missingIDs...)
			result := s.Client.DevicePermissions.UpdateInventoryRoleAssignment(ctx, devicepermissions.GetInventoryRoleAssignmentOptions{
				UserScopedOptions: devicepermissions.UserScopedOptions{UserID: userID},
				ID:                current.id,
			}, map[string]any{"roles": roleRefs(roleIDs)})
			if result.Err != nil {
				return details, fmt.Errorf("inventory assignment for group %q: %w", assignment.Group, result.Err)
			}
			current.roles = roleIDs
		} else {
			result := s.Client.DevicePermissions.AssignInventoryRole(ctx, devicepermissions.UserScopedOptions{UserID: userID}, map[string]any{
				"managedObject": groupID,
				"roles":         roleRefs(missingIDs),
			})
			if result.Err != nil {
				return details, fmt.Errorf("inventory assignment for group %q: %w", assignment.Group, result.Err)
			}
			existing[groupID] = &existingAssignment{id: result.Data.ID(), roles: missingIDs}
		}
		details = append(details, "inventory roles on "+assignment.Group+": "+strings.Join(missingNames, ", "))
	}
	return details, nil
}

// roleID resolves an inventory role name to its ID
func (c *inventoryAssignmentCache) roleID(name string) (int64, error) {
	matches := c.roles[name]
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("inventory role %q: not found", name)
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
		return "", fmt.Errorf("device group %q: not found", ref)
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
	result := s.Client.ManagedObjects.List(ctx, managedobjects.ListOptions{
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
