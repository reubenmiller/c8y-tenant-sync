package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/tidwall/gjson"
)

// Smart group managed object markers. The Cumulocity UI creates smart groups
// (dynamic groups) with the type c8y_DynamicGroup and the c8y_IsDynamicGroup
// fragment; the devices of the group are selected by the inventory query in
// c8y_DeviceQueryString.
const (
	smartGroupFragment      = "c8y_IsDynamicGroup"
	smartGroupType          = "c8y_DynamicGroup"
	smartGroupQueryFragment = "c8y_DeviceQueryString"
	smartGroupColumns       = "c8y_DeviceColumnsConfig"
	// smartGroupInvisible marks the helper groups the UI creates (e.g. for
	// bulk operations): c8y_IsDynamicGroup.invisible = {}
	smartGroupInvisible = "invisible"
)

// reservedSmartGroupFragments are managed object properties which can not be
// set with fragments: they are set from the other fields, managed by
// Cumulocity, or are the bookkeeping fragment of this tool
var reservedSmartGroupFragments = []string{
	"id", "self", "name", "type", "description", "owner", "creationTime", "lastUpdated",
	"childAssets", "childDevices", "childAdditions", "assetParents", "deviceParents", "additionParents",
	smartGroupFragment, smartGroupQueryFragment, smartGroupColumns, SyncToolFragment,
	// The device group marker would make the smart group a device group as
	// well (device group lookups and export select has(c8y_IsDeviceGroup))
	deviceGroupFragment,
}

// smartGroupPageSize is the page size for smart group lookups
const smartGroupPageSize = 2000

// defaultSmartGroupColumns is the device list table configuration the UI
// gives a new smart group; the declared table settings are merged into it on
// create
func defaultSmartGroupColumns() map[string]any {
	return map[string]any{
		"columns":    []any{},
		"pagination": map[string]any{"currentPage": 1, "pageSize": smartGroupDefaultPageSize},
	}
}

// smartGroupDefaultPageSize is the page size the UI gives the device list
// table of a new smart group
const smartGroupDefaultPageSize = 20

// SyncSmartGroups ensures the smart groups exist with their query, columns,
// visibility, description and fragments, or (state absent) that they do not
// exist. A smart group is identified by its name among the smart groups of
// the tenant; smart groups which are not listed are never changed. Smart
// groups are flat: they are not linked into the asset hierarchy, and
// deleting one never deletes devices (its devices are only selected by the
// query).
//
// A dry run compares with the tenant when it can read it, and otherwise
// lists the planned changes.
func (s *Syncer) SyncSmartGroups(ctx context.Context, specs []SmartGroupSpec) error {
	for _, spec := range specs {
		if spec.Absent() {
			s.deleteSmartGroup(ctx, spec)
			continue
		}
		s.ensureSmartGroup(ctx, spec)
	}
	return nil
}

// ensureSmartGroup finds, creates or updates a single smart group and
// records the result
func (s *Syncer) ensureSmartGroup(ctx context.Context, spec SmartGroupSpec) {
	reader := s.readClient()
	if reader == nil {
		// A dry run which cannot read the tenant
		s.record(SectionSmartGroups, spec.Name, ActionPlanned, "ensure smart group", nil)
		return
	}

	existing, err := findSmartGroup(ctx, reader, spec.Name, spec.Invisible != nil && *spec.Invisible)
	if err != nil {
		s.record(SectionSmartGroups, spec.Name, ActionFailed, "find smart group", err)
		return
	}

	if existing == nil {
		if s.DryRun {
			s.record(SectionSmartGroups, spec.Name, ActionPlanned, "create", nil)
			return
		}
		created := s.Client.ManagedObjects.Create(ctx, smartGroupBody(spec))
		if created.Err != nil {
			s.record(SectionSmartGroups, spec.Name, ActionFailed, "create smart group", created.Err)
			return
		}
		s.record(SectionSmartGroups, spec.Name, ActionCreated, "", nil)
		return
	}

	changes := smartGroupDiff(spec, *existing)
	if len(changes) == 0 {
		s.record(SectionSmartGroups, spec.Name, ActionUnchanged, "", nil)
		return
	}
	detail := "update " + strings.Join(smartGroupChangeNames(changes), ", ")
	if s.DryRun {
		s.record(SectionSmartGroups, spec.Name, ActionPlanned, detail, nil)
		return
	}
	names := smartGroupChangeNames(changes)
	changes[SyncToolFragment] = syncMeta()
	result := s.Client.ManagedObjects.Update(ctx, existing.ID(), changes)
	if result.Err != nil {
		s.record(SectionSmartGroups, spec.Name, ActionFailed, "update smart group", result.Err)
		return
	}
	s.record(SectionSmartGroups, spec.Name, ActionUpdated, "updated "+strings.Join(names, ", "), nil)
}

