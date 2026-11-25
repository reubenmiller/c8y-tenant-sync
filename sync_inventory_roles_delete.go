package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users/devicepermissions"
)

// maxRoleHolders is the number of users and groups named when an inventory
// role cannot be deleted because it is still assigned
const maxRoleHolders = 10

// roleDelete is an inventory role with state absent which exists in the
// tenant
type roleDelete struct {
	name string
	id   int64
}

// DeleteInventoryRoles deletes the inventory roles with state absent. It runs
// after the users section, so the roles which the manifest revokes in the same
// run (inventoryRolesMode: exact) no longer count as assigned.
//
// Cumulocity refuses to delete a role which is still assigned to a user. The
// role then fails with the users and device groups which still have it, so
// they can be added to the manifest or revoked by hand. Assignments of users
// the manifest does not manage are never removed.
func (s *Syncer) DeleteInventoryRoles(ctx context.Context) {
	deletes := s.roleDeletes
	s.roleDeletes = nil
	for _, role := range deletes {
		result := s.Client.InventoryRoles.Delete(ctx, role.id)
		if result.Err == nil {
			s.record(SectionInventoryRoles, role.name, ActionDeleted, "", nil)
			continue
		}
		if result.HTTPStatus != http.StatusUnprocessableEntity {
			s.record(SectionInventoryRoles, role.name, ActionFailed, "delete inventory role", result.Err)
			continue
		}
		holders, total, err := s.inventoryRoleHolders(ctx, role.id)
		if err != nil || total == 0 {
			s.record(SectionInventoryRoles, role.name, ActionFailed, "delete inventory role: it is still assigned", result.Err)
			continue
		}
		detail := "still assigned to " + strings.Join(holders, ", ")
		if total > len(holders) {
			detail += fmt.Sprintf(" and %d more", total-len(holders))
		}
		s.record(SectionInventoryRoles, role.name, ActionFailed, "",
			fmt.Errorf("cannot delete inventory role: %s; revoke it there or declare those users with the role removed", detail))
	}
}

// inventoryRoleHolders returns the assignments which give the role, as
// "userName on group" (at most maxRoleHolders, sorted), and the total number
// of such assignments. Cumulocity has no query for the assignments of a role,
// so the assignments of every user are read.
func (s *Syncer) inventoryRoleHolders(ctx context.Context, roleID int64) ([]string, int, error) {
	var holders []string
	groupNames := map[string]string{}
	result := s.Client.Users.ListAll(ctx, users.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for user, err := range result.Items() {
		if err != nil {
			return nil, 0, err
		}
		assignments := s.Client.DevicePermissions.ListInventoryRoleAssignments(ctx, devicepermissions.ListInventoryRoleAssignmentOptions{
			UserScopedOptions: devicepermissions.UserScopedOptions{UserID: user.ID()},
			PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
		})
		for item, err := range assignments.Items() {
			if err != nil {
				return nil, 0, err
			}
			if !slices.Contains(item.RoleIDs(), roleID) {
				continue
			}
			groupID := item.ManagedObjectID()
			name, ok := groupNames[groupID]
			if !ok {
				name = s.assignmentGroupName(ctx, groupID)
				groupNames[groupID] = name
			}
			holders = append(holders, user.UserName()+" on "+name)
		}
	}
	slices.Sort(holders)
	total := len(holders)
	if total > maxRoleHolders {
		holders = holders[:maxRoleHolders]
	}
	return holders, total, nil
}
