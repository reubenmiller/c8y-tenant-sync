package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInventory is an in-memory inventory of device groups served by the fake
// Cumulocity server: the group query, create, update and the child assets of
// each group
type fakeInventory struct {
	f       *fakeC8y
	mu      sync.Mutex
	objects map[string]map[string]any
	// children maps a parent ID to its child asset IDs
	children map[string][]string
	nextID   int
}

var fakeNameFilter = regexp.MustCompile(`name eq '((?:[^']|'')*)'`)

func newFakeInventory(t *testing.T) *fakeInventory {
	inv := &fakeInventory{
		f:        newFakeC8y(t),
		objects:  map[string]map[string]any{},
		children: map[string][]string{},
		nextID:   100,
	}
	inv.f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		name := ""
		if match := fakeNameFilter.FindStringSubmatch(query); match != nil {
			name = strings.ReplaceAll(match[1], "''", "'")
		}
		inv.mu.Lock()
		var items []map[string]any
		for _, id := range inv.sortedIDs() {
			obj := inv.objects[id]
			if _, ok := obj["c8y_IsDeviceGroup"]; !ok {
				continue
			}
			if name != "" && obj["name"] != name {
				continue
			}
			items = append(items, inv.withParents(id))
		}
		inv.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": items, "statistics": stats()})
	})
	inv.f.Handle("POST /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		inv.mu.Lock()
		inv.nextID++
		id := fmt.Sprintf("%d", inv.nextID)
		inv.mu.Unlock()
		inv.add(id, body)
		writeJSON(w, http.StatusCreated, inv.get(id))
	})
	return inv
}

func (inv *fakeInventory) sortedIDs() []string {
	ids := make([]string, 0, len(inv.objects))
	for id := range inv.objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (inv *fakeInventory) get(id string) map[string]any {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.objects[id]
}

// withParents returns the object with its asset parents (caller holds mu)
func (inv *fakeInventory) withParents(id string) map[string]any {
	out := map[string]any{}
	for k, v := range inv.objects[id] {
		out[k] = v
	}
	refs := []any{}
	for parent, children := range inv.children {
		for _, child := range children {
			if child == id {
				refs = append(refs, map[string]any{"managedObject": map[string]any{"id": parent}})
			}
		}
	}
	out["assetParents"] = map[string]any{"references": refs}
	return out
}

// add stores a managed object and registers its routes
func (inv *fakeInventory) add(id string, obj map[string]any) {
	obj["id"] = id
	inv.mu.Lock()
	inv.objects[id] = obj
	inv.mu.Unlock()

	inv.f.Handle("PUT /inventory/managedObjects/"+id, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		inv.mu.Lock()
		for k, v := range body {
			inv.objects[id][k] = v
		}
		inv.mu.Unlock()
		writeJSON(w, http.StatusOK, inv.get(id))
	})
	inv.f.Handle("GET /inventory/managedObjects/"+id+"/childAssets", func(w http.ResponseWriter, r *http.Request) {
		inv.mu.Lock()
		refs := []any{}
		// Like real Cumulocity: the referenced managed objects only carry
		// id, name and self (no fragments)
		for _, child := range inv.children[id] {
			obj := inv.objects[child]
			refs = append(refs, map[string]any{"managedObject": map[string]any{
				"id": obj["id"], "name": obj["name"], "self": "http://fake/inventory/managedObjects/" + child,
			}})
		}
		inv.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"references": refs, "statistics": stats()})
	})
	inv.f.Handle("POST /inventory/managedObjects/"+id+"/childAssets", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		childID := fmt.Sprint(body["managedObject"].(map[string]any)["id"])
		inv.link(id, childID)
		writeJSON(w, http.StatusCreated, map[string]any{})
	})
}

func (inv *fakeInventory) link(parentID, childID string) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.children[parentID] = append(inv.children[parentID], childID)
}

