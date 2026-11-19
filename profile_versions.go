package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/firmware/firmwareitems"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/firmware/firmwareversions"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/software/softwareitems"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/software/softwareversions"
)

// VersionLatest selects the highest semver version of a firmware or software
// item in a device profile
const VersionLatest = "latest"

// isLatestVersion reports whether a profile reference asks for the latest version
func isLatestVersion(version string) bool {
	return strings.EqualFold(strings.TrimSpace(version), VersionLatest)
}

// repositoryVersion is a firmware or software version in the tenant
type repositoryVersion struct {
	Version string
	URL     string
}

// selectLatestVersion returns the version with the highest semver version.
// Versions which are not valid semver are ignored. Prereleases (versions with
// a "-" suffix, e.g. 1.2.0-rc1) are only considered with includePrereleases.
// When versions are equal (e.g. 1.2 and 1.2.0), the first one wins.
func selectLatestVersion(versions []repositoryVersion, includePrereleases bool) (repositoryVersion, error) {
	var (
		latest      repositoryVersion
		latestSem   *semver.Version
		skipped     int
		prereleases int
	)
	for _, candidate := range versions {
		parsed, err := semver.NewVersion(strings.TrimSpace(candidate.Version))
		if err != nil {
			skipped++
			continue
		}
		if parsed.Prerelease() != "" && !includePrereleases {
			prereleases++
			continue
		}
		if latestSem == nil || parsed.GreaterThan(latestSem) {
			latest, latestSem = candidate, parsed
		}
	}
	if latestSem == nil {
		detail := fmt.Sprintf("%d version(s)", len(versions))
		if skipped > 0 {
			detail += fmt.Sprintf(", %d not valid semver", skipped)
		}
		if prereleases > 0 {
			detail += fmt.Sprintf(", %d prerelease(s) excluded (set includePrereleases: true to use them)", prereleases)
		}
		return repositoryVersion{}, fmt.Errorf("no version found to use as latest (%s)", detail)
	}
	return latest, nil
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
// software reference. For "latest", all versions are listed once and the
// highest semver version is used; otherwise the version is used as-is with
// the given lookup.
func resolveVersion(version string, includePrereleases bool, list func() ([]repositoryVersion, error), lookup func() (string, error)) (string, func() (string, error), error) {
	if !isLatestVersion(version) {
		return version, lookup, nil
	}
	versions, err := list()
	if err != nil {
		return "", nil, err
	}
	latest, err := selectLatestVersion(versions, includePrereleases)
	if err != nil {
		return "", nil, err
	}
	return latest.Version, func() (string, error) { return latest.URL, nil }, nil
}
