package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCloudsmith serves the package listing API for one repository: it
// applies the name, format and version terms of the query, sorts by
// version (newest first, like sort=-version) and pages the results. It
// counts the pages served, to check that listings stop early.
type fakeCloudsmith struct {
	server   *httptest.Server
	mu       sync.Mutex
	packages []map[string]any
	pages    int
	queries  []string
}

func newFakeCloudsmith(t *testing.T, packages ...map[string]any) *fakeCloudsmith {
	f := &fakeCloudsmith{packages: packages}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

var fakeQueryTerm = regexp.MustCompile(`(\w+):(\S+)`)

func (f *fakeCloudsmith) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/dl/"+filepath.Base(r.URL.Path) {
		_, _ = w.Write([]byte("binary of " + filepath.Base(r.URL.Path)))
		return
	}
	if r.URL.Path != "/v1/packages/thinedge/tedge-release/" {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query().Get("query")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages++
	f.queries = append(f.queries, query)

	var matched []map[string]any
	for _, pkg := range f.packages {
		ok := true
		for _, term := range fakeQueryTerm.FindAllStringSubmatch(query, -1) {
			field, value := term[1], term[2]
			re := regexp.MustCompile(value)
			if !re.MatchString(fmt.Sprint(pkg[field])) {
				ok = false
			}
		}
		if ok {
			matched = append(matched, pkg)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return compareVersions(fmt.Sprint(matched[i]["version"]), fmt.Sprint(matched[j]["version"])) > 0
	})

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	total := (len(matched) + size - 1) / size
	start := min((page-1)*size, len(matched))
	end := min(start+size, len(matched))
	w.Header().Set("X-Pagination-Page", strconv.Itoa(page))
	w.Header().Set("X-Pagination-Pagetotal", strconv.Itoa(total))
	_ = json.NewEncoder(w).Encode(matched[start:end])
}

// compareVersions compares dotted numeric versions (enough for the tests)
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na - nb
		}
	}
	return len(pa) - len(pb)
}

func (f *fakeCloudsmith) package_(name, version, format, arch, filename string) map[string]any {
	archs := []map[string]any{}
	if arch != "" {
		archs = append(archs, map[string]any{"name": arch})
	}
	return map[string]any{
		"name": name, "version": version, "format": format, "filename": filename,
		"cdn_url":           f.server.URL + "/dl/" + filename,
		"slug_perm":         name + "-" + version + "-" + format + "-" + arch,
		"is_sync_completed": true,
		"architectures":     archs,
	}
}

func cloudsmithResolver(t *testing.T, f *fakeCloudsmith) *SourceResolver {
	r := NewSourceResolver(t.TempDir(), t.TempDir(), false)
	r.CloudsmithAPI = f.server.URL + "/v1"
	return r
}

// tedgeReleases adds the tedge deb packages of versions for two
// architectures, plus an apk and an rpm of the newest version
func tedgeReleases(t *testing.T, versions ...string) *fakeCloudsmith {
	f := newFakeCloudsmith(t)
	for _, v := range versions {
		for _, arch := range []string{"arm64", "amd64"} {
			f.packages = append(f.packages, f.package_("tedge", v, "deb", arch, "tedge_"+v+"_"+arch+".deb"))
		}
		f.packages = append(f.packages, f.package_("tedge-agent", v, "deb", "arm64", "tedge-agent_"+v+"_arm64.deb"))
	}
	newest := versions[0]
	f.packages = append(f.packages,
		f.package_("tedge", newest+"-r0", "alpine", "aarch64", "tedge-"+newest+"-r0.apk"),
		f.package_("tedge", newest+"-r0", "alpine", "x86_64", "tedge-"+newest+"-r0.apk"),
		f.package_("tedge", newest+"-1", "rpm", "aarch64", "tedge-"+newest+"-1.aarch64.rpm"),
	)
	return f
}

func filenames(files []ResolvedFile) []string {
	var names []string
	for _, f := range files {
		names = append(names, f.Filename)
	}
	sort.Strings(names)
	return names
}

