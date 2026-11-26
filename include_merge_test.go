package main

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMergeManifestsKeepsEverySection guards against new manifest sections
// being forgotten in mergeManifests, which would silently drop them from
// manifests that use include or several files on the command line
func TestMergeManifestsKeepsEverySection(t *testing.T) {
	skip := map[string]bool{
		"Include":       true, // consumed by the loader
		"IncludedFiles": true, // set by the loader
		"Hooks":         true, // covered by the include tests
	}
	manifestType := reflect.TypeOf(Manifest{})
	for i := 0; i < manifestType.NumField(); i++ {
		field := manifestType.Field(i)
		if !field.IsExported() || skip[field.Name] {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			for _, fromOver := range []bool{false, true} {
				base, over := &Manifest{}, &Manifest{}
				target := base
				if fromOver {
					target = over
				}
				value := reflect.ValueOf(target).Elem().Field(i)
				switch field.Type.Kind() {
				case reflect.Slice:
					value.Set(reflect.MakeSlice(field.Type, 1, 1))
				case reflect.Pointer:
					value.Set(reflect.New(field.Type.Elem()))
				case reflect.Map:
					value.Set(reflect.MakeMap(field.Type))
					value.SetMapIndex(reflect.New(field.Type.Key()).Elem(), reflect.New(field.Type.Elem()).Elem())
				default:
					t.Fatalf("unhandled manifest field kind %s: add it to mergeManifests and this test", field.Type.Kind())
				}

				merged := reflect.ValueOf(mergeManifests(base, over)).Elem().Field(i)
				if field.Type.Kind() == reflect.Slice || field.Type.Kind() == reflect.Map {
					assert.Equal(t, 1, merged.Len(), "%s is dropped by mergeManifests (from the later file: %v)", field.Name, fromOver)
				} else {
					assert.False(t, merged.IsNil(), "%s is dropped by mergeManifests (from the later file: %v)", field.Name, fromOver)
				}
			}
		})
	}
}

func TestIncludeMergesDeviceGroupsAndInventoryRoles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base.yaml", []byte(`deviceGroups:
  - name: Europe
    groups:
      - name: Plant Berlin
  - name: Asia
inventoryRoles:
  - name: Site operator
    permissions:
      - {scope: ALARM, permission: READ}
`))
	main := writeFile(t, dir, "tenant.yaml", []byte(`include: [base.yaml]
deviceGroups:
  - name: Europe
    groups:
      - name: Plant Munich
inventoryRoles:
  - name: Site operator
    permissions:
      - {scope: ALARM, permission: ADMIN}
  - name: Viewer
    permissions:
      - {scope: MANAGED_OBJECT, permission: READ}
`))
	manifest, err := LoadManifest(main)
	if !assert.NoError(t, err) {
		return
	}
	if assert.Len(t, manifest.DeviceGroups, 2) {
		assert.Equal(t, "Europe", manifest.DeviceGroups[0].Name)
		assert.Equal(t, "Plant Munich", manifest.DeviceGroups[0].Groups[0].Name, "the later tree replaces the earlier one")
		assert.Equal(t, "Asia", manifest.DeviceGroups[1].Name)
	}
	if assert.Len(t, manifest.InventoryRoles, 2) {
		assert.Equal(t, "ADMIN", manifest.InventoryRoles[0].Permissions[0].Permission)
		assert.Equal(t, "Viewer", manifest.InventoryRoles[1].Name)
	}
}
