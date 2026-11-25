package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/devicegroups"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/model"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/tidwall/gjson"
)

// Device group managed object markers. The Cumulocity UI creates root groups
// with the type c8y_DeviceGroup and nested groups with c8y_DeviceSubgroup.
const (
	deviceGroupFragment = "c8y_IsDeviceGroup"
	deviceGroupType     = "c8y_DeviceGroup"
	deviceSubgroupType  = "c8y_DeviceSubgroup"
)

// reservedDeviceGroupFragments are managed object properties which can not be
// set with fragments: they are set from the other fields, managed by
// Cumulocity, or are the bookkeeping fragment of this tool
var reservedDeviceGroupFragments = []string{
	"id", "self", "name", "type", "description", "owner", "creationTime", "lastUpdated",
	"childAssets", "childDevices", "childAdditions", "assetParents", "deviceParents", "additionParents",
	deviceGroupFragment, SyncToolFragment,
}

// deviceGroupPageSize is the page size for device group lookups
const deviceGroupPageSize = 2000

// SyncDeviceGroups ensures the tree of device groups exists. A group is
// identified by its name within its parent: a root group by name among the
// groups without a parent, a nested group by name among the groups whose asset
// parents include its parent. Missing groups are created and linked to their parent; the
// description and fragments are updated when they differ. Groups are never
// deleted or unlinked.
func (s *Syncer) SyncDeviceGroups(ctx context.Context, specs []DeviceGroupSpec) error {
	for _, spec := range specs {
		s.syncDeviceGroup(ctx, "", "", spec)
	}
	return nil
}

// syncDeviceGroup ensures a group below the parent (an empty parentID is the
// root level) and then its children
func (s *Syncer) syncDeviceGroup(ctx context.Context, parentID, parentPath string, spec DeviceGroupSpec) {
	path := spec.Name
	if parentPath != "" {
		path = parentPath + "/" + spec.Name
	}

	if s.DryRun {
		detail := "ensure root group"
		if parentPath != "" {
			detail = "ensure group in " + parentPath
		}
		s.record(SectionDeviceGroups, path, ActionPlanned, detail, nil)
		// Users of the same dry run can be given inventory roles on it
		s.pending().deviceGroups[path] = true
		s.pending().deviceGroups[spec.Name] = true
		for _, child := range spec.Groups {
			s.syncDeviceGroup(ctx, "", path, child)
		}
		return
	}

	id, err := s.ensureDeviceGroup(ctx, parentID, parentPath, path, spec)
	if err != nil {
		// Without the group its children can not be placed
		s.skipDeviceGroupChildren(path, spec.Groups)
		return
	}
	for _, child := range spec.Groups {
		s.syncDeviceGroup(ctx, id, path, child)
	}
}

// skipDeviceGroupChildren records the subtree of a group which failed
func (s *Syncer) skipDeviceGroupChildren(parentPath string, children []DeviceGroupSpec) {
	for _, child := range children {
		path := parentPath + "/" + child.Name
		s.record(SectionDeviceGroups, path, ActionSkipped, "parent group "+parentPath+" failed", nil)
		s.skipDeviceGroupChildren(path, child.Groups)
	}
}

// ensureDeviceGroup finds, creates, links and updates a single group and
// records the result. It returns the ID of the group.
func (s *Syncer) ensureDeviceGroup(ctx context.Context, parentID, parentPath, path string, spec DeviceGroupSpec) (string, error) {
	fail := func(detail string, err error) (string, error) {
		s.record(SectionDeviceGroups, path, ActionFailed, detail, err)
		return "", err
	}

	existing, needsLink, err := s.findDeviceGroup(ctx, parentID, spec.Name)
	if err != nil {
		return fail("find group", err)
	}

	if existing == nil {
		groupType := deviceGroupType
		if parentID != "" {
			groupType = deviceSubgroupType
		}
		body := map[string]any{}
		for key, value := range spec.Fragments {
			body[key] = value
		}
		body["name"] = spec.Name
		body["type"] = groupType
		body[deviceGroupFragment] = map[string]any{}
		body[SyncToolFragment] = syncMeta()
		if spec.Description != "" {
			body["description"] = spec.Description
		}
		created := s.Client.DeviceGroups.Create(ctx, body)
		if created.Err != nil {
			return fail("create group", created.Err)
		}
		id := created.Data.ID()
		detail := ""
		if parentID != "" {
			if err := s.linkDeviceGroup(ctx, parentID, id); err != nil {
				return fail("link to "+parentPath, err)
			}
			detail = "linked to " + parentPath
		}
		s.record(SectionDeviceGroups, path, ActionCreated, detail, nil)
		return id, nil
	}

	id := existing.ID()
	var details []string
	changes := deviceGroupDiff(spec, *existing)
	if len(changes) > 0 {
		names := sortedKeys(changes)
		changes[SyncToolFragment] = syncMeta()
		result := s.Client.ManagedObjects.Update(ctx, id, changes)
		if result.Err != nil {
			return fail("update group", result.Err)
		}
		details = append(details, "updated "+strings.Join(names, ", "))
	}
	if needsLink {
		if err := s.linkDeviceGroup(ctx, parentID, id); err != nil {
			return fail("link to "+parentPath, err)
		}
		details = append(details, "linked to "+parentPath)
	}

	action := ActionUnchanged
	if len(details) > 0 {
		action = ActionUpdated
	}
	s.record(SectionDeviceGroups, path, action, strings.Join(details, "; "), nil)
	return id, nil
}

