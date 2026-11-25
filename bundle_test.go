package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarGz builds a .tar.gz archive from files (name -> content)
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(files[name]))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// blueprintFiles is a manifest which uses an included manifest, local
// source files and a hook script
var blueprintFiles = map[string]string{
	"prod.yaml": `
include:
  - base/common.yaml
software:
  - source: {path: packages}
hooks:
  pre:
    - run: ./scripts/check.sh
`,
	"base/common.yaml": `
trustedCertificates:
  - source: {path: ../certs/ca.pem}
`,
	"packages/app_1.0.0_all.deb": "deb",
	"certs/ca.pem":               "pem",
	"scripts/check.sh":           "#!/bin/sh\necho ok\n",
	"notes.txt":                  "not used by the manifest",
	"prod.secrets.yaml":          "secret: value\n",
}

// listArchive returns the names of the files in a bundle
func listArchive(t *testing.T, archive string) []string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, extractBundle(archive, dir))
	var names []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			names = append(names, filepath.ToSlash(rel))
		}
		return err
	}))
	sort.Strings(names)
	return names
}

func TestBundleCommandPackagesManifestFiles(t *testing.T) {
	for _, ext := range []string{".tar.gz", ".zip"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, blueprintFiles)
			require.NoError(t, os.Chmod(filepath.Join(dir, "scripts/check.sh"), 0o755))
			output := filepath.Join(t.TempDir(), "blueprint"+ext)

			_, _ = captureStdio(t, func() {
				require.NoError(t, runBundle(filepath.Join(dir, "prod.yaml"), output, []string{filepath.Join(dir, "scripts")}))
			})
			assert.Equal(t, []string{
				BundleMarkerFile,
				"base/common.yaml",
				"certs/ca.pem",
				"packages/app_1.0.0_all.deb",
				"prod.yaml",
				"scripts/check.sh",
			}, listArchive(t, output))

			// The same files give the same archive
			again := filepath.Join(t.TempDir(), "again"+ext)
			_, _ = captureStdio(t, func() {
				require.NoError(t, runBundle(filepath.Join(dir, "prod.yaml"), again, []string{filepath.Join(dir, "scripts")}))
			})
			first, _ := fileSHA256(output)
			second, _ := fileSHA256(again)
			assert.Equal(t, first, second)

			// Executable scripts stay executable
			extracted := t.TempDir()
			require.NoError(t, extractBundle(output, extracted))
			info, err := os.Stat(filepath.Join(extracted, "scripts/check.sh"))
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&0o111)
		})
	}
}

func TestBundleCommandRejectsFilesOutsideManifestDir(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"shared/ca.pem": "pem",
		"app/tenant.yaml": `
trustedCertificates:
  - source: {path: ../shared/ca.pem}
`,
	})
	err := runBundle(filepath.Join(dir, "app/tenant.yaml"), filepath.Join(t.TempDir(), "b.tar.gz"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside")
	assert.Contains(t, err.Error(), "ca.pem")
}

// bundleArchive packages blueprintFiles with the bundle command
func bundleArchive(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, blueprintFiles)
	output := filepath.Join(t.TempDir(), "blueprint.tar.gz")
	_, _ = captureStdio(t, func() {
		require.NoError(t, runBundle(filepath.Join(dir, "prod.yaml"), output, []string{filepath.Join(dir, "scripts")}))
	})
	raw, err := os.ReadFile(output)
	require.NoError(t, err)
	return raw
}

func TestIncludeLocalBundle(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor/blueprint.tar.gz"), bundleArchive(t), 0o644))
	writeFiles(t, dir, map[string]string{"tenant.yaml": "include:\n  - vendor/blueprint.tar.gz\n"})

	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)

	assert.Equal(t, []string{
		"vendor/blueprint.tar.gz!prod.yaml",
		"vendor/blueprint.tar.gz!base/common.yaml",
	}, manifest.IncludedFiles)

	// Source paths point into the extracted bundle, hooks run there
	require.Len(t, manifest.Software, 1)
	require.Len(t, manifest.TrustedCertificates, 1)
	resolver := NewSourceResolver(dir, "", true)
	files, err := resolver.Resolve(manifest.Software[0].Source)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "app_1.0.0_all.deb", files[0].Filename)
	files, err = resolver.Resolve(manifest.TrustedCertificates[0].Source)
	require.NoError(t, err)
	content, err := os.ReadFile(files[0].Path)
	require.NoError(t, err)
	assert.Equal(t, "pem", string(content))
	require.Len(t, manifest.Hooks.Pre, 1)
	assert.FileExists(t, filepath.Join(manifest.Hooks.Pre[0].Dir, "scripts/check.sh"))

	// Cleanup removes the extracted files
	manifest.Cleanup()
	assert.NoFileExists(t, files[0].Path)
}