// deleteSmartGroup deletes a smart group with state absent if it exists.
// Smart groups have no child assets (their devices are selected by the
// query), so deleting one never deletes or moves devices.
func (s *Syncer) deleteSmartGroup(ctx context.Context, spec SmartGroupSpec) {
	reader := s.readClient()
	if reader == nil {
		s.record(SectionSmartGroups, spec.Name, ActionPlanned, "delete if it exists", nil)
		return
	}
	existing, err := findSmartGroup(ctx, reader, spec.Name, spec.Invisible != nil && *spec.Invisible)
	if err != nil {
		s.record(SectionSmartGroups, spec.Name, ActionFailed, "find smart group", err)
		return
	}
	if existing == nil {
		s.record(SectionSmartGroups, spec.Name, ActionUnchanged, "absent", nil)
		return
	}
	if s.DryRun {
		s.record(SectionSmartGroups, spec.Name, ActionPlanned, "delete", nil)
		return
	}
	// No cascade: only the smart group managed object is deleted
	result := s.Client.ManagedObjects.Delete(ctx, existing.ID(), managedobjects.DeleteOptions{})
	if result.Err != nil {
		s.record(SectionSmartGroups, spec.Name, ActionFailed, "delete smart group", result.Err)
		return
	}
	s.record(SectionSmartGroups, spec.Name, ActionDeleted, "", nil)
}

// smartGroupBody is the managed object of a new smart group
func smartGroupBody(spec SmartGroupSpec) map[string]any {
	body := map[string]any{}
	for key, value := range spec.Fragments {
		body[key] = value
	}
	marker := map[string]any{}
	if spec.Invisible != nil && *spec.Invisible {
		marker[smartGroupInvisible] = map[string]any{}
	}
	query := ""
	if spec.Query != nil {
		query = *spec.Query
	}
	// The declared table settings are merged into the UI default, so a
	// partial table (e.g. only the page size) keeps the other defaults
	columns := deepMerge(defaultSmartGroupColumns(), spec.Table.columnsConfig())
	body["name"] = spec.Name
	body["type"] = smartGroupType
	body[smartGroupFragment] = marker
	body[smartGroupQueryFragment] = query
	body[smartGroupColumns] = columns
	body[SyncToolFragment] = syncMeta()
	if spec.Description != "" {
		body["description"] = spec.Description
	}
	return body
}

// smartGroupDiff returns the properties of an existing smart group which
// differ from the manifest. Only properties set in the manifest are managed:
// an omitted query, description, table setting or invisible is left alone.
// The table columns are compared as a whole list and the page size by value;
// changed settings are merged into the existing c8y_DeviceColumnsConfig, so
// the settings the manifest leaves out (e.g. the current page) are kept.
// Fragments count as unchanged when the existing value contains all desired
// values (the UI or other tools may add more); a changed fragment replaces
// the fragment as a whole (as for device groups). A missing query fragment
// counts as an empty query.
func smartGroupDiff(spec SmartGroupSpec, existing jsonmodels.ManagedObject) map[string]any {
	changes := map[string]any{}
	if spec.Query != nil {
		// A missing fragment is the same as no filter, like an empty string
		if existing.Get(smartGroupQueryFragment).String() != *spec.Query {
			changes[smartGroupQueryFragment] = *spec.Query
		}
	}
	if spec.Description != "" && existing.Get("description").String() != spec.Description {
		changes["description"] = spec.Description
	}
	if desired := spec.Table.columnsConfig(); desired != nil {
		current := existing.Get(smartGroupColumns)
		if !current.Exists() || !jsonContains(desired, current.Raw) {
			// Cumulocity replaces a fragment as a whole: send the existing
			// configuration with the declared settings merged in
			base, _ := current.Value().(map[string]any)
			changes[smartGroupColumns] = deepMerge(cloneMap(base), desired)
		}
	}
	if spec.Invisible != nil && isInvisibleSmartGroup(existing) != *spec.Invisible {
		// Keep the other properties of the marker fragment
		marker := map[string]any{}
		if current, ok := existing.Get(smartGroupFragment).Value().(map[string]any); ok {
			for key, value := range current {
				marker[key] = value
			}
		}
		if *spec.Invisible {
			marker[smartGroupInvisible] = map[string]any{}
		} else {
			// The fragment is replaced as a whole, so leaving the
			// property out removes it
			delete(marker, smartGroupInvisible)
		}
		changes[smartGroupFragment] = marker
	}
	for key, value := range spec.Fragments {
		current := existing.Get(gjson.Escape(key))
		if !current.Exists() || !jsonContains(value, current.Raw) {
			changes[key] = value
		}
	}
	return changes
}

// smartGroupChangeNames names the changed properties with the manifest
// fields where there is one (query, table, invisible), sorted
func smartGroupChangeNames(changes map[string]any) []string {
	names := make([]string, 0, len(changes))
	for key := range changes {
		switch key {
		case smartGroupQueryFragment:
			names = append(names, "query")
		case smartGroupColumns:
			names = append(names, "table")
		case smartGroupFragment:
			names = append(names, "invisible")
		case SyncToolFragment:
		default:
			names = append(names, key)
		}
	}
	sort.Strings(names)
	return names
}