// group adds an existing device group, optionally below a parent
func (inv *fakeInventory) group(id, name, groupType, parentID string, extra map[string]any) {
	obj := map[string]any{"name": name, "type": groupType, "c8y_IsDeviceGroup": map[string]any{}}
	for k, v := range extra {
		obj[k] = v
	}
	inv.add(id, obj)
	if parentID != "" {
		inv.link(parentID, id)
	}
}

func (inv *fakeInventory) syncer(t *testing.T) *Syncer {
	return newFakeSyncer(t, inv.f)
}

func deviceGroupResults(s *Syncer) map[string]ChangeResult {
	out := map[string]ChangeResult{}
	for _, r := range s.Results {
		if r.Section == SectionDeviceGroups {
			out[r.Item] = r
		}
	}
	return out
}

func exampleDeviceGroupTree() []DeviceGroupSpec {
	return []DeviceGroupSpec{{
		Name:        "Europe",
		Description: "European sites",
		Fragments:   map[string]any{"customer_Region": map[string]any{"code": "EU"}},
		Groups: []DeviceGroupSpec{{
			Name:   "Plant Berlin",
			Groups: []DeviceGroupSpec{{Name: "Line 1"}, {Name: "Line 2"}},
		}},
	}}
}

func TestSyncDeviceGroupsCreatesTree(t *testing.T) {
	inv := newFakeInventory(t)
	syncer := inv.syncer(t)

	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), exampleDeviceGroupTree()))

	results := deviceGroupResults(syncer)
	require.Len(t, results, 4, "results: %+v", syncer.Results)
	assert.Equal(t, ActionCreated, results["Europe"].Action)
	assert.Equal(t, ActionCreated, results["Europe/Plant Berlin"].Action)
	assert.Equal(t, "linked to Europe", results["Europe/Plant Berlin"].Detail)
	assert.Equal(t, ActionCreated, results["Europe/Plant Berlin/Line 1"].Action)
	assert.Equal(t, "linked to Europe/Plant Berlin", results["Europe/Plant Berlin/Line 1"].Detail)
	assert.Equal(t, ActionCreated, results["Europe/Plant Berlin/Line 2"].Action)

	creates := inv.f.Requests(http.MethodPost, "/inventory/managedObjects")
	var bodies []map[string]any
	for _, req := range creates {
		if req.Path == "/inventory/managedObjects" {
			bodies = append(bodies, req.Body)
		}
	}
	require.Len(t, bodies, 4)
	root := bodies[0]
	assert.Equal(t, "Europe", root["name"])
	assert.Equal(t, "c8y_DeviceGroup", root["type"])
	assert.Equal(t, "European sites", root["description"])
	assert.Equal(t, map[string]any{"code": "EU"}, root["customer_Region"])
	assert.Contains(t, root, "c8y_IsDeviceGroup")
	assert.Contains(t, root, SyncToolFragment)
	assert.Equal(t, "c8y_DeviceSubgroup", bodies[1]["type"])

	// The children are linked to their parents
	assert.Equal(t, []string{"102"}, inv.children["101"])
	assert.Equal(t, []string{"103", "104"}, inv.children["102"])

	// A second run changes nothing
	again := inv.syncer(t)
	require.NoError(t, again.SyncDeviceGroups(context.Background(), exampleDeviceGroupTree()))
	for item, result := range deviceGroupResults(again) {
		assert.Equal(t, ActionUnchanged, result.Action, item)
	}
	assert.Len(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"), 0)
}

