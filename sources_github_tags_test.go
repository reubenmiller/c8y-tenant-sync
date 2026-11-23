package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMonorepoReleases serves the releases of a release-please monorepo,
// newest first, paged like the GitHub API (per_page, page). It counts the
// pages served.
type fakeMonorepoReleases struct {
	server   *httptest.Server
	mu       sync.Mutex
	releases []map[string]any
	pages    int
}

func newFakeMonorepo(t *testing.T, tags ...string) *fakeMonorepoReleases {
	f := &fakeMonorepoReleases{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/thin-edge/tedge-flows-examples/releases" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pages++
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		start := min((page-1)*perPage, len(f.releases))
		end := min(start+perPage, len(f.releases))
		_ = json.NewEncoder(w).Encode(f.releases[start:end])
	}))
	t.Cleanup(f.server.Close)
	for _, tag := range tags {
		f.add(tag, false)
	}
	return f
}

// add appends a release (older than the ones added before) with the asset
// release-please builds for the flow, e.g. c8y.2Fc8y-deploy-poll_0.6.1.tar.gz
func (f *fakeMonorepoReleases) add(tag string, prerelease bool) {
	var component, version string
	for i := len(tag) - 1; i > 0; i-- {
		if tag[i-1] == '-' && tag[i] == 'v' {
			component, version = tag[:i-1], tag[i+1:]
			break
		}
	}
	asset := "c8y.2F" + component + "_" + version + ".tar.gz"
	f.releases = append(f.releases, map[string]any{
		"tag_name":   tag,
		"prerelease": prerelease,
		"assets": []map[string]any{{
			"name":                 asset,
			"browser_download_url": "https://github.com/thin-edge/tedge-flows-examples/releases/download/" + tag + "/" + asset,
		}},
	})
}

func monorepoResolver(t *testing.T, f *fakeMonorepoReleases) *SourceResolver {
	r := NewSourceResolver(t.TempDir(), t.TempDir(), true)
	r.GitHubAPI = f.server.URL
	return r
}

func resolvedTags(t *testing.T, files []ResolvedFile) []string {
	var hints []string
	for _, file := range files {
		hints = append(hints, file.Filename+" "+file.VersionHint)
	}
	sort.Strings(hints)
	return hints
}

func TestGitHubTagsLatestPerPattern(t *testing.T) {
	f := newFakeMonorepo(t,
		"c8y-deploy-poll-v0.6.1",
		"c8y-deploy-poll-v0.6.0",
		"c8y-deploy-status-v0.3.1",
		"c8y-deploy-operation-v0.3.0",
		"c8y-deploy-status-v0.3.0",
	)
	files, err := monorepoResolver(t, f).Resolve(Source{GitHub: &GitHubSource{
		Repo:     "thin-edge/tedge-flows-examples",
		Tags:     []string{"c8y-deploy-poll-v*", "c8y-deploy-status-v*"},
		LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"c8y.2Fc8y-deploy-poll_0.6.1.tar.gz 0.6.1",
		"c8y.2Fc8y-deploy-status_0.3.1.tar.gz 0.3.1",
	}, resolvedTags(t, files), "the latest release of each pattern, with the component prefix removed from the version")
}

func TestGitHubTagsLatestNAndAll(t *testing.T) {
	f := newFakeMonorepo(t,
		"c8y-deploy-poll-v0.6.1",
		"c8y-deploy-status-v0.3.1",
		"c8y-deploy-poll-v0.6.0",
		"c8y-deploy-poll-v0.5.0",
	)
	r := monorepoResolver(t, f)

	files, err := r.Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"c8y-deploy-poll-v*"}, Release: "latest-2", LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"c8y.2Fc8y-deploy-poll_0.6.0.tar.gz 0.6.0",
		"c8y.2Fc8y-deploy-poll_0.6.1.tar.gz 0.6.1",
	}, resolvedTags(t, files))

	files, err = r.Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"c8y-deploy-*"}, Release: "all", LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Len(t, files, 4)
}

