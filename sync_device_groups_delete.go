package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects/childassets"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects/childdevices"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// maxGroupContents is the number of child assets and child devices named
// when a device group cannot be deleted because it is not empty
const maxGroupContents = 5

// deviceGroupDelete is a device group with state absent (with its child
// groups, which are absent too) found by the deviceGroups section
type deviceGroupDelete struct {
	// parentID is the ID of the parent group (empty at the root level, or
	// in a dry run, which resolves the parent by its path)
	parentID   string
	parentPath string
	spec       DeviceGroupSpec
}

// queueDeviceGroupDelete queues a device group with state absent. It is
// looked up and deleted after the users section, children first. A dry run
// which cannot read the tenant reports the group right away.
func (s *Syncer) queueDeviceGroupDelete(parentID, parentPath string, spec DeviceGroupSpec) {
	if s.DryRun && s.dryRunReader() == nil {
		s.planDeviceGroupDeletesOffline(parentPath, spec)
		return
	}
	s.deviceGroupDeletes = append(s.deviceGroupDeletes, deviceGroupDelete{parentID: parentID, parentPath: parentPath, spec: spec})
}

// planDeviceGroupDeletesOffline reports the absent groups of a dry run which
// cannot read the tenant, children first
func (s *Syncer) planDeviceGroupDeletesOffline(parentPath string, spec DeviceGroupSpec) {
	path := joinGroupPath(parentPath, spec.Name)
	for _, child := range spec.Groups {
		s.planDeviceGroupDeletesOffline(path, child)
	}
	s.record(SectionDeviceGroups, path, ActionPlanned, "delete if it exists and is empty, after the users section", nil)
}

// DeleteDeviceGroups deletes the device groups with state absent, children
// before their parents. A group is only deleted when it is empty: a group
// which still has child assets or child devices fails with what is inside
// (its content is never deleted with it). A dry run reports the deletes.
func (s *Syncer) DeleteDeviceGroups(ctx context.Context) {
	deletes := s.deviceGroupDeletes
	s.deviceGroupDeletes = nil
	for _, item := range deletes {
		parentID, found := item.parentID, true
		if s.DryRun && item.parentPath != "" {
			var err error
			parentID, found, err = s.resolveDeviceGroupPath(ctx, item.parentPath)
			if err != nil {
				path := joinGroupPath(item.parentPath, item.spec.Name)
				s.record(SectionDeviceGroups, path, ActionFailed, "find group", err)
				s.skipDeviceGroupChildren(path, item.spec.Groups)
				continue
			}
		}
		if !found {
			// The parent does not exist, so neither does the group
			s.recordDeviceGroupsAbsent(item.parentPath, item.spec)
			continue
		}
		s.deleteDeviceGroup(ctx, parentID, item.parentPath, item.spec)
	}
}

// deleteDeviceGroup deletes the child groups and then the group itself. It
// reports whether the group no longer exists (deleted, planned in a dry run
// or absent).
func (s *Syncer) deleteDeviceGroup(ctx context.Context, parentID, parentPath string, spec DeviceGroupSpec) bool {
	path := joinGroupPath(parentPath, spec.Name)
	existing, _, err := s.findDeviceGroup(ctx, parentID, spec.Name)
	if err != nil {
		s.record(SectionDeviceGroups, path, ActionFailed, "find group", err)
		s.skipDeviceGroupChildren(path, spec.Groups)
		return false
	}
	if existing == nil {
		s.recordDeviceGroupsAbsent(parentPath, spec)
		return true
	}
	id := existing.ID()

	// Children first: the group is only empty once they are gone. In a dry
	// run they still exist, so the groups which would be deleted are left
	// out of the content.
	removed := map[string]bool{}
	for _, child := range spec.Groups {
		childID := ""
		if s.DryRun {
			if group, _, err := s.findDeviceGroup(ctx, id, child.Name); err == nil && group != nil {
				childID = group.ID()
			}
		}
		if s.deleteDeviceGroup(ctx, id, path, child) && childID != "" {
			removed[childID] = true
		}
	}

	contents, err := s.deviceGroupContents(ctx, id, removed)
	if err != nil {
		s.record(SectionDeviceGroups, path, ActionFailed, "read group content", err)
		return false
	}
	if contents != "" {
		s.record(SectionDeviceGroups, path, ActionFailed, "",
			fmt.Errorf("cannot delete device group: it still has %s; move or delete them first (a group is never deleted with its content)", contents))
		return false
	}
	if s.DryRun {
		s.record(SectionDeviceGroups, path, ActionPlanned, "delete after the users section (fails if not empty)", nil)
		return true
	}
	result := s.Client.ManagedObjects.Delete(ctx, id, managedobjects.DeleteOptions{})
	if result.Err != nil {
		s.record(SectionDeviceGroups, path, ActionFailed, "delete group", result.Err)
		return false
	}
	s.record(SectionDeviceGroups, path, ActionDeleted, "", nil)
	return true
}