// linkDeviceGroup assigns a group as a child asset of its parent
func (s *Syncer) linkDeviceGroup(ctx context.Context, parentID, childID string) error {
	return s.Client.ManagedObjects.ChildAssets.Assign(ctx, parentID, childID).Err
}

// findDeviceGroup looks up a group by name within its parent. needsLink is
// set when the group was found without any parent and must still be linked
// to the parent (e.g. a previous run created it but failed to link it).
// Several matches are an error: the manifest can not tell which one is meant.
//
// All device groups with the name are read with their asset parents in one
// inventory query; the child asset references of the parent are not used,
// since their managed objects often only carry id, name and self.
func (s *Syncer) findDeviceGroup(ctx context.Context, parentID, name string) (group *jsonmodels.ManagedObject, needsLink bool, err error) {
	candidates, err := s.deviceGroupsByName(ctx, name)
	if err != nil {
		return nil, false, err
	}

	if parentID != "" {
		var children []jsonmodels.ManagedObject
		for _, item := range candidates {
			if hasAssetParent(item, parentID) {
				children = append(children, item)
			}
		}
		switch len(children) {
		case 0:
		case 1:
			return &children[0], false, nil
		default:
			return nil, false, ambiguousDeviceGroupError(name, "in the parent group", children)
		}
	}

	// Root level, or a nested group which is not linked to its parent yet:
	// groups with the name which have no parent group. Root groups and
	// unlinked subgroups are told apart by their type, so a root group is
	// never moved below another group.
	var orphans []jsonmodels.ManagedObject
	for _, item := range candidates {
		if !hasAnyAssetParent(item) && isDeviceSubgroup(item) == (parentID != "") {
			orphans = append(orphans, item)
		}
	}
	switch len(orphans) {
	case 0:
		return nil, false, nil
	case 1:
		return &orphans[0], parentID != "", nil
	default:
		scope := "at the root level"
		if parentID != "" {
			scope = "as unlinked subgroups"
		}
		return nil, false, ambiguousDeviceGroupError(name, scope, orphans)
	}
}

func ambiguousDeviceGroupError(name, scope string, matches []jsonmodels.ManagedObject) error {
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		ids = append(ids, match.ID())
	}
	sort.Strings(ids)
	return fmt.Errorf("%d device groups named %q %s (ids: %s): rename or remove the duplicates so the group is unique",
		len(matches), name, scope, strings.Join(ids, ", "))
}

// deviceGroupsByName returns the device groups with exactly the name,
// including their asset parents
func (s *Syncer) deviceGroupsByName(ctx context.Context, name string) ([]jsonmodels.ManagedObject, error) {
	opts := devicegroups.ListOptions{
		Query: model.NewInventoryQuery().
			HasFragment(deviceGroupFragment).
			AddFilterEqStr("name", escapeQueryValue(name)).
			Build(),
		GetOptions: managedobjects.GetOptions{WithParents: true},
	}
	opts.PageSize = deviceGroupPageSize
	result := s.Client.DeviceGroups.List(ctx, opts)
	var matches []jsonmodels.ManagedObject
	for item, err := range result.Items() {
		if err != nil {
			return nil, fmt.Errorf("query device groups: %w", err)
		}
		// The query match is not necessarily exact (e.g. case), so compare
		// the name again
		if item.Name() == name {
			matches = append(matches, item)
		}
	}
	return matches, nil
}

