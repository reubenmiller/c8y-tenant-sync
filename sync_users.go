package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/usergroups"
	groupusers "github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/usergroups/users"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/userroles"
	rolegroups "github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/userroles/usergroups"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/users"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// SyncGlobalRoles ensures global roles (user groups in the Cumulocity API)
// exist with the desired description and permissions. Permission assignments
// are additive: permissions already on the role but absent from the manifest
// are left alone, so built-in global roles (admins, business, ...) can be
// extended safely.
func (s *Syncer) SyncGlobalRoles(ctx context.Context, specs []GlobalRoleSpec) error {
	// Role self links are shared by all groups of the target tenant
	roleSelfLinks := map[string]string{}

	for _, spec := range specs {
		if s.DryRun {
			detail := "ensure global role"
			if len(spec.Permissions) > 0 {
				detail += " with permissions: " + strings.Join(spec.Permissions, ", ")
			}
			s.record(SectionGlobalRoles, spec.Name, ActionPlanned, detail, nil)
			continue
		}

		group := s.Client.UserGroups.GetByName(ctx, usergroups.GetByNameOptions{GroupName: spec.Name})

		action := ActionUnchanged
		var details []string
		var groupID string

		if group.Err != nil {
			// Not found (or not readable): create it
			body := map[string]any{"name": spec.Name}
			if spec.Description != "" {
				body["description"] = spec.Description
			}
			created := s.Client.UserGroups.Create(ctx, body)
			if created.Err != nil {
				s.record(SectionGlobalRoles, spec.Name, ActionFailed, "create global role", created.Err)
				continue
			}
			groupID = created.Data.ID()
			action = ActionCreated
		} else {
			groupID = group.Data.ID()
			if spec.Description != "" && group.Data.Get("description").String() != spec.Description {
				result := s.Client.UserGroups.Update(ctx, usergroups.UpdateOptions{ID: usergroups.ByID(groupID)}, map[string]any{
					"name":        spec.Name,
					"description": spec.Description,
				})
				if result.Err != nil {
					s.record(SectionGlobalRoles, spec.Name, ActionFailed, "update global role", result.Err)
					continue
				}
				action = ActionUpdated
				details = append(details, "description updated")
			}
		}

		assigned, err := s.ensureGroupRoles(ctx, groupID, action == ActionCreated, spec.Permissions, roleSelfLinks)
		if err != nil {
			s.record(SectionGlobalRoles, spec.Name, ActionFailed, "assign permissions", err)
			continue
		}
		if len(assigned) > 0 {
			if action == ActionUnchanged {
				action = ActionUpdated
			}
			details = append(details, "assigned permissions: "+strings.Join(assigned, ", "))
		}
		s.record(SectionGlobalRoles, spec.Name, action, strings.Join(details, "; "), nil)
	}
	return nil
}

// ensureGroupRoles assigns the missing roles to a group and returns the names
// of the roles actually assigned. For a freshly created group the existing
// role lookup is skipped (it has none yet).
func (s *Syncer) ensureGroupRoles(ctx context.Context, groupID string, created bool, roles []string, roleSelfLinks map[string]string) ([]string, error) {
	if len(roles) == 0 {
		return nil, nil
	}

	existing := map[string]bool{}
	if !created {
		result := s.Client.UserRoles.Groups.List(ctx, rolegroups.ListOptions{
			GroupID:           groupID,
			PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
		})
		for item, err := range result.Items() {
			if err != nil {
				return nil, fmt.Errorf("list roles: %w", err)
			}
			existing[item.Name()] = true
		}
	}

	var assigned []string
	for _, roleName := range roles {
		if existing[roleName] {
			continue
		}
		selfLink, ok := roleSelfLinks[roleName]
		if !ok {
			role := s.Client.UserRoles.Get(ctx, userroles.GetOption{Name: roleName})
			if role.Err != nil {
				return nil, fmt.Errorf("role %q: %w", roleName, role.Err)
			}
			selfLink = role.Data.Self()
			roleSelfLinks[roleName] = selfLink
		}
		result := s.Client.UserRoles.Groups.AssignRole(ctx, rolegroups.AssignRoleOptions{GroupID: groupID}, map[string]any{
			"role": map[string]any{"self": selfLink},
		})
		if result.Err != nil {
			return nil, fmt.Errorf("role %q: %w", roleName, result.Err)
		}
		// A conflict means the role was already assigned
		if result.Status != op.StatusDuplicate {
			assigned = append(assigned, roleName)
		}
	}
	return assigned, nil
}