// fakeBundleRelease serves a GitHub release with a bundle asset and a
// release notes asset
type fakeBundleRelease struct {
	server    *httptest.Server
	bundle    []byte
	digest    string
	downloads int
}

func newFakeBundleRelease(t *testing.T, bundle []byte) *fakeBundleRelease {
	t.Helper()
	sum := sha256.Sum256(bundle)
	f := &fakeBundleRelease{bundle: bundle, digest: "sha256:" + hex.EncodeToString(sum[:])}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/blueprints/releases/latest", "/repos/acme/blueprints/releases/tags/v1.0.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.0.0",
				"assets": []map[string]any{
					{"name": "blueprint-1.0.0.tar.gz", "url": f.server.URL + "/asset/bundle", "size": len(f.bundle), "digest": f.digest},
					{"name": "NOTES.md", "url": f.server.URL + "/asset/notes"},
				},
			})
		case "/asset/bundle":
			f.downloads++
			_, _ = w.Write(f.bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBundleRelease) options() LoadOptions {
	return LoadOptions{GitHubAPI: f.server.URL, HTTPClient: f.server.Client()}
}

func TestIncludeGitHubBundle(t *testing.T) {
	release := newFakeBundleRelease(t, bundleArchive(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"tenant.yaml": `
include:
  - github:
      repo: acme/blueprints
  - github:
      repo: acme/blueprints
      release: v1.0.0
`})

	// The bundle has a hook: a remote bundle needs --allow-remote-commands
	_, err := LoadManifestWith(filepath.Join(dir, "tenant.yaml"), release.options())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--allow-remote-commands")

	release.downloads = 0
	opts := release.options()
	opts.AllowRemoteCommands = true
	opts.WorkDir = t.TempDir()
	manifest, err := LoadManifestWith(filepath.Join(dir, "tenant.yaml"), opts)
	require.NoError(t, err)
	defer manifest.Cleanup()

	// Both entries name the same bundle: downloaded and merged once
	assert.Equal(t, []string{
		"acme/blueprints@v1.0.0/blueprint-1.0.0.tar.gz!prod.yaml",
		"acme/blueprints@v1.0.0/blueprint-1.0.0.tar.gz!base/common.yaml",
	}, manifest.IncludedFiles)
	assert.Equal(t, 1, release.downloads)
	require.Len(t, manifest.Software, 1)
	files, err := NewSourceResolver(dir, "", true).Resolve(manifest.Software[0].Source)
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestIncludeGitHubBundleChecksums(t *testing.T) {
	bundle := tarGz(t, map[string]string{"tenant.yaml": "features:\n  - key: f\n"})
	dir := t.TempDir()

	t.Run("digest mismatch", func(t *testing.T) {
		release := newFakeBundleRelease(t, bundle)
		release.digest = "sha256:" + hex.EncodeToString(make([]byte, 32))
		writeFiles(t, dir, map[string]string{"tenant.yaml": "include:\n  - github: {repo: acme/blueprints}\n"})
		_, err := LoadManifestWith(filepath.Join(dir, "tenant.yaml"), release.options())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "checksum mismatch")
	})

	t.Run("sha256 pin", func(t *testing.T) {
		release := newFakeBundleRelease(t, bundle)
		writeFiles(t, dir, map[string]string{"tenant.yaml": "include:\n  - github: {repo: acme/blueprints}\n    sha256: " + release.digest + "\n"})
		manifest, err := LoadManifestWith(filepath.Join(dir, "tenant.yaml"), release.options())
		require.NoError(t, err)
		manifest.Cleanup()

		writeFiles(t, dir, map[string]string{"tenant.yaml": "include:\n  - github: {repo: acme/blueprints}\n    sha256: " + hex.EncodeToString(make([]byte, 32)) + "\n"})
		_, err = LoadManifestWith(filepath.Join(dir, "tenant.yaml"), release.options())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "checksum mismatch")
	})
}