func TestCloudsmithLatest(t *testing.T) {
	f := tedgeReleases(t, "2.1.0", "2.0.1", "2.0.0")
	files, err := cloudsmithResolver(t, f).Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo:     "thinedge/tedge-release",
		Packages: []string{"tedge"},
		Formats:  []string{"deb"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"tedge_2.1.0_amd64.deb", "tedge_2.1.0_arm64.deb"}, filenames(files),
		"every architecture of the latest version, and not tedge-agent")
	for _, file := range files {
		assert.Equal(t, "2.1.0", file.Package.Version)
		assert.NotEmpty(t, file.URL, "linked by default")
		assert.Empty(t, file.Path)
	}
	assert.Contains(t, f.queries[0], "name:^tedge$ AND format:deb")
}

func TestCloudsmithLatestNStopsEarly(t *testing.T) {
	var versions []string
	for minor := 30; minor >= 0; minor-- {
		versions = append(versions, fmt.Sprintf("1.%d.0", minor))
	}
	f := tedgeReleases(t, versions...)
	r := cloudsmithResolver(t, f)

	files, err := r.Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"deb"}, Version: "latest-3",
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"tedge_1.28.0_amd64.deb", "tedge_1.28.0_arm64.deb",
		"tedge_1.29.0_amd64.deb", "tedge_1.29.0_arm64.deb",
		"tedge_1.30.0_amd64.deb", "tedge_1.30.0_arm64.deb",
	}, filenames(files))
	assert.Equal(t, 1, f.pages, "62 packages over one page of 100: no further page needed")
}

func TestCloudsmithPagesUntilVersionsComplete(t *testing.T) {
	var versions []string
	for minor := 120; minor >= 0; minor-- {
		versions = append(versions, fmt.Sprintf("1.%d.0", minor))
	}
	f := tedgeReleases(t, versions...)
	files, err := cloudsmithResolver(t, f).Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"deb"}, Version: "latest-60",
	}})
	require.NoError(t, err)
	assert.Len(t, files, 120, "two architectures of 60 versions")
	assert.Equal(t, 2, f.pages, "the second page completes the versions, the third is not read")
}

func TestCloudsmithAllExactAndGlob(t *testing.T) {
	f := tedgeReleases(t, "2.1.0", "2.0.1")
	r := cloudsmithResolver(t, f)

	files, err := r.Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"deb"}, Version: "all",
	}})
	require.NoError(t, err)
	assert.Len(t, files, 4)

	files, err = r.Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"deb"}, Version: "2.0.1",
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"tedge_2.0.1_amd64.deb", "tedge_2.0.1_arm64.deb"}, filenames(files))

	// A glob selects the latest version of each matching package
	files, err = r.Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"tedge*"}, Formats: []string{"deb"}, Architectures: []string{"arm64"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"tedge-agent_2.1.0_arm64.deb", "tedge_2.1.0_arm64.deb"}, filenames(files))
	assert.Contains(t, f.queries[len(f.queries)-1], "name:^tedge AND format:deb", "a glob is queried by its prefix")
}

func TestCloudsmithNameTerm(t *testing.T) {
	assert.Equal(t, "name:^tedge$", cloudsmithNameTerm("tedge"))
	assert.Equal(t, "name:^tedge-dot-", cloudsmithNameTerm("tedge-dot-*"))
	assert.Equal(t, "name:^tedge-", cloudsmithNameTerm("tedge-?gent"))
	assert.Equal(t, "", cloudsmithNameTerm("*"))
}

func TestCloudsmithAllFormatsByDefault(t *testing.T) {
	f := tedgeReleases(t, "2.1.0")
	files, err := cloudsmithResolver(t, f).Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "https://broadcasts.cloudsmith.com/thinedge/tedge-release", Packages: []string{"tedge"}, Architectures: []string{"arm64", "aarch64"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"tedge-2.1.0-1.aarch64.rpm", "tedge-2.1.0-r0.apk", "tedge_2.1.0_arm64.deb"}, filenames(files))
}

func TestCloudsmithNothingMatches(t *testing.T) {
	f := tedgeReleases(t, "2.1.0")
	_, err := cloudsmithResolver(t, f).Resolve(Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"missing"},
	}})
	require.ErrorIs(t, err, ErrNothingToDo)
	assert.ErrorContains(t, err, "no packages in thinedge/tedge-release matched missing (version: latest)")
}

