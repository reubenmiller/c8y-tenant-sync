package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	inventoryroles "github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/userroles/inventory"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users/devicepermissions"
)

// optInExportSections are only exported when they are named in --only: the
// users section contains personal data (user names and email addresses)
var optInExportSections = []string{SectionUsers}

// serviceUserPrefix is the user name prefix of the service users Cumulocity
// creates for microservices. They are not exported.
const serviceUserPrefix = "service_"

// users exports the users of the tenant with ifMissing: skip, so applying the
// export never creates accounts, and their global and inventory roles (in the
// default additive mode, so applying it to the same tenant changes nothing).
// Passwords and other personal fields are never exported. Service users of
// microservices are left out, and so are device users (the API does not list
// them).
func (e *Exporter) users(ctx context.Context) ([]UserSpec, error) {
	roleNames, err := e.inventoryRoleNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("list inventory roles: %w", err)
	}
	groups, err := e.deviceGroupNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list device groups: %w", err)
	}
	index := newDeviceGroupIndex(groups)

	var specs []UserSpec
	result := e.Client.Users.ListAll(ctx, users.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for user, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(strings.ToLower(user.UserName()), serviceUserPrefix) {
			continue
		}
		spec := UserSpec{
			Username:  user.UserName(),
			IfMissing: UserIfMissingSkip,
			Email:     user.Email(),
			FirstName: user.FirstName(),
			LastName:  user.LastName(),
		}
		// enabled is only written when false (true is the default)
		if user.Get("enabled").Exists() && !user.Enabled() {
			disabled := false
			spec.Enabled = &disabled
		}
		if spec.GlobalRoles, err = e.userGlobalRoles(ctx, user.ID()); err != nil {
			return nil, fmt.Errorf("user %q: %w", spec.Username, err)
		}
		if spec.InventoryRoles, err = e.userInventoryRoles(ctx, user.ID(), spec.Username, roleNames, index); err != nil {
			return nil, fmt.Errorf("user %q: %w", spec.Username, err)
		}
		specs = append(specs, spec)
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Username < specs[j].Username })
	return specs, nil
}

// deviceGroupNodes lists the device groups of the tenant once per export
func (e *Exporter) deviceGroupNodes(ctx context.Context) ([]deviceGroupNode, error) {
	if e.groups == nil {
		groups, err := listDeviceGroupNodes(ctx, e.Client.DeviceGroups)
		if err != nil {
			return nil, err
		}
		e.groups = groups
		if e.groups == nil {
			e.groups = []deviceGroupNode{}
		}
	}
	return e.groups, nil
}

// inventoryRoleNames maps the IDs of the inventory roles to their names
func (e *Exporter) inventoryRoleNames(ctx context.Context) (map[int64]string, error) {
	names := map[int64]string{}
	result := e.Client.InventoryRoles.List(ctx, inventoryroles.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for role, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		names[role.ID()] = role.Name()
	}
	return names, nil
}

