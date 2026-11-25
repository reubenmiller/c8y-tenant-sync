package main

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// demoCleanupManifest deletes everything step 1 of the demo creates
const demoCleanupManifest = `
users:
  - {userName: demo-berlin-ops, state: absent}
  - {userName: demo-paris-tech, state: absent}
  - {userName: demo-contractor, state: absent}
inventoryRoles:
  - {name: Demo site operator, state: absent}
  - {name: Demo remote restart, state: absent}
globalRoles:
  - {name: Demo site staff, state: absent}
deviceGroups:
  - name: Demo Europe
    state: absent
    groups:
      - {name: Demo Plant Berlin, state: absent}
      - {name: Demo Plant Paris, state: absent}
`

// demoTenant is the tenant after step 1 of the demo: three users who have
// the global role Demo site staff and the inventory roles on the plants.
// Like Cumulocity, it refuses to delete an inventory role which a user still
// has, and deleting a user removes the user's assignments.
type demoTenant struct {
	inv *fakeInventory
	mu  sync.Mutex
	// users by name, with the IDs of their inventory roles
	users map[string][]string
	// roles are the inventory roles by ID
	roles map[string]string
	// globalRoles are the global roles by name
	globalRoles map[string]string
	// deletes are the deleted objects in order ("user demo-contractor")
	deletes []string
}

func newDemoTenant(t *testing.T) *demoTenant {
	inv := newFakeInventory(t)
	inv.group("100", "Demo Europe", deviceGroupType, "", nil)
	inv.group("101", "Demo Plant Berlin", deviceSubgroupType, "100", nil)
	inv.group("102", "Demo Plant Paris", deviceSubgroupType, "100", nil)
	d := &demoTenant{
		inv: inv,
		users: map[string][]string{
			"demo-berlin-ops": {"21"},
			"demo-paris-tech": {"21", "22"},
			"demo-contractor": {"22"},
		},
		roles:       map[string]string{"21": "Demo site operator", "22": "Demo remote restart"},
		globalRoles: map[string]string{"Demo site staff": "40"},
	}
	f := inv.f
	fakeSyncUserGroups(f, "tenant-sync", map[string]any{"id": "2", "name": "admins"})

	for name := range d.users {
		f.Handle("GET /user/t123/userByName/"+name, func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			_, ok := d.users[name]
			d.mu.Unlock()
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "identity/Not Found", "message": "no user"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": name, "userName": name, "self": "http://example/user/t123/users/" + name})
		})
		f.Handle("DELETE /user/t123/users/"+name, func(w http.ResponseWriter, r *http.Request) {
			d.deleted("user "+name, func() { delete(d.users, name) })
			w.WriteHeader(http.StatusNoContent)
		})
	}

	f.Handle("GET /user/inventoryroles", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		roles := []map[string]any{}
		for id, name := range d.roles {
			roles = append(roles, map[string]any{"id": id, "name": name})
		}
		d.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"roles": roles, "statistics": stats()})
	})
	for id, name := range d.roles {
		f.Handle("DELETE /user/inventoryroles/"+id, func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			for _, roleIDs := range d.users {
				if slices.Contains(roleIDs, id) {
					d.mu.Unlock()
					writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "undefined/validationError", "message": "Cannot remove used InventoryRole"})
					return
				}
			}
			d.mu.Unlock()
			d.deleted("inventory role "+name, func() { delete(d.roles, id) })
			w.WriteHeader(http.StatusNoContent)
		})
	}

	f.Handle("GET /user/t123/groupByName/Demo site staff", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		id, ok := d.globalRoles["Demo site staff"]
		d.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "identity/Not Found", "message": "no group"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": "Demo site staff"})
	})
	f.Handle("DELETE /user/t123/groups/40", func(w http.ResponseWriter, r *http.Request) {
		d.deleted("global role Demo site staff", func() { delete(d.globalRoles, "Demo site staff") })
		w.WriteHeader(http.StatusNoContent)
	})
	return d
}

// deleted records a delete and applies it
func (d *demoTenant) deleted(label string, apply func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	apply()
	d.deletes = append(d.deletes, label)
}

func TestApplyDemoCleanupManifest(t *testing.T) {
	manifest, err := LoadManifest(writeManifest(t, demoCleanupManifest))
	require.NoError(t, err)
	d := newDemoTenant(t)
	for _, id := range []string{"100", "101", "102"} {
		d.inv.f.Handle("DELETE /inventory/managedObjects/"+id, d.deleteGroup(id))
	}

	// A dry run reports every delete and writes nothing
	dry := newDryRunReaderSyncer(d.inv.f)
	require.NoError(t, dry.applyTarget(context.Background(), context.Background(), manifest, nil))
	for _, r := range dry.Results {
		require.NoError(t, r.Err, "%s %s", r.Section, r.Item)
		assert.Equal(t, ActionPlanned, r.Action, "%s %s", r.Section, r.Item)
		assert.Contains(t, r.Detail, "delete", "%s %s", r.Section, r.Item)
	}
	assert.Len(t, dry.Results, 9)
	assertOnlyReads(t, d.inv.f)

	// The first run deletes everything, users first and device groups last
	s := d.inv.syncer(t)
	require.NoError(t, s.applyTarget(context.Background(), context.Background(), manifest, nil))
	for _, r := range s.Results {
		require.NoError(t, r.Err, "%s %s", r.Section, r.Item)
		assert.Equal(t, ActionDeleted, r.Action, "%s %s", r.Section, r.Item)
	}
	assert.Len(t, s.Results, 9)
	assert.Equal(t, []string{
		"user demo-berlin-ops",
		"user demo-paris-tech",
		"user demo-contractor",
		"inventory role Demo site operator",
		"inventory role Demo remote restart",
		"global role Demo site staff",
		"device group Demo Plant Berlin",
		"device group Demo Plant Paris",
		"device group Demo Europe",
	}, d.deletes)

	// The second run has nothing left to delete
	s2 := d.inv.syncer(t)
	require.NoError(t, s2.applyTarget(context.Background(), context.Background(), manifest, nil))
	for _, r := range s2.Results {
		require.NoError(t, r.Err, "%s %s", r.Section, r.Item)
		assert.Equal(t, ActionUnchanged, r.Action, "%s %s", r.Section, r.Item)
		assert.Equal(t, "absent", r.Detail, "%s %s", r.Section, r.Item)
	}
	assert.Len(t, s2.Results, 9)
	assert.Len(t, d.deletes, 9, "the second run deletes nothing")
}

// deleteGroup wraps the delete of a device group of the fake inventory so
// it is recorded
func (d *demoTenant) deleteGroup(id string) http.HandlerFunc {
	inv := d.inv
	return func(w http.ResponseWriter, r *http.Request) {
		inv.mu.Lock()
		name, _ := inv.objects[id]["name"].(string)
		delete(inv.objects, id)
		delete(inv.children, id)
		for parent, children := range inv.children {
			inv.children[parent] = slices.DeleteFunc(children, func(child string) bool { return child == id })
		}
		inv.mu.Unlock()
		d.deleted("device group "+name, func() {})
		w.WriteHeader(http.StatusNoContent)
	}
}