func TestGitHubTagsSkipsPrereleases(t *testing.T) {
	f := newFakeMonorepo(t)
	f.add("c8y-deploy-poll-v0.7.0", true)
	f.add("c8y-deploy-poll-v0.6.1", false)
	r := monorepoResolver(t, f)

	files, err := r.Resolve(Source{GitHub: &GitHubSource{Repo: "thin-edge/tedge-flows-examples", Tags: []string{"c8y-deploy-poll-v*"}, LinkOnly: boolPtr(true)}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c8y.2Fc8y-deploy-poll_0.6.1.tar.gz 0.6.1"}, resolvedTags(t, files))

	files, err = r.Resolve(Source{GitHub: &GitHubSource{Repo: "thin-edge/tedge-flows-examples", Tags: []string{"c8y-deploy-poll-v*"}, IncludePrereleases: true, LinkOnly: boolPtr(true)}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c8y.2Fc8y-deploy-poll_0.7.0.tar.gz 0.7.0"}, resolvedTags(t, files))
}

func TestGitHubTagsPaging(t *testing.T) {
	f := newFakeMonorepo(t)
	// 150 releases of other components first: the release of the pattern
	// is on the second page
	for i := 0; i < 150; i++ {
		f.add(fmt.Sprintf("log-surge-v1.%d.0", 200-i), false)
	}
	f.add("c8y-deploy-poll-v0.6.1", false)
	for i := 0; i < 150; i++ {
		f.add(fmt.Sprintf("uptime-v1.%d.0", 200-i), false)
	}

	files, err := monorepoResolver(t, f).Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"c8y-deploy-poll-v*"}, LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c8y.2Fc8y-deploy-poll_0.6.1.tar.gz 0.6.1"}, resolvedTags(t, files))
	assert.Equal(t, 2, f.pages, "the listing stops once every pattern has its release")
}

func TestGitHubTagsNothingMatches(t *testing.T) {
	f := newFakeMonorepo(t, "c8y-deploy-poll-v0.6.1")
	_, err := monorepoResolver(t, f).Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"missing-v*"},
	}})
	require.ErrorIs(t, err, ErrNothingToDo)
	assert.ErrorContains(t, err, "no releases in thin-edge/tedge-flows-examples matched tags: missing-v*")
}

func TestGitHubTagsValidation(t *testing.T) {
	source := Source{GitHub: &GitHubSource{Repo: "a/b", Tags: []string{"comp-v*"}, Release: "v1.2.3"}}
	assert.ErrorContains(t, source.Validate(), `github.tags can't be combined with the release tag "v1.2.3"`)
	source = Source{GitHub: &GitHubSource{Repo: "a/b", Tags: []string{"[comp"}}}
	assert.ErrorContains(t, source.Validate(), `github.tags: invalid pattern "[comp"`)
	for _, release := range []string{"", "latest", "latest-3", "all"} {
		source = Source{GitHub: &GitHubSource{Repo: "a/b", Tags: []string{"comp-v*"}, Release: release}}
		assert.NoError(t, source.Validate(), release)
	}
}

func TestTagVersion(t *testing.T) {
	assert.Equal(t, "0.6.1", tagVersion("c8y-deploy-poll-v0.6.1", []string{"c8y-deploy-poll-v*"}))
	assert.Equal(t, "v0.6.1", tagVersion("c8y-deploy-poll-v0.6.1", []string{"c8y-deploy-poll-*"}), "v is removed by the caller")
	assert.Equal(t, "c8y-deploy-poll-v0.6.1", tagVersion("c8y-deploy-poll-v0.6.1", []string{"*"}))
	assert.Equal(t, "v1.0.0", tagVersion("v1.0.0", nil))
}

func TestGitHubTagsSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("software:\n  - source:\n      github:\n        repo: thin-edge/tedge-flows-examples\n        tags: [c8y-deploy-poll-v*]\n        release: latest\n")))
}

// TestSyncSoftwareFromMonorepoTags creates one software item per component
func TestSyncSoftwareFromMonorepoTags(t *testing.T) {
	gh := newFakeMonorepo(t, "c8y-deploy-poll-v0.6.1", "c8y-deploy-status-v0.3.1", "c8y-deploy-poll-v0.6.0")
	f := fakeSoftwareRepository(t, nil)
	s := newFakeSyncer(t, f)
	s.Resolver = monorepoResolver(t, gh)
	s.Resolver.SkipDownload = false

	require.NoError(t, s.SyncSoftware(t.Context(), []SoftwareSpec{{
		Source: Source{GitHub: &GitHubSource{
			Repo: "thin-edge/tedge-flows-examples", Tags: []string{"c8y-deploy-poll-v*", "c8y-deploy-status-v*"}, LinkOnly: boolPtr(true),
		}},
	}}))
	var items []string
	for _, r := range s.Results {
		require.NoError(t, r.Err, r.Item)
		items = append(items, r.Item)
	}
	sort.Strings(items)
	assert.Equal(t, []string{
		"c8y/c8y-deploy-poll [archive] 0.6.1",
		"c8y/c8y-deploy-status [archive] 0.3.1",
	}, items)
}