func TestSyncDeviceGroupsExistingUnchanged(t *testing.T) {
	inv := newFakeInventory(t)
	inv.group("1", "Europe", "c8y_DeviceGroup", "", map[string]any{
		"description": "European sites",
		// Additional values in a fragment are allowed
		"customer_Region": map[string]any{"code": "EU", "manager": "someone"},
	})
	inv.group("2", "Plant Berlin", "c8y_DeviceSubgroup", "1", nil)
	inv.group("3", "Line 1", "c8y_DeviceSubgroup", "2", nil)
	inv.group("4", "Line 2", "c8y_DeviceSubgroup", "2", nil)
	// A group with the same name below another parent is not a match
	inv.group("5", "Asia", "c8y_DeviceGroup", "", nil)
	inv.group("6", "Plant Berlin", "c8y_DeviceSubgroup", "5", nil)

	syncer := inv.syncer(t)
	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), exampleDeviceGroupTree()))

	results := deviceGroupResults(syncer)
	require.Len(t, results, 4)
	for item, result := range results {
		assert.Equal(t, ActionUnchanged, result.Action, item)
	}
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"))
	assert.Empty(t, inv.f.Requests(http.MethodPost, "/inventory/managedObjects"))

	// Root groups are found with a device group query by name
	queries := inv.f.Requests(http.MethodGet, "/inventory/managedObjects")
	require.NotEmpty(t, queries)
	assert.Contains(t, queries[0].Query, "has%28c8y_IsDeviceGroup%29")
	assert.Contains(t, queries[0].Query, "name+eq+%27Europe%27")
	assert.Contains(t, queries[0].Query, "withParents=true")
}

func TestSyncDeviceGroupsUpdatesDescriptionAndFragments(t *testing.T) {
	inv := newFakeInventory(t)
	inv.group("1", "Europe", "c8y_DeviceGroup", "", map[string]any{
		"description":     "old",
		"customer_Region": map[string]any{"code": "US"},
	})

	syncer := inv.syncer(t)
	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), []DeviceGroupSpec{{
		Name:        "Europe",
		Description: "European sites",
		Fragments:   map[string]any{"customer_Region": map[string]any{"code": "EU"}},
	}}))

	result := resultFor(t, syncer, SectionDeviceGroups)
	assert.Equal(t, ActionUpdated, result.Action)
	assert.Equal(t, "updated customer_Region, description", result.Detail)

	updates := inv.f.Requests(http.MethodPut, "/inventory/managedObjects/1")
	require.Len(t, updates, 1)
	assert.Equal(t, "European sites", updates[0].Body["description"])
	assert.Equal(t, map[string]any{"code": "EU"}, updates[0].Body["customer_Region"])
	assert.Contains(t, updates[0].Body, SyncToolFragment)
	assert.NotContains(t, updates[0].Body, "name", "only changed properties are sent")
}

func TestSyncDeviceGroupsLinksMissingChildAsset(t *testing.T) {
	inv := newFakeInventory(t)
	inv.group("1", "Europe", "c8y_DeviceGroup", "", nil)
	// e.g. created by an earlier run which failed to link it
	inv.group("2", "Plant Berlin", "c8y_DeviceSubgroup", "", nil)
	// A root group with the same name is never moved below another group
	inv.group("3", "Plant Berlin", "c8y_DeviceGroup", "", nil)

	syncer := inv.syncer(t)
	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), []DeviceGroupSpec{{
		Name:   "Europe",
		Groups: []DeviceGroupSpec{{Name: "Plant Berlin"}},
	}}))

	results := deviceGroupResults(syncer)
	assert.Equal(t, ActionUnchanged, results["Europe"].Action)
	assert.Equal(t, ActionUpdated, results["Europe/Plant Berlin"].Action)
	assert.Equal(t, "linked to Europe", results["Europe/Plant Berlin"].Detail)
	assert.Equal(t, []string{"2"}, inv.children["1"])

	links := inv.f.Requests(http.MethodPost, "/inventory/managedObjects/1/childAssets")
	require.Len(t, links, 1)
	assert.Equal(t, map[string]any{"managedObject": map[string]any{"id": "2"}}, links[0].Body)
}