// isInvisibleSmartGroup reports whether a smart group is hidden from the
// group list of the UI
func isInvisibleSmartGroup(item jsonmodels.ManagedObject) bool {
	value := item.Get(smartGroupFragment + "." + smartGroupInvisible)
	return value.Exists() && value.Type != gjson.Null
}

// findSmartGroup looks up a smart group by name. When several smart groups
// have the name, only those with the wanted visibility count (visible ones,
// unless the manifest sets invisible: true), so a hidden helper group of the
// UI does not clash with a visible group of the same name; export uses the
// same rule. Several remaining matches are an error: the manifest can not
// tell which one is meant.
func findSmartGroup(ctx context.Context, client *api.Client, name string, invisible bool) (*jsonmodels.ManagedObject, error) {
	result := client.ManagedObjects.List(ctx, managedobjects.ListOptions{
		Query:             fmt.Sprintf("has(%s) and name eq '%s'", smartGroupFragment, escapeQueryValue(name)),
		PaginationOptions: pagination.PaginationOptions{PageSize: smartGroupPageSize},
	})
	var matches []jsonmodels.ManagedObject
	for item, err := range result.Items() {
		if err != nil {
			return nil, fmt.Errorf("query smart groups: %w", err)
		}
		// The query language treats * as a wildcard and may not match the
		// case exactly: keep exact matches only
		if item.Name() == name {
			matches = append(matches, item)
		}
	}
	if len(matches) > 1 {
		var wanted []jsonmodels.ManagedObject
		for _, match := range matches {
			if isInvisibleSmartGroup(match) == invisible {
				wanted = append(wanted, match)
			}
		}
		if len(wanted) > 0 {
			matches = wanted
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID())
		}
		return nil, duplicateSmartGroupsError(name, ids)
	}
}

// duplicateSmartGroupsError reports several smart groups with one name
func duplicateSmartGroupsError(name string, ids []string) error {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	return fmt.Errorf("%d smart groups named %q (ids: %s): rename or remove the duplicates so the smart group is unique",
		len(ids), name, strings.Join(ids, ", "))
}

// listSmartGroups reads the smart groups of the tenant for export, sorted
// by name. The helper groups the UI hides (c8y_IsDynamicGroup.invisible)
// are left out, and so are bookkeeping and platform properties: only the
// name, description, query and a non-default column configuration are kept.
// Several visible smart groups with one name are an error, since the
// manifest could not be validated or synced.
func listSmartGroups(ctx context.Context, client *api.Client) ([]SmartGroupSpec, error) {
	opts := managedobjects.ListOptions{
		Query:             fmt.Sprintf("has(%s)", smartGroupFragment),
		PaginationOptions: pagination.PaginationOptions{PageSize: smartGroupPageSize},
	}
	seen := map[string]bool{}
	idsByName := map[string][]string{}
	var specs []SmartGroupSpec
	for item, err := range client.ManagedObjects.ListAll(ctx, opts).Items() {
		if err != nil {
			return nil, err
		}
		if seen[item.ID()] || isInvisibleSmartGroup(item) {
			continue
		}
		seen[item.ID()] = true
		idsByName[item.Name()] = append(idsByName[item.Name()], item.ID())
		spec := SmartGroupSpec{
			Name:        item.Name(),
			Description: item.Get("description").String(),
		}
		// A group without the query fragment is exported without a query,
		// so the exported manifest is unchanged on the same tenant
		if query := item.Get(smartGroupQueryFragment); query.Exists() {
			value := query.String()
			spec.Query = &value
		}
		spec.Table = exportSmartGroupTable(item)
		specs = append(specs, spec)
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	for i := 1; i < len(specs); i++ {
		if specs[i].Name == specs[i-1].Name {
			return nil, duplicateSmartGroupsError(specs[i].Name, idsByName[specs[i].Name])
		}
	}
	return specs, nil
}

// integralNumbers converts the whole numbers of a decoded JSON value (which
// are float64) to int, so the exported value is the same as the value read
// back from the YAML manifest
func integralNumbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = integralNumbers(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = integralNumbers(item)
		}
		return out
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int(v)
		}
		return v
	default:
		return value
	}
}

// exportSmartGroupTable returns the table settings of a smart group which
// differ from the UI default: the columns when there are any and the page
// size when it is not 20 (nil when both are the default)
func exportSmartGroupTable(item jsonmodels.ManagedObject) *SmartGroupTableSpec {
	table := &SmartGroupTableSpec{}
	for _, column := range item.Get(smartGroupColumns + ".columns").Array() {
		if value, ok := column.Value().(map[string]any); ok {
			table.Columns = append(table.Columns, integralNumbers(value).(map[string]any))
		}
	}
	if pageSize := item.Get(smartGroupColumns + ".pagination.pageSize"); pageSize.Exists() && pageSize.Int() != smartGroupDefaultPageSize {
		value := int(pageSize.Int())
		table.PageSize = &value
	}
	if table.Columns == nil && table.PageSize == nil {
		return nil
	}
	return table
}
