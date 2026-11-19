package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExistingConfiguration serves configuration items which already exist
// (id 700 for "mosquitto", 710 for "collectd") and records uploads and updates
func fakeExistingConfiguration(t *testing.T) *fakeC8y {
	f := newFakeC8y(t)
	existing := map[string]map[string]any{
		"mosquitto": {"id": "700", "name": "mosquitto", "type": "c8y_ConfigurationDump", "configurationType": "mosquitto.conf", "url": f.server.URL + "/inventory/binaries/old"},
		"collectd":  {"id": "710", "name": "collectd", "type": "c8y_ConfigurationDump", "configurationType": "collectd.conf", "url": f.server.URL + "/inventory/binaries/old2"},
	}
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		items := []any{}
		for name, item := range existing {
			if strings.Contains(r.URL.Query().Get("query"), "'"+name+"'") {
				items = append(items, item)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": items, "statistics": stats()})
	})
	for _, item := range existing {
		item := item
		id := item["id"].(string)
		f.JSON("GET /inventory/managedObjects/"+id, http.StatusOK, item)
		f.JSON("PUT /inventory/managedObjects/"+id, http.StatusOK, item)
		f.JSON("POST /inventory/managedObjects/"+id+"/childAdditions", http.StatusCreated, map[string]any{})
	}
	f.JSON("POST /inventory/binaries", http.StatusCreated, map[string]any{"id": "801", "self": f.server.URL + "/inventory/binaries/801"})
	f.JSON("DELETE /inventory/binaries/old", http.StatusNoContent, nil)
	return f
}

func TestConfigurationExistingIsNotReplacedWithoutForce(t *testing.T) {
	f := fakeExistingConfiguration(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{{
		Name: "mosquitto", ConfigurationType: "mosquitto.conf", Content: "listener 8883\n",
	}}))

	assert.NotEqual(t, ActionFailed, resultFor(t, s, SectionConfiguration).Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"), "existing items are kept without force")
}

func TestConfigurationEntryForce(t *testing.T) {
	f := fakeExistingConfiguration(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{
		{Name: "mosquitto", ConfigurationType: "mosquitto.conf", Content: "listener 8883\n", Force: true},
		// Not forced: stays as it is, although it is in the same run
		{Name: "collectd", ConfigurationType: "collectd.conf", Content: "Interval 10\n"},
	}))

	results := resultsByItem(s)
	mosquitto := results["mosquitto (mosquitto.conf)"]
	require.NoError(t, mosquitto.Err)
	assert.Equal(t, ActionUpdated, mosquitto.Action)
	assert.NotEqual(t, ActionFailed, results["collectd (collectd.conf)"].Action)

	uploads := f.Requests(http.MethodPost, "/inventory/binaries")
	require.Len(t, uploads, 1, "only the forced entry is uploaded")
	assert.Contains(t, string(uploads[0].RawBody), "listener 8883", "the new content is uploaded")
	assert.NotContains(t, string(uploads[0].RawBody), "Interval 10")
	assert.NotEmpty(t, f.Requests(http.MethodPut, "/inventory/managedObjects/700"), "the forced item is updated")
	assert.Empty(t, f.Requests(http.MethodPut, "/inventory/managedObjects/710"), "the other item is not changed")
}

func TestConfigurationGlobalForceStillApplies(t *testing.T) {
	f := fakeExistingConfiguration(t)
	s := newFakeSyncer(t, f)
	s.Force = true

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{
		{Name: "collectd", ConfigurationType: "collectd.conf", Content: "Interval 10\n"},
	}))
	assert.Len(t, f.Requests(http.MethodPost, "/inventory/binaries"), 1)
}

func TestConfigurationForceInManifest(t *testing.T) {
	path := writeManifest(t, `configuration:
  - name: mosquitto
    configurationType: mosquitto.conf
    force: true
    content: |
      listener 1883
  - configurationType: collectd.conf
    force: true
    source:
      path: ./config/collectd.conf
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.True(t, manifest.Configuration[0].Force)
	assert.True(t, manifest.Configuration[1].Force, "force also applies to entries with a source")
	assert.NoError(t, validateYAML(t, compileSchema(t), []byte("configuration:\n  - name: a\n    configurationType: x\n    force: true\n    content: b\n")))
}