// TestSyncDeviceGroupsChildLookupUsesQuery checks that nested groups are found
// by the inventory query with their asset parents, not by the child asset
// references of the parent (which only carry id, name and self)
func TestSyncDeviceGroupsChildLookupUsesQuery(t *testing.T) {
	inv := newFakeInventory(t)
	inv.group("1", "Europe", "c8y_DeviceGroup", "", nil)
	inv.group("2", "Plant Berlin", "c8y_DeviceSubgroup", "1", nil)
	// A device (not a group) with the same name below the parent is ignored
	inv.add("3", map[string]any{"name": "Plant Berlin", "c8y_IsDevice": map[string]any{}})
	inv.link("1", "3")

	syncer := inv.syncer(t)
	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), []DeviceGroupSpec{{
		Name:   "Europe",
		Groups: []DeviceGroupSpec{{Name: "Plant Berlin"}},
	}}))

	results := deviceGroupResults(syncer)
	assert.Equal(t, ActionUnchanged, results["Europe"].Action)
	assert.Equal(t, ActionUnchanged, results["Europe/Plant Berlin"].Action)
	assert.Empty(t, inv.f.Requests(http.MethodPost, "/inventory/managedObjects"), "nothing is created or linked")
	assert.Empty(t, inv.f.Requests(http.MethodGet, "/inventory/managedObjects/1/childAssets"))

	queries := inv.f.Requests(http.MethodGet, "/inventory/managedObjects")
	require.Len(t, queries, 2)
	assert.Contains(t, queries[1].Query, "name+eq+%27Plant+Berlin%27")
	assert.Contains(t, queries[1].Query, "withParents=true")
}

func TestSyncDeviceGroupsAmbiguousName(t *testing.T) {
	inv := newFakeInventory(t)
	inv.group("1", "Europe", "c8y_DeviceGroup", "", nil)
	inv.group("2", "Europe", "c8y_DeviceGroup", "", nil)
	inv.group("3", "Asia", "c8y_DeviceGroup", "", nil)
	inv.group("4", "Plant", "c8y_DeviceSubgroup", "3", nil)
	inv.group("5", "Plant", "c8y_DeviceSubgroup", "3", nil)

	syncer := inv.syncer(t)
	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), []DeviceGroupSpec{
		{Name: "Europe", Groups: []DeviceGroupSpec{{Name: "Plant Berlin"}}},
		{Name: "Asia", Groups: []DeviceGroupSpec{{Name: "Plant"}}},
	}))

	results := deviceGroupResults(syncer)
	assert.Equal(t, ActionFailed, results["Europe"].Action)
	assert.ErrorContains(t, results["Europe"].Err, `2 device groups named "Europe" at the root level (ids: 1, 2)`)
	assert.Equal(t, ActionSkipped, results["Europe/Plant Berlin"].Action, "children of a failed group are skipped")
	assert.Equal(t, ActionUnchanged, results["Asia"].Action, "other groups are still synced")
	assert.Equal(t, ActionFailed, results["Asia/Plant"].Action)
	assert.ErrorContains(t, results["Asia/Plant"].Err, `2 device groups named "Plant" in the parent group (ids: 4, 5)`)
	assert.Empty(t, inv.f.Requests(http.MethodPost, "/inventory/managedObjects"), "nothing is created on ambiguity")
}

func TestSyncDeviceGroupsDryRun(t *testing.T) {
	syncer := &Syncer{DryRun: true}
	require.NoError(t, syncer.SyncDeviceGroups(context.Background(), exampleDeviceGroupTree()))

	require.Len(t, syncer.Results, 4)
	assert.Equal(t, "Europe", syncer.Results[0].Item)
	assert.Equal(t, "ensure root group", syncer.Results[0].Detail)
	assert.Equal(t, "Europe/Plant Berlin", syncer.Results[1].Item)
	assert.Equal(t, "ensure group in Europe", syncer.Results[1].Detail)
	assert.Equal(t, "Europe/Plant Berlin/Line 1", syncer.Results[2].Item)
	assert.Equal(t, "Europe/Plant Berlin/Line 2", syncer.Results[3].Item)
	for _, result := range syncer.Results {
		assert.Equal(t, ActionPlanned, result.Action)
		assert.Equal(t, SectionDeviceGroups, result.Section)
	}
}