func TestCloudsmithDownload(t *testing.T) {
	f := tedgeReleases(t, "2.1.0")
	linkOnly := false
	source := Source{Cloudsmith: &CloudsmithSource{
		Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"alpine"}, LinkOnly: &linkOnly,
	}}

	files, err := cloudsmithResolver(t, f).Resolve(source)
	require.NoError(t, err)
	require.Len(t, files, 2)
	assert.NotEqual(t, files[0].Path, files[1].Path, "packages with the same filename are downloaded to separate directories")
	for _, file := range files {
		assert.Empty(t, file.URL)
		content, err := os.ReadFile(file.Path)
		require.NoError(t, err)
		assert.Equal(t, "binary of tedge-2.1.0-r0.apk", string(content))
	}

	// A dry run does not download
	r := cloudsmithResolver(t, f)
	r.SkipDownload = true
	files, err = r.Resolve(source)
	require.NoError(t, err)
	assert.NoFileExists(t, files[0].Path)
}

func TestCloudsmithValidation(t *testing.T) {
	for _, tc := range []struct {
		source CloudsmithSource
		err    string
	}{
		{CloudsmithSource{Repo: "thinedge", Packages: []string{"tedge"}}, "cloudsmith.repo must be owner/repository"},
		{CloudsmithSource{Repo: "thinedge/tedge-release"}, "cloudsmith.packages requires at least one"},
		{CloudsmithSource{Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"npm"}}, `unsupported format "npm"`},
		{CloudsmithSource{Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Version: "latest-0"}, "latest-N requires a number of at least 1"},
		{CloudsmithSource{Repo: "thinedge/tedge-release", Packages: []string{"[tedge"}}, `invalid pattern "[tedge"`},
	} {
		source := Source{Cloudsmith: &tc.source}
		assert.ErrorContains(t, source.Validate(), tc.err)
	}

	source := Source{Cloudsmith: &CloudsmithSource{Repo: "https://broadcasts.cloudsmith.com/thinedge/community/", Packages: []string{"*"}}}
	require.NoError(t, source.Validate())
	owner, repo, _ := source.Cloudsmith.OwnerRepo()
	assert.Equal(t, "thinedge/community", owner+"/"+repo)

	both := Source{Path: "./packages", Cloudsmith: &CloudsmithSource{Repo: "a/b", Packages: []string{"x"}}}
	assert.ErrorContains(t, both.Validate(), "mutually exclusive")
}

func TestCloudsmithSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("software:\n  - source:\n      cloudsmith:\n        repo: thinedge/tedge-release\n        packages: [tedge]\n        formats: [deb, alpine]\n        version: latest-2\n")))
	assert.Error(t, validateYAML(t, schema, []byte("software:\n  - source:\n      cloudsmith:\n        repo: thinedge/tedge-release\n        packages: [tedge]\n        formats: [npm]\n")))
	assert.Error(t, validateYAML(t, schema, []byte("software:\n  - source:\n      cloudsmith:\n        repo: thinedge/tedge-release\n")))
}

// TestSyncSoftwareFromCloudsmith creates the software versions from the
// package metadata: the apk filenames have no architecture, but each
// version still gets the right one, and the type follows the format
func TestSyncSoftwareFromCloudsmith(t *testing.T) {
	cs := tedgeReleases(t, "2.1.0")
	f := fakeSoftwareRepository(t, nil)
	s := newFakeSyncer(t, f)
	s.Resolver = cloudsmithResolver(t, cs)

	require.NoError(t, s.SyncSoftware(context.Background(), []SoftwareSpec{{
		Source: Source{Cloudsmith: &CloudsmithSource{Repo: "thinedge/tedge-release", Packages: []string{"tedge"}, Formats: []string{"deb", "alpine"}}},
	}}))

	var items []string
	for _, r := range s.Results {
		require.NoError(t, r.Err, r.Item)
		items = append(items, r.Item)
	}
	sort.Strings(items)
	assert.Equal(t, []string{
		"tedge [aarch64/apk] 2.1.0-r0",
		"tedge [amd64/apt] 2.1.0",
		"tedge [arm64/apt] 2.1.0",
		"tedge [x86_64/apk] 2.1.0-r0",
	}, items)

	var urls []string
	for _, v := range f.Requests(http.MethodPost, "/inventory/managedObjects/500/childAdditions") {
		urls = append(urls, v.Body["c8y_Software"].(map[string]any)["url"].(string))
	}
	assert.Contains(t, urls, cs.server.URL+"/dl/tedge-2.1.0-r0.apk")
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"), "linked, not uploaded")
}
