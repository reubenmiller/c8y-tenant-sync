package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/usergroups"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users"
)

// deleteSections are the sections whose entries with state absent are
// deleted together once the last of them has run: the users first (in their
// own section), then the inventory roles, the global roles and the device
// groups, so nothing is deleted while a later section still uses it
var deleteSections = []string{SectionDeviceGroups, SectionGlobalRoles, SectionInventoryRoles, SectionUsers}

// deferredDeleteSection returns the section after which the deferred deletes
// run: the last of the delete sections which the --only filter selects (empty
// when it selects none)
func deferredDeleteSection(only []string) string {
	last := ""
	for _, section := range SectionOrder {
		for _, candidate := range deleteSections {
			if section == candidate && sectionEnabled(only, section) {
				last = section
			}
		}
	}
	return last
}

// runDeferredDeletes deletes the inventory roles, global roles and device
// groups with state absent, in that order
func (s *Syncer) runDeferredDeletes(ctx context.Context) {
	s.DeleteInventoryRoles(ctx)
	s.DeleteGlobalRoles(ctx)
}

// globalRoleDelete is a global role with state absent which exists in the
// tenant
type globalRoleDelete struct {
	name string
	id   string
}

// errSyncUserGlobalRole is returned when the manifest would delete a global
// role of the user running the sync
var errSyncUserGlobalRole = errors.New("refusing to delete a global role of the user running the sync")

// planGlobalRoleDelete looks up a global role with state absent. An existing
// role is queued for deletion (a dry run reports it), a missing one is
// reported as unchanged. Built-in global roles and the global roles of the
// user running the sync are never deleted.
func (s *Syncer) planGlobalRoleDelete(ctx context.Context, spec GlobalRoleSpec, syncUserGroups *syncUserGroupsCache) {
	if s.DryRun && s.dryRunReader() == nil {
		s.record(SectionGlobalRoles, spec.Name, ActionPlanned, "delete if it exists, after the users section", nil)
		return
	}
	if isBuiltinGlobalRole(spec.Name) {
		s.record(SectionGlobalRoles, spec.Name, ActionFailed, "",
			fmt.Errorf("refusing to delete the built-in global role %q", spec.Name))
		return
	}
	group := s.readClient().UserGroups.GetByName(ctx, usergroups.GetByNameOptions{GroupName: spec.Name})
	if group.Err != nil {
		if group.HTTPStatus == http.StatusNotFound {
			s.record(SectionGlobalRoles, spec.Name, ActionUnchanged, "absent", nil)
			return
		}
		s.record(SectionGlobalRoles, spec.Name, ActionFailed, "read global role", group.Err)
		return
	}
	id := group.Data.ID()
	member, err := syncUserGroups.contains(ctx, s, id)
	if err != nil {
		s.record(SectionGlobalRoles, spec.Name, ActionFailed, "delete global role", err)
		return
	}
	if member {
		s.record(SectionGlobalRoles, spec.Name, ActionFailed, "",
			fmt.Errorf("%w: %s belongs to %q; remove that user from the role first or run the sync as another user", errSyncUserGlobalRole, syncUserGroups.userName, spec.Name))
		return
	}
	if s.DryRun {
		s.record(SectionGlobalRoles, spec.Name, ActionPlanned, "delete after the users section", nil)
		return
	}
	s.globalRoleDeletes = append(s.globalRoleDeletes, globalRoleDelete{name: spec.Name, id: id})
}

// DeleteGlobalRoles deletes the global roles with state absent. It runs
// after the users section, so users deleted in the same run no longer hold
// the role; Cumulocity removes the role from its remaining members.
func (s *Syncer) DeleteGlobalRoles(ctx context.Context) {
	deletes := s.globalRoleDeletes
	s.globalRoleDeletes = nil
	for _, role := range deletes {
		result := s.Client.UserGroups.Delete(ctx, usergroups.DeleteOptions{ID: usergroups.ByID(role.id)})
		if result.Err != nil {
			s.record(SectionGlobalRoles, role.name, ActionFailed, "delete global role", result.Err)
			continue
		}
		s.record(SectionGlobalRoles, role.name, ActionDeleted, "", nil)
	}
}

// syncUserGroupsCache looks up the global roles of the user running the sync
// once per section
type syncUserGroupsCache struct {
	loaded   bool
	userName string
	groupIDs map[string]bool
	err      error
}

// contains reports whether the user running the sync belongs to the global
// role
func (c *syncUserGroupsCache) contains(ctx context.Context, s *Syncer, groupID string) (bool, error) {
	if !c.loaded {
		c.loaded = true
		c.err = c.load(ctx, s)
	}
	if c.err != nil {
		return false, c.err
	}
	return c.groupIDs[groupID], nil
}

func (c *syncUserGroupsCache) load(ctx context.Context, s *Syncer) error {
	current := s.readClient().Users.CurrentUser.Get(ctx)
	if current.Err != nil {
		return fmt.Errorf("look up the user running the sync: %w", current.Err)
	}
	c.userName = current.Data.UserName()
	c.groupIDs = map[string]bool{}
	result := s.readClient().Users.ListGroupsWithUser(ctx, users.ListGroupsOptions{
		UserID:            current.Data.ID(),
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for item, err := range result.Items() {
		if err != nil {
			return fmt.Errorf("list the global roles of the user running the sync: %w", err)
		}
		c.groupIDs[item.ID()] = true
	}
	return nil
}