// SyncUsers ensures users exist with the desired profile fields, global roles
// and inventory roles. The password is only used when creating a user
// (existing passwords are never overwritten); global roles are additive:
// global roles the user has but which are absent from the manifest are kept.
//
// A dry run reads the users (when it can) and reports the same changes as a
// real run, without writing them.
func (s *Syncer) SyncUsers(ctx context.Context, specs []UserSpec) error {
	if s.DryRun && s.dryRunReader() == nil {
		// The tenant cannot be read: list the desired users without
		// comparing them
		for _, spec := range specs {
			detail := "ensure user"
			if len(spec.GlobalRoles) > 0 {
				detail += " with global roles: " + strings.Join(spec.GlobalRoles, ", ")
			}
			if len(spec.InventoryRoles) > 0 {
				detail += "; inventory roles: " + describeInventoryAssignments(spec.InventoryRoles)
			}
			s.record(SectionUsers, spec.Username, ActionPlanned, detail, nil)
		}
		return nil
	}

	// Group IDs are resolved once and shared by all users of the target tenant
	groupIDs := map[string]string{}
	// Inventory roles and device groups are looked up once as well
	inventory := &inventoryAssignmentCache{}

	for _, spec := range specs {
		plan, err := s.planUser(ctx, spec, inventory)
		if err != nil {
			s.record(SectionUsers, spec.Username, ActionFailed, plan.failure, err)
			continue
		}
		if s.DryRun {
			s.recordUserPlan(spec, plan)
			continue
		}
		s.applyUserPlan(ctx, spec, plan, groupIDs)
	}
	return nil
}

// userPlan is the change a user needs: shared by the dry run (which reports
// it) and the real run (which applies it)
type userPlan struct {
	// exists is set when the user exists in the tenant
	exists           bool
	userID, userSelf string
	// profile are the profile fields to update (existing users only)
	profile map[string]any
	// globalRoles are the global roles the user is not a member of yet
	globalRoles []string
	// inventory are the changes of the user's inventory roles, and
	// inventoryErr the error of the assignments which cannot be planned
	inventory    []inventoryAssignmentChange
	inventoryErr error
	// failure describes the step which failed (with the returned error)
	failure string
}

// planUser compares the user of the manifest with the tenant
func (s *Syncer) planUser(ctx context.Context, spec UserSpec, inventory *inventoryAssignmentCache) (userPlan, error) {
	var plan userPlan
	existing := s.readClient().Users.GetByUsername(ctx, users.GetByUsernameOptions{Username: spec.Username})
	if existing.Err != nil {
		// Not found: create it. Cumulocity requires an email for new users
		if spec.CreateEmail() == "" {
			plan.failure = "create user"
			return plan, fmt.Errorf("an email is required to create a user: set email, or use an email address as userName")
		}
		plan.globalRoles = slices.Clone(spec.GlobalRoles)
	} else {
		plan.exists = true
		plan.userID = existing.Data.ID()
		plan.userSelf = existing.Data.Self()
		plan.profile = userDiff(spec, existing.Data)
		missing, err := s.missingGlobalRoles(ctx, plan.userID, spec.GlobalRoles)
		if err != nil {
			plan.failure = "assign global roles"
			return plan, err
		}
		plan.globalRoles = missing
	}
	plan.inventory, plan.inventoryErr = s.planInventoryAssignments(ctx, plan.userID, plan.exists, spec.InventoryRoles, inventory)
	return plan, nil
}

// recordUserPlan reports the changes of a user in a dry run
func (s *Syncer) recordUserPlan(spec UserSpec, plan userPlan) {
	var details []string
	if !plan.exists {
		details = append(details, "create user")
	} else if len(plan.profile) > 0 {
		details = append(details, "update "+strings.Join(sortedKeys(plan.profile), ", "))
	}
	if len(plan.globalRoles) > 0 {
		details = append(details, "assign global roles: "+strings.Join(plan.globalRoles, ", "))
	}
	for _, change := range plan.inventory {
		details = append(details, change.describe())
	}
	if plan.inventoryErr != nil {
		s.record(SectionUsers, spec.Username, ActionFailed, strings.Join(append(details, "assign inventory roles"), "; "), plan.inventoryErr)
		return
	}
	if len(details) == 0 {
		s.record(SectionUsers, spec.Username, ActionUnchanged, "", nil)
		return
	}
	s.record(SectionUsers, spec.Username, ActionPlanned, strings.Join(details, "; "), nil)
}

// applyUserPlan writes the changes of a user and records the result
func (s *Syncer) applyUserPlan(ctx context.Context, spec UserSpec, plan userPlan, groupIDs map[string]string) {
	action := ActionUnchanged
	var details []string
	userID, userSelf := plan.userID, plan.userSelf

	if !plan.exists {
		created := s.Client.Users.Create(ctx, userCreateBody(spec))
		if created.Err != nil {
			s.record(SectionUsers, spec.Username, ActionFailed, "create user", created.Err)
			return
		}
		userID = created.Data.ID()
		userSelf = created.Data.Self()
		action = ActionCreated
		if name, ok := s.generatedName(spec.Password); ok {
			details = append(details, "initial password from ${generate:"+name+"}")
		}
	} else {
		if name, ok := s.newlyGeneratedName(spec.Password); ok {
			// Passwords are only set when creating users
			details = append(details, "generated password ${generate:"+name+"} not applied (the user already exists)")
		}
		if len(plan.profile) > 0 {
			result := s.Client.Users.Update(ctx, users.UpdateOptions{ID: users.ByID(userID)}, plan.profile)
			if result.Err != nil {
				s.record(SectionUsers, spec.Username, ActionFailed, "update user", result.Err)
				return
			}
			action = ActionUpdated
			details = append(details, "updated "+strings.Join(sortedKeys(plan.profile), ", "))
		}
	}

	assigned, err := s.assignUserGroups(ctx, userSelf, plan.globalRoles, groupIDs)
	if err != nil {
		s.record(SectionUsers, spec.Username, ActionFailed, "assign global roles", err)
		return
	}
	if len(assigned) > 0 {
		if action == ActionUnchanged {
			action = ActionUpdated
		}
		details = append(details, "assigned global roles: "+strings.Join(assigned, ", "))
	}

	inventoryChanges, err := s.applyInventoryAssignments(ctx, userID, plan.inventory)
	if len(inventoryChanges) > 0 {
		if action == ActionUnchanged {
			action = ActionUpdated
		}
		details = append(details, inventoryChanges...)
	}
	if err = errors.Join(plan.inventoryErr, err); err != nil {
		s.record(SectionUsers, spec.Username, ActionFailed, strings.Join(append(details, "assign inventory roles"), "; "), err)
		return
	}
	s.record(SectionUsers, spec.Username, action, strings.Join(details, "; "), nil)
}

