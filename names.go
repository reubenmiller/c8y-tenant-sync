package main

import (
	"fmt"
	"slices"
	"strings"
)

// sameName reports whether two names are equal, ignoring case and
// surrounding spaces (extension names are matched like this)
func sameName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// editDistance is the Levenshtein distance between two strings (by rune,
// ignoring case)
func editDistance(a, b string) int {
	ra := []rune(strings.ToLower(a))
	rb := []rune(strings.ToLower(b))
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// suggestNames returns up to limit candidates which are close to the name:
// a small edit distance, or one containing the other. The closest come first.
func suggestNames(name string, candidates []string, limit int) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	lower := strings.ToLower(name)
	maxDistance := max(2, len([]rune(name))/4)

	type match struct {
		name     string
		distance int
	}
	var matches []match
	seen := map[string]bool{}
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		distance := editDistance(name, candidate)
		contains := len(lower) >= 4 && len(key) >= 4 && (strings.Contains(key, lower) || strings.Contains(lower, key))
		if distance <= maxDistance || contains {
			matches = append(matches, match{candidate, distance})
		}
	}
	slices.SortStableFunc(matches, func(a, b match) int { return a.distance - b.distance })
	var out []string
	for _, m := range matches {
		if len(out) == limit {
			break
		}
		out = append(out, m.name)
	}
	return out
}

// didYouMean formats suggestions as `; did you mean "a" or "b"?` (empty
// without suggestions)
func didYouMean(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	quoted := make([]string, len(suggestions))
	for i, s := range suggestions {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "; did you mean " + strings.Join(quoted, " or ") + "?"
}

// ExtensionReferenceWarnings checks the extensions installed into web
// applications against the extensions the manifest uploads. A reference which
// matches no declared extension (by name, or by the context path or name read
// from the archive, mapped context path -> name, e.g. by validate
// --check-sources) is not an error, since the extension may
// already be on the tenant, but it is a likely typo, so it is reported with
// the closest declared names.
func (m *Manifest) ExtensionReferenceWarnings(contextPaths map[string]string) []string {
	var candidates []string
	for _, ext := range m.Extensions {
		candidates = append(candidates, ext.Name)
	}
	for contextPath, name := range contextPaths {
		candidates = append(candidates, contextPath, name)
	}
	slices.Sort(candidates)

	var warnings []string
	for i, app := range m.Applications {
		for j, ref := range app.Extensions {
			if !ref.IsInstalled() || strings.TrimSpace(ref.Name) == "" {
				continue // removing an extension which is not uploaded is normal
			}
			if slices.ContainsFunc(candidates, func(c string) bool { return sameName(c, ref.Name) }) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"applications[%d].extensions[%d]: %q is not uploaded by the extensions section, so it must already be on the tenant (matched by name or context path)%s",
				i, j, ref.Name, didYouMean(suggestNames(ref.Name, candidates, 3))))
		}
	}
	return warnings
}