// hasAnyAssetParent reports whether a managed object (read with withParents)
// is a child asset of another managed object
func hasAnyAssetParent(item jsonmodels.ManagedObject) bool {
	return len(item.Get("assetParents.references").Array()) > 0
}

// hasAssetParent reports whether a managed object (read with withParents) is
// a child asset of the parent
func hasAssetParent(item jsonmodels.ManagedObject, parentID string) bool {
	for _, ref := range item.Get("assetParents.references").Array() {
		if ref.Get("managedObject.id").String() == parentID {
			return true
		}
	}
	return false
}

// isDeviceSubgroup reports whether a group has the type of a nested group
func isDeviceSubgroup(item jsonmodels.ManagedObject) bool {
	return strings.EqualFold(item.Type(), deviceSubgroupType)
}

// deviceGroupDiff returns the properties of an existing group which differ
// from the manifest. Only properties set in the manifest are managed: an
// empty description is left alone, and a fragment counts as unchanged when
// the existing value contains all desired values (Cumulocity or other tools
// may add more).
func deviceGroupDiff(spec DeviceGroupSpec, existing jsonmodels.ManagedObject) map[string]any {
	changes := map[string]any{}
	if spec.Description != "" && existing.Get("description").String() != spec.Description {
		changes["description"] = spec.Description
	}
	for key, value := range spec.Fragments {
		current := existing.Get(gjson.Escape(key))
		if !current.Exists() || !jsonContains(value, current.Raw) {
			changes[key] = value
		}
	}
	return changes
}

// jsonContains reports whether the existing JSON value contains the desired
// value: objects may have additional keys, all other values must be equal
func jsonContains(desired any, existingRaw string) bool {
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false
	}
	var want, have any
	if err := json.Unmarshal(desiredJSON, &want); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(existingRaw), &have); err != nil {
		return false
	}
	return containsValue(want, have)
}

func containsValue(want, have any) bool {
	wantMap, ok := want.(map[string]any)
	if !ok {
		return reflect.DeepEqual(want, have)
	}
	haveMap, ok := have.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range wantMap {
		current, ok := haveMap[key]
		if !ok || !containsValue(value, current) {
			return false
		}
	}
	return true
}

// listDeviceGroupTree reads all device groups of the tenant and returns them
// as a tree (used by export). Bookkeeping and platform properties are left
// out; only the name, description and the child groups are kept.
func listDeviceGroupTree(ctx context.Context, service *devicegroups.Service) ([]DeviceGroupSpec, error) {
	opts := devicegroups.ListOptions{
		Query:      model.NewInventoryQuery().HasFragment(deviceGroupFragment).Build(),
		GetOptions: managedobjects.GetOptions{WithParents: true},
	}
	opts.PaginationOptions = pagination.PaginationOptions{PageSize: deviceGroupPageSize}

	type node struct {
		spec     DeviceGroupSpec
		parents  []string
		children []string
	}
	nodes := map[string]*node{}
	var order []string
	for item, err := range service.ListAll(ctx, opts).Items() {
		if err != nil {
			return nil, err
		}
		if _, seen := nodes[item.ID()]; seen {
			continue
		}
		n := &node{spec: DeviceGroupSpec{Name: item.Name(), Description: item.Get("description").String()}}
		for _, ref := range item.Get("assetParents.references").Array() {
			n.parents = append(n.parents, ref.Get("managedObject.id").String())
		}
		nodes[item.ID()] = n
		order = append(order, item.ID())
	}

	// A group is placed below its first parent which is a device group, so
	// groups linked to several parents appear once
	var roots []string
	for _, id := range order {
		n := nodes[id]
		placed := false
		for _, parentID := range n.parents {
			if parent, ok := nodes[parentID]; ok && parentID != id {
				parent.children = append(parent.children, id)
				placed = true
				break
			}
		}
		if !placed {
			roots = append(roots, id)
		}
	}

	var build func(ids []string, visiting map[string]bool) []DeviceGroupSpec
	build = func(ids []string, visiting map[string]bool) []DeviceGroupSpec {
		specs := make([]DeviceGroupSpec, 0, len(ids))
		for _, id := range ids {
			if visiting[id] {
				continue // a cycle in the asset hierarchy
			}
			visiting[id] = true
			n := nodes[id]
			spec := n.spec
			spec.Groups = build(n.children, visiting)
			if len(spec.Groups) == 0 {
				spec.Groups = nil
			}
			specs = append(specs, spec)
			delete(visiting, id)
		}
		sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
		return specs
	}
	return build(roots, map[string]bool{}), nil
}
