package main

import "strings"

// existingFound reports whether a get-or-create result returned an existing
// resource rather than creating one
func existingFound(meta map[string]any) bool {
	found, ok := meta["found"].(bool)
	return ok && found
}

// isTenantBinaryURL reports whether the url points to a binary stored in
// Cumulocity (inventory binaries API) rather than to an external location
func isTenantBinaryURL(url string) bool {
	return strings.Contains(url, "/inventory/binaries/")
}

// binarySourceChanged reports whether an existing item no longer matches where
// the manifest says its binary comes from, so get-or-create must not leave it
// as is. This catches a switch between linking and uploading (e.g. changing
// github.linkOnly) and a changed link url. An already uploaded binary is not
// compared by content: replacing it still requires --force.
//
//   - desiredURL set (link): changed when the existing url differs
//   - uploading a file: changed when the existing url is not a Cumulocity binary
//   - neither (explicit entry without url): never changed
func binarySourceChanged(existingURL, desiredURL string, uploading bool) bool {
	if desiredURL != "" {
		return existingURL != desiredURL
	}
	if uploading {
		return !isTenantBinaryURL(existingURL)
	}
	return false
}
