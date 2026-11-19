package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/firmware/firmwareitems"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/firmware/firmwareversions"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/software/softwareitems"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/software/softwareversions"
)

// repositoryVersion is a firmware or software version in the tenant
type repositoryVersion struct {
	Version string
	URL     string
}

// VersionSelector selects a version of a firmware or software item by
// semver: "latest", "latest-N" (the Nth version before the latest, e.g.
// latest-1 is the second latest), or a semver constraint such as "^1.2" or
// ">=1.0, <2.0" (the highest matching version).
type VersionSelector struct {
	raw        string
	offset     int
	constraint *semver.Constraints
}

// ParseVersionSelector parses a versionSelector value
func ParseVersionSelector(value string) (VersionSelector, error) {
	raw := strings.TrimSpace(value)
	selector := VersionSelector{raw: raw}
	lower := strings.ToLower(raw)

	switch {
	case raw == "":
		return selector, fmt.Errorf("versionSelector is empty")
	case lower == "latest":
		return selector, nil
	case strings.HasPrefix(lower, "latest-"):
		offset, err := strconv.Atoi(strings.TrimPrefix(lower, "latest-"))
		if err != nil || offset < 1 {
			return selector, fmt.Errorf("invalid versionSelector %q: latest-N requires a positive number, e.g. latest-1 for the second latest version", raw)
		}
		selector.offset = offset
		return selector, nil
	}

	constraint, err := semver.NewConstraint(raw)
	if err != nil {
		return selector, fmt.Errorf("invalid versionSelector %q: expected latest, latest-N or a semver constraint such as ^1.2 or \">=1.0, <2.0\": %w", raw, err)
	}
	selector.constraint = constraint
	return selector, nil
}

// String returns the selector as written in the manifest
func (s VersionSelector) String() string {
	return s.raw
}

// matches reports whether a version passes the constraint of the selector.
// Prereleases are matched by the release they lead up to (1.5.0-rc1 is
// checked as 1.5.0), since semver constraints never match prereleases.
func (s VersionSelector) matches(version *semver.Version) bool {
	if s.constraint == nil {
		return true
	}
	if version.Prerelease() != "" {
		release, err := version.SetPrerelease("")
		if err != nil {
			return false
		}
		return s.constraint.Check(&release)
	}
	return s.constraint.Check(version)
}

// Select returns the version chosen by the selector. Versions which are not
// valid semver are ignored, and prereleases (e.g. 1.2.0-rc1) are only
// considered with includePrereleases. Versions which are equal in semver
// (e.g. 1.2 and 1.2.0) count once, keeping the first one.
func (s VersionSelector) Select(versions []repositoryVersion, includePrereleases bool) (repositoryVersion, error) {
	type candidate struct {
		version repositoryVersion
		semver  *semver.Version
	}
	var (
		candidates  []candidate
		invalid     int
		prereleases int
		unmatched   int
	)
	for _, item := range versions {
		parsed, err := semver.NewVersion(strings.TrimSpace(item.Version))
		if err != nil {
			invalid++
			continue
		}
		if parsed.Prerelease() != "" && !includePrereleases {
			prereleases++
			continue
		}
		if !s.matches(parsed) {
			unmatched++
			continue
		}
		duplicate := false
		for _, existing := range candidates {
			if existing.semver.Equal(parsed) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			candidates = append(candidates, candidate{version: item, semver: parsed})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].semver.GreaterThan(candidates[j].semver)
	})

	if s.offset < len(candidates) {
		return candidates[s.offset].version, nil
	}

	detail := fmt.Sprintf("%d version(s)", len(versions))
	if s.constraint != nil {
		detail += fmt.Sprintf(", %d not matching %s", unmatched, s.raw)
	}
	if invalid > 0 {
		detail += fmt.Sprintf(", %d not valid semver", invalid)
	}
	if prereleases > 0 {
		detail += fmt.Sprintf(", %d prerelease(s) excluded (set includePrereleases: true to use them)", prereleases)
	}
	if s.offset > 0 && len(candidates) > 0 {
		return repositoryVersion{}, fmt.Errorf("versionSelector %s needs at least %d versions, but only %d qualify (%s)", s.raw, s.offset+1, len(candidates), detail)
	}
	return repositoryVersion{}, fmt.Errorf("no version matches versionSelector %s (%s)", s.raw, detail)
}

// listFirmwareVersions returns all versions of a firmware item
func (s *Syncer) listFirmwareVersions(ctx context.Context, name string) ([]repositoryVersion, error) {
	firmware := s.Client.Repository.Firmware.Get(ctx, firmwareitems.NewRef().ByName(name), firmwareitems.GetOptions{})
	if firmware.Err != nil {
		return nil, fmt.Errorf("firmware item not found: %w", firmware.Err)
	}
	result := s.Client.Repository.Firmware.Versions.List(ctx, firmwareversions.ListOptions{
		FirmwareID:        firmware.Data.ID(),
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	var versions []repositoryVersion
	for item, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		versions = append(versions, repositoryVersion{
			Version: item.Get("c8y_Firmware.version").String(),
			URL:     item.Get("c8y_Firmware.url").String(),
		})
	}
	return versions, nil
}

// listSoftwareVersions returns all versions of a software item
func (s *Syncer) listSoftwareVersions(ctx context.Context, name, softwareType string) ([]repositoryVersion, error) {
	software := s.Client.Repository.Software.Get(ctx, softwareitems.NewRef().ByName(name, softwareType), softwareitems.GetOptions{})
	if software.Err != nil {
		return nil, fmt.Errorf("software item not found: %w", software.Err)
	}
	result := s.Client.Repository.Software.Versions.List(ctx, softwareversions.ListOptions{
		SoftwareID:        software.Data.ID(),
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	var versions []repositoryVersion
	for item, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		versions = append(versions, repositoryVersion{
			Version: item.Get("c8y_Software.version").String(),
			URL:     item.Get("c8y_Software.url").String(),
		})
	}
	return versions, nil
}

// resolveVersion resolves the version and url lookup of a firmware or
// software reference. An exact version is used as-is (it can be any string,
// including "latest") with the given lookup. A versionSelector lists all
// versions once and selects one of them.
func resolveVersion(version, selector string, includePrereleases bool, list func() ([]repositoryVersion, error), lookup func() (string, error)) (string, func() (string, error), error) {
	if selector == "" {
		return version, lookup, nil
	}
	parsed, err := ParseVersionSelector(selector)
	if err != nil {
		return "", nil, err
	}
	versions, err := list()
	if err != nil {
		return "", nil, err
	}
	selected, err := parsed.Select(versions, includePrereleases)
	if err != nil {
		return "", nil, err
	}
	return selected.Version, func() (string, error) { return selected.URL, nil }, nil
}

// versionLabel describes the requested version of a reference in messages
func versionLabel(version, selector string) string {
	if selector != "" {
		return "versionSelector " + selector
	}
	return version
}