// recordDeviceGroupsAbsent reports a group which does not exist, and its
// child groups, as unchanged
func (s *Syncer) recordDeviceGroupsAbsent(parentPath string, spec DeviceGroupSpec) {
	path := joinGroupPath(parentPath, spec.Name)
	for _, child := range spec.Groups {
		s.recordDeviceGroupsAbsent(path, child)
	}
	s.record(SectionDeviceGroups, path, ActionUnchanged, "absent", nil)
}

// resolveDeviceGroupPath looks up a group by its path of names from the root
// level, the way the deviceGroups section finds them. found is false when a
// group on the path does not exist.
func (s *Syncer) resolveDeviceGroupPath(ctx context.Context, path string) (id string, found bool, err error) {
	for _, name := range strings.Split(path, "/") {
		group, _, err := s.findDeviceGroup(ctx, id, name)
		if err != nil {
			return "", false, err
		}
		if group == nil {
			return "", false, nil
		}
		id = group.ID()
	}
	return id, true, nil
}

// deviceGroupContents describes the child assets and child devices of a
// group, e.g. "2 child assets (Sensor 1, Sensor 2)", leaving out the removed
// managed objects. It is empty when the group is empty.
func (s *Syncer) deviceGroupContents(ctx context.Context, id string, removed map[string]bool) (string, error) {
	page := pagination.PaginationOptions{PageSize: maxGroupContents + len(removed), WithTotalElements: true}
	assets := s.readClient().ManagedObjects.ChildAssets.List(ctx, id, childassets.ListOptions{PaginationOptions: page})
	if assets.Err != nil {
		return "", fmt.Errorf("child assets: %w", assets.Err)
	}
	devices := s.readClient().ManagedObjects.ChildDevices.List(ctx, id, childdevices.ListOptions{PaginationOptions: page})
	if devices.Err != nil {
		return "", fmt.Errorf("child devices: %w", devices.Err)
	}
	var parts []string
	if part := describeChildren(assets, removed, "child asset"); part != "" {
		parts = append(parts, part)
	}
	if part := describeChildren(devices, removed, "child device"); part != "" {
		parts = append(parts, part)
	}
	return strings.Join(parts, " and "), nil
}

// describeChildren describes a page of children as "<count> <noun>s (names)"
func describeChildren(result op.Result[jsonmodels.ManagedObject], removed map[string]bool, noun string) string {
	var names []string
	removedCount := 0
	listed := 0
	for item, err := range result.Items() {
		if err != nil {
			break
		}
		listed++
		if removed[item.ID()] {
			removedCount++
			continue
		}
		name := item.Name()
		if name == "" {
			name = "id " + item.ID()
		}
		if len(names) < maxGroupContents {
			names = append(names, name)
		}
	}
	total := int(result.Meta.TotalElements())
	if total < listed {
		total = listed
	}
	count := total - removedCount
	if count <= 0 {
		return ""
	}
	label := noun
	if count != 1 {
		label += "s"
	}
	detail := fmt.Sprintf("%d %s", count, label)
	if len(names) > 0 {
		list := strings.Join(names, ", ")
		if count > len(names) {
			list += ", ..."
		}
		detail += " (" + list + ")"
	}
	return detail
}

// joinGroupPath appends a group name to the path of its parent
func joinGroupPath(parentPath, name string) string {
	if parentPath == "" {
		return name
	}
	return parentPath + "/" + name
}