func TestIncludeGitHubBundleErrors(t *testing.T) {
	release := newFakeBundleRelease(t, tarGz(t, map[string]string{"tenant.yaml": ""}))
	cases := map[string]string{
		"github: {repo: acme/blueprints, assets: ['*']}":                       "a bundle needs exactly one asset",
		"github: {repo: acme/blueprints, assets: ['*.md']}":                    "is not a bundle archive",
		"github: {repo: acme/blueprints, assets: ['*.whl']}":                   "no asset matches",
		"github: {repo: acme/blueprints, release: all}":                        "selects several releases",
		"github: {repo: acme}":                                                 "must have the form owner/name",
		"github: {repo: acme/blueprints, unknown: true}":                       "field unknown not found",
		"{path: base.yaml, github: {repo: acme/blueprints}}":                   "mutually exclusive",
		"{path: base.yaml, manifest: x.yaml}":                                  "only supported for bundles",
		"{github: {repo: acme/blueprints}, manifest: missing.yaml}":            "manifest missing.yaml not found in the bundle",
		"{github: {repo: acme/blueprints, assets: ['*.whl']}, optional: true}": "",
	}
	for entry, want := range cases {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"tenant.yaml": "include:\n  - " + entry + "\n"})
			manifest, err := LoadManifestWith(filepath.Join(dir, "tenant.yaml"), release.options())
			if want == "" {
				require.NoError(t, err)
				assert.Empty(t, manifest.IncludedFiles)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), want)
		})
	}
}

func TestRunURLBundle(t *testing.T) {
	server := newManifestServer(t, map[string]string{
		"/releases/blueprint.tar.gz": string(tarGz(t, map[string]string{
			// Wrapped in a single directory, like a GitHub source archive
			"blueprint-1.0/tenant.yaml":        "include:\n  - base.yaml\ntrustedCertificates:\n  - source: {path: ca.pem}\n",
			"blueprint-1.0/base.yaml":          "features:\n  - key: f\n",
			"blueprint-1.0/ca.pem":             "pem",
			"blueprint-1.0/escape.yaml":        "trustedCertificates:\n  - source: {path: ../../ca.pem}\n",
			"blueprint-1.0/include-local.yaml": "include:\n  - ../../outside.yaml\n",
		})),
	})
	location := server.URL + "/releases/blueprint.tar.gz"

	manifest, err := LoadManifest(location)
	require.NoError(t, err)
	defer manifest.Cleanup()
	assert.Equal(t, []string{location + "!blueprint-1.0/base.yaml"}, manifest.IncludedFiles)
	require.Len(t, manifest.TrustedCertificates, 1)
	files, err := NewSourceResolver(manifestBaseDir(location), "", true).Resolve(manifest.TrustedCertificates[0].Source)
	require.NoError(t, err)
	assert.Equal(t, "ca.pem", files[0].Filename)

	// Local paths of a remote bundle must stay in the bundle
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"escape.yaml":  "include:\n  - {path: " + location + ", manifest: escape.yaml}\n",
		"include.yaml": "include:\n  - {path: " + location + ", manifest: include-local.yaml}\n",
	})
	_, err = LoadManifest(filepath.Join(dir, "escape.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the bundle")
	_, err = LoadManifest(filepath.Join(dir, "include.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only include paths inside the bundle")
}

func TestExtractBundleRejectsUnsafeEntries(t *testing.T) {
	for _, name := range []string{"../evil.yaml", "a/../../evil.yaml", "/etc/evil.yaml"} {
		t.Run(name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "bad.tar.gz")
			require.NoError(t, os.WriteFile(archive, tarGz(t, map[string]string{name: "x"}), 0o644))
			dest := t.TempDir()
			err := extractBundle(archive, dest)
			require.Error(t, err)
			assert.NoFileExists(t, filepath.Join(filepath.Dir(dest), "evil.yaml"))
		})
	}
}

func TestFindBundleEntry(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.yaml": "", "b.yaml": ""})
	_, err := findBundleEntry(dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "several manifests (a.yaml, b.yaml)")

	writeFiles(t, dir, map[string]string{BundleMarkerFile: "manifest: b.yaml\n"})
	entry, err := findBundleEntry(dir, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "b.yaml"), entry)

	entry, err = findBundleEntry(dir, "a.yaml")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a.yaml"), entry)

	single := t.TempDir()
	writeFiles(t, single, map[string]string{"only.yml": ""})
	entry, err = findBundleEntry(single, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(single, "only.yml"), entry)
}
