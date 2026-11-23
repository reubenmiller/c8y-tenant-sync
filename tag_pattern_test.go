package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTagPatternMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, tag, component, version string
		ok                               bool
	}{
		{"{component}-v*", "c8y-deploy-poll-v0.6.1", "c8y-deploy-poll", "0.6.1", true},
		{"{component}-v*", "x509-cert-issuer-v1.0.0", "x509-cert-issuer", "1.0.0", true},
		{"{component}-v*", "v1.0.0", "", "", false},
		{"c8y-{component}-v*", "c8y-deploy-poll-v0.6.1", "deploy-poll", "0.6.1", true},
		{"c8y-{component}-v*", "log-surge-v2.0.1", "", "", false},
		{"c8y-deploy-poll-v*", "c8y-deploy-poll-v0.6.1", "", "0.6.1", true},
		{"c8y-deploy-poll-v*", "c8y-deploy-status-v0.3.1", "", "", false},
		{"v[0-9]*", "v1.2.3", "", "1.2.3", true},
		{"release-?", "release-7", "", "7", true},
		{"stable", "stable", "", "stable", true},
		{"*", "anything", "", "anything", true},
		{"a.b-v*", "axb-v1", "", "", false}, // dots are literal
	} {
		p, err := compileTagPattern(tc.pattern)
		require.NoError(t, err, tc.pattern)
		component, version, ok := p.Match(tc.tag)
		assert.Equal(t, tc.ok, ok, "%s ~ %s", tc.pattern, tc.tag)
		if tc.ok {
			assert.Equal(t, tc.component, component, "%s ~ %s", tc.pattern, tc.tag)
			assert.Equal(t, tc.version, version, "%s ~ %s", tc.pattern, tc.tag)
		}
	}

	for _, invalid := range []string{"", "{component}-{component}", "comp-[v"} {
		_, err := compileTagPattern(invalid)
		assert.Error(t, err, invalid)
	}
}

func TestGitHubTagsPerComponent(t *testing.T) {
	f := newFakeMonorepo(t,
		"c8y-deploy-poll-v0.6.1",
		"c8y-deploy-poll-v0.6.0",
		"c8y-deploy-status-v0.3.1",
		"c8y-deploy-operation-v0.3.0",
		"c8y-deploy-status-v0.3.0",
		"c8y-deploy-poll-v0.5.0",
	)
	r := monorepoResolver(t, f)

	files, err := r.Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"{component}-v*"}, LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"c8y.2Fc8y-deploy-operation_0.3.0.tar.gz 0.3.0",
		"c8y.2Fc8y-deploy-poll_0.6.1.tar.gz 0.6.1",
		"c8y.2Fc8y-deploy-status_0.3.1.tar.gz 0.3.1",
	}, resolvedTags(t, files), "the latest release of every component")

	files, err = r.Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"{component}-v*"}, Release: "latest-2", LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"c8y.2Fc8y-deploy-operation_0.3.0.tar.gz 0.3.0",
		"c8y.2Fc8y-deploy-poll_0.6.0.tar.gz 0.6.0",
		"c8y.2Fc8y-deploy-poll_0.6.1.tar.gz 0.6.1",
		"c8y.2Fc8y-deploy-status_0.3.0.tar.gz 0.3.0",
		"c8y.2Fc8y-deploy-status_0.3.1.tar.gz 0.3.1",
	}, resolvedTags(t, files), "the latest two of every component")
}

func TestGitHubTagsPerComponentReadsAllPages(t *testing.T) {
	f := newFakeMonorepo(t)
	for i := 0; i < 150; i++ {
		f.add(fmt.Sprintf("c8y-deploy-poll-v0.%d.0", 200-i), false)
	}
	f.add("c8y-deploy-status-v0.3.1", false) // only on the second page
	files, err := monorepoResolver(t, f).Resolve(Source{GitHub: &GitHubSource{
		Repo: "thin-edge/tedge-flows-examples", Tags: []string{"{component}-v*"}, LinkOnly: boolPtr(true),
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"c8y.2Fc8y-deploy-poll_0.200.0.tar.gz 0.200.0",
		"c8y.2Fc8y-deploy-status_0.3.1.tar.gz 0.3.1",
	}, resolvedTags(t, files))
	assert.Equal(t, 2, f.pages, "a new component can be on any page")
}

func TestGitHubTagsComponentValidation(t *testing.T) {
	source := Source{GitHub: &GitHubSource{Repo: "a/b", Tags: []string{"{component}-{component}"}}}
	assert.ErrorContains(t, source.Validate(), `github.tags: invalid pattern "{component}-{component}": {component} can be used once`)
	source = Source{GitHub: &GitHubSource{Repo: "a/b", Tags: []string{"{component}-v*"}, Release: "latest-3"}}
	assert.NoError(t, source.Validate())
}