func TestDeviceGroupDiffFragments(t *testing.T) {
	assert.True(t, jsonContains(map[string]any{"a": 1}, `{"a":1,"b":2}`))
	assert.True(t, jsonContains(map[string]any{"a": map[string]any{"x": "y"}}, `{"a":{"x":"y","z":1}}`))
	assert.False(t, jsonContains(map[string]any{"a": 1}, `{"a":2}`))
	assert.False(t, jsonContains([]any{1, 2}, `[1,2,3]`), "lists must be equal")
	assert.True(t, jsonContains("text", `"text"`))
	assert.False(t, jsonContains(map[string]any{"a": 1}, `"text"`))
}

func TestLoadManifestDeviceGroups(t *testing.T) {
	path := writeManifest(t, `
deviceGroups:
  - name: Europe
    description: European sites
    fragments:
      customer_Region:
        code: EU
    groups:
      - name: Plant Berlin
        groups:
          - name: Line 1
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	require.Len(t, manifest.DeviceGroups, 1)
	europe := manifest.DeviceGroups[0]
	assert.Equal(t, "Europe", europe.Name)
	assert.Equal(t, map[string]any{"customer_Region": map[string]any{"code": "EU"}}, europe.Fragments)
	require.Len(t, europe.Groups, 1)
	assert.Equal(t, "Line 1", europe.Groups[0].Groups[0].Name)
}

func TestValidateDeviceGroups(t *testing.T) {
	testCases := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "empty name",
			manifest: `
deviceGroups:
  - name: Europe
    groups:
      - name: ""
`,
			want: "deviceGroups[0].groups[0]: name is required",
		},
		{
			name: "missing root name",
			manifest: `
deviceGroups:
  - description: no name
`,
			want: "deviceGroups[0]: name is required",
		},
		{
			name: "duplicate sibling names",
			manifest: `
deviceGroups:
  - name: Europe
    groups:
      - name: Plant Berlin
      - name: Plant Berlin
`,
			want: `deviceGroups[0].groups[1]: duplicate group name "Plant Berlin"`,
		},
		{
			name: "duplicate root names",
			manifest: `
deviceGroups:
  - name: Europe
  - name: Europe
`,
			want: `deviceGroups[1]: duplicate group name "Europe"`,
		},
		{
			name: "reserved fragment",
			manifest: `
deviceGroups:
  - name: Europe
    fragments:
      type: custom
`,
			want: `deviceGroups[0].fragments: "type" can not be set as a fragment`,
		},
		{
			name: "surrounding spaces",
			manifest: `
deviceGroups:
  - name: "Europe "
`,
			want: "must not start or end with spaces",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.manifest))
			assert.ErrorContains(t, err, tc.want)
		})
	}

	// The same name in different parents is fine
	_, err := LoadManifest(writeManifest(t, `
deviceGroups:
  - name: Europe
    groups: [{name: Line 1}]
  - name: Asia
    groups: [{name: Line 1}]
`))
	assert.NoError(t, err)
}

func TestListDeviceGroupTree(t *testing.T) {
	inv := newFakeInventory(t)
	inv.group("1", "Europe", "c8y_DeviceGroup", "", map[string]any{
		"description":     "European sites",
		SyncToolFragment:  map[string]any{"tool": "tenant-sync"},
		"customer_Region": map[string]any{"code": "EU"},
	})
	inv.group("2", "Plant Berlin", "c8y_DeviceSubgroup", "1", nil)
	inv.group("3", "Line 1", "c8y_DeviceSubgroup", "2", nil)
	inv.group("4", "Asia", "c8y_DeviceGroup", "", nil)

	groups, err := listDeviceGroupTree(context.Background(), inv.f.Client().DeviceGroups)
	require.NoError(t, err)
	assert.Equal(t, []DeviceGroupSpec{
		{Name: "Asia"},
		{Name: "Europe", Description: "European sites", Groups: []DeviceGroupSpec{
			{Name: "Plant Berlin", Groups: []DeviceGroupSpec{{Name: "Line 1"}}},
		}},
	}, groups)
}