// userGlobalRoles returns the sorted names of the user's global roles (user
// groups in the API)
func (e *Exporter) userGlobalRoles(ctx context.Context, userID string) ([]string, error) {
	var names []string
	result := e.Client.Users.ListGroupsWithUser(ctx, users.ListGroupsOptions{
		UserID:            userID,
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for group, err := range result.Items() {
		if err != nil {
			return nil, fmt.Errorf("list global roles: %w", err)
		}
		if !slices.Contains(names, group.Name()) {
			names = append(names, group.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// userInventoryRoles returns the user's inventory roles by device group,
// sorted by group and role name. Several assignments on one group are merged.
// Assignments on managed objects which are not device groups, and roles which
// cannot be found, are left out with a note.
func (e *Exporter) userInventoryRoles(ctx context.Context, userID, userName string, roleNames map[int64]string, index *deviceGroupIndex) ([]InventoryAssignmentSpec, error) {
	byGroup := map[string]*InventoryAssignmentSpec{}
	var specs []*InventoryAssignmentSpec
	result := e.Client.DevicePermissions.ListInventoryRoleAssignments(ctx, devicepermissions.ListInventoryRoleAssignmentOptions{
		UserScopedOptions: devicepermissions.UserScopedOptions{UserID: userID},
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for item, err := range result.Items() {
		if err != nil {
			return nil, fmt.Errorf("list inventory roles: %w", err)
		}
		groupID := item.ManagedObjectID()
		if _, ok := index.byID[groupID]; !ok {
			exportNote(SectionUsers, fmt.Sprintf("%s: the inventory roles on managed object %s are not exported: it is not a device group", userName, groupID))
			continue
		}
		spec, ok := byGroup[groupID]
		if !ok {
			label, unique := index.label(groupID)
			if !unique {
				exportNote(SectionUsers, fmt.Sprintf("%s: device group %s (ID %s) has no unique name or path: review its inventory roles", userName, label, groupID))
			}
			spec = &InventoryAssignmentSpec{Group: label}
			byGroup[groupID] = spec
			specs = append(specs, spec)
		}
		for _, roleID := range item.RoleIDs() {
			name, ok := roleNames[roleID]
			if !ok {
				exportNote(SectionUsers, fmt.Sprintf("%s: inventory role %d on %s is not exported: the role was not found", userName, roleID, spec.Group))
				continue
			}
			if !slices.Contains(spec.Roles, name) {
				spec.Roles = append(spec.Roles, name)
			}
		}
	}

	var out []InventoryAssignmentSpec
	for _, spec := range specs {
		if len(spec.Roles) == 0 {
			continue
		}
		sort.Strings(spec.Roles)
		out = append(out, *spec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out, nil
}

// deviceGroupIndex names device groups the way the users section resolves
// them (see Syncer.resolveDeviceGroup): by name, or by a path of group names
// when the name is not unique
type deviceGroupIndex struct {
	byID   map[string]deviceGroupNode
	byName map[string][]string
}

func newDeviceGroupIndex(groups []deviceGroupNode) *deviceGroupIndex {
	index := &deviceGroupIndex{byID: map[string]deviceGroupNode{}, byName: map[string][]string{}}
	for _, group := range groups {
		index.byID[group.id] = group
		index.byName[group.name] = append(index.byName[group.name], group.id)
	}
	return index
}

// resolve returns the IDs of the groups a reference matches, like
// Syncer.resolveDeviceGroup: the groups with exactly that name, else (for a
// reference containing "/") the groups matching the path
func (x *deviceGroupIndex) resolve(ref string) []string {
	if ids := x.byName[ref]; len(ids) > 0 || !strings.Contains(ref, "/") {
		return ids
	}
	names := splitGroupPath(ref)
	candidates := slices.Clone(x.byName[names[0]])
	for _, name := range names[1:] {
		parents := map[string]bool{}
		for _, id := range candidates {
			parents[id] = true
		}
		candidates = nil
		for _, id := range x.byName[name] {
			if slices.ContainsFunc(x.byID[id].parents, func(parent string) bool { return parents[parent] }) {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) == 0 {
			break
		}
	}
	return candidates
}

// path returns the names of the group and its ancestors, from the root. Like
// the exported device group tree, a group is placed below its first parent
// which is a device group.
func (x *deviceGroupIndex) path(id string) []string {
	var names []string
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		seen[id] = true
		group := x.byID[id]
		names = append(names, group.name)
		next := ""
		for _, parent := range group.parents {
			if _, ok := x.byID[parent]; ok && parent != id {
				next = parent
				break
			}
		}
		id = next
	}
	slices.Reverse(names)
	return names
}

// label returns the shortest reference which resolves to exactly this group:
// its name, else the shortest Parent/Child path ending in its name. When no
// reference is unique, the full path is returned with unique false.
func (x *deviceGroupIndex) label(id string) (string, bool) {
	names := x.path(id)
	for start := len(names) - 1; start >= 0; start-- {
		ref := strings.Join(names[start:], "/")
		if ids := x.resolve(ref); len(ids) == 1 && ids[0] == id {
			return ref, true
		}
	}
	return strings.Join(names, "/"), false
}