// missingGlobalRoles returns the global roles of the manifest which the
// existing user is not a member of
func (s *Syncer) missingGlobalRoles(ctx context.Context, userID string, groups []string) ([]string, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	member := map[string]bool{}
	result := s.readClient().Users.ListGroupsWithUser(ctx, users.ListGroupsOptions{
		UserID:            userID,
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for item, err := range result.Items() {
		if err != nil {
			return nil, fmt.Errorf("list group memberships: %w", err)
		}
		member[item.Name()] = true
	}
	var missing []string
	for _, groupName := range groups {
		if !member[groupName] && !slices.Contains(missing, groupName) {
			missing = append(missing, groupName)
		}
	}
	return missing, nil
}

// assignUserGroups assigns the user to the groups and returns the names of
// the groups the user was actually assigned to
func (s *Syncer) assignUserGroups(ctx context.Context, userSelf string, groups []string, groupIDs map[string]string) ([]string, error) {
	var assigned []string
	for _, groupName := range groups {
		groupID, ok := groupIDs[groupName]
		if !ok {
			group := s.Client.UserGroups.GetByName(ctx, usergroups.GetByNameOptions{GroupName: groupName})
			if group.Err != nil {
				return nil, fmt.Errorf("group %q: %w", groupName, group.Err)
			}
			groupID = group.Data.ID()
			groupIDs[groupName] = groupID
		}
		result := s.Client.UserGroups.Users.AssignUser(ctx, groupusers.AssignUserOptions{GroupID: groupID}, map[string]any{
			"user": map[string]any{"self": userSelf},
		})
		if result.Err != nil {
			return nil, fmt.Errorf("group %q: %w", groupName, result.Err)
		}
		// A conflict means the user was already a member
		if result.Status != op.StatusDuplicate {
			assigned = append(assigned, groupName)
		}
	}
	return assigned, nil
}

// userCreateBody builds the create request body for a user
// generatedName returns the name of a generated value
func (s *Syncer) generatedName(value string) (string, bool) {
	if s.Secrets == nil || value == "" {
		return "", false
	}
	return s.Secrets.NameOf(value)
}

// newlyGeneratedName returns the name of a value generated in this run
func (s *Syncer) newlyGeneratedName(value string) (string, bool) {
	if s.Secrets == nil || value == "" {
		return "", false
	}
	return s.Secrets.IsGenerated(value)
}

func userCreateBody(spec UserSpec) map[string]any {
	body := map[string]any{
		"userName": spec.Username,
		"enabled":  spec.IsEnabled(),
	}
	if email := spec.CreateEmail(); email != "" {
		body["email"] = email
	}
	if spec.FirstName != "" {
		body["firstName"] = spec.FirstName
	}
	if spec.LastName != "" {
		body["lastName"] = spec.LastName
	}
	if spec.Phone != "" {
		body["phone"] = spec.Phone
	}
	if spec.Password != "" {
		body["password"] = spec.Password
	}
	if spec.SendPasswordResetEmail {
		body["sendPasswordResetEmail"] = true
	}
	return body
}

// userDiff returns the fields whose existing value differs from the manifest.
// Only fields set in the manifest are managed (empty fields are left alone),
// except enabled which defaults to true. The password is deliberately never
// part of an update.
func userDiff(spec UserSpec, existing jsonmodels.User) map[string]any {
	changes := map[string]any{}
	if spec.Email != "" && existing.Email() != spec.Email {
		changes["email"] = spec.Email
	}
	if spec.FirstName != "" && existing.FirstName() != spec.FirstName {
		changes["firstName"] = spec.FirstName
	}
	if spec.LastName != "" && existing.LastName() != spec.LastName {
		changes["lastName"] = spec.LastName
	}
	if spec.Phone != "" && existing.Get("phone").String() != spec.Phone {
		changes["phone"] = spec.Phone
	}
	if existing.Enabled() != spec.IsEnabled() {
		changes["enabled"] = spec.IsEnabled()
	}
	return changes
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
