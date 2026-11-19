package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGitHub serves a single release with one asset and redirects all
// requests of the resolver's http client to it
func fakeGitHub(t *testing.T) (client *http.Client, assetURL string, downloads *int) {
	t.Helper()
	count := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/os/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.2.3",
				"assets": []map[string]any{{
					"name":                 "image-rpi4.img.xz",
					"browser_download_url": "https://github.com/acme/os/releases/download/v1.2.3/image-rpi4.img.xz",
					"url":                  server.URL + "/asset",
				}},
			})
		default:
			count++
			_, _ = w.Write([]byte("image"))
		}
	}))
	t.Cleanup(server.Close)

	target, _ := url.Parse(server.URL)
	client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = target.Scheme
		clone.URL.Host = target.Host
		clone.Host = target.Host
		return http.DefaultTransport.RoundTrip(clone)
	})}
	return client, "https://github.com/acme/os/releases/download/v1.2.3/image-rpi4.img.xz", &count
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func boolPtr(v bool) *bool { return &v }

func TestFirmwareGitHubLinksByDefault(t *testing.T) {
	client, assetURL, downloads := fakeGitHub(t)
	resolver := NewSourceResolver(t.TempDir(), t.TempDir(), false)
	resolver.HTTPClient = client

	spec := FirmwareSpec{Source: Source{GitHub: &GitHubSource{Repo: "acme/os"}}}
	files, err := resolver.Resolve(spec.resolvedSource())
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, assetURL, files[0].URL)
	assert.Empty(t, files[0].Path)
	assert.Equal(t, "1.2.3", files[0].VersionHint)
	assert.Equal(t, 0, *downloads, "linked assets must not be downloaded")

	// The manifest is not changed by applying the default
	assert.Nil(t, spec.Source.GitHub.LinkOnly)
}

func TestFirmwareGitHubUploadWhenLinkOnlyFalse(t *testing.T) {
	client, _, downloads := fakeGitHub(t)
	resolver := NewSourceResolver(t.TempDir(), t.TempDir(), false)
	resolver.HTTPClient = client

	spec := FirmwareSpec{Source: Source{GitHub: &GitHubSource{Repo: "acme/os", LinkOnly: boolPtr(false)}}}
	files, err := resolver.Resolve(spec.resolvedSource())
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Empty(t, files[0].URL)
	require.NotEmpty(t, files[0].Path)
	content, err := os.ReadFile(files[0].Path)
	require.NoError(t, err)
	assert.Equal(t, "image", string(content))
	assert.Equal(t, 1, *downloads)
}

func TestLinkOnlyDefaults(t *testing.T) {
	gh := Source{GitHub: &GitHubSource{Repo: "acme/os"}}

	// Software and configuration keep uploading by default
	assert.False(t, gh.GitHub.IsLinkOnly())
	assert.True(t, gh.withLinkOnlyDefault(true).GitHub.IsLinkOnly())

	// An explicit value always wins
	explicit := Source{GitHub: &GitHubSource{Repo: "acme/os", LinkOnly: boolPtr(false)}}
	assert.False(t, explicit.withLinkOnlyDefault(true).GitHub.IsLinkOnly())

	// Non-GitHub sources are unchanged
	local := Source{Path: "./firmware"}
	assert.Equal(t, local, local.withLinkOnlyDefault(true))
}
