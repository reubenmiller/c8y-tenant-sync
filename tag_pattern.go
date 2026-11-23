package main

import (
	"fmt"
	"regexp"
	"strings"
)

// componentPlaceholder in a github.tags pattern matches the component of a
// release tag, e.g. "{component}-v*" matches c8y-deploy-poll-v0.6.1 with the
// component c8y-deploy-poll. Releases are then selected per component.
const componentPlaceholder = "{component}"

// tagPattern is a compiled github.tags glob pattern
type tagPattern struct {
	pattern string
	re      *regexp.Regexp
	// component is the index of the {component} group (0: none)
	component int
	// firstWildcard is the index of the first glob wildcard group (0: none)
	firstWildcard int
}

// compileTagPattern translates a glob pattern (*, ?, [...]) with an optional
// {component} placeholder into a regular expression. The component matches
// as much as possible, so "{component}-v*" splits a tag at its last "-v".
func compileTagPattern(pattern string) (*tagPattern, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	if strings.Count(pattern, componentPlaceholder) > 1 {
		return nil, fmt.Errorf("%s can be used once", componentPlaceholder)
	}
	compiled := &tagPattern{pattern: pattern}
	var b strings.Builder
	b.WriteString("^")
	group := 0
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], componentPlaceholder):
			group++
			compiled.component = group
			b.WriteString("(.+)")
			i += len(componentPlaceholder)
			continue
		case pattern[i] == '*' || pattern[i] == '?' || pattern[i] == '[':
			group++
			if compiled.firstWildcard == 0 {
				compiled.firstWildcard = group
			}
			switch pattern[i] {
			case '*':
				b.WriteString("(.*)")
			case '?':
				b.WriteString("(.)")
			default:
				end := strings.IndexByte(pattern[i+1:], ']')
				if end < 0 {
					return nil, fmt.Errorf("unterminated [")
				}
				class := pattern[i+1 : i+1+end]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				b.WriteString("([" + class + "])")
				i += end + 2
				continue
			}
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
		i++
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	compiled.re = re
	return compiled, nil
}

// Match reports whether the tag matches, with the component (when the
// pattern has the placeholder) and the version part of the tag: the tag from
// the first glob wildcard on (e.g. "v0.6.1" for "{component}-*"), or the
// whole tag when the pattern has no wildcard
func (p *tagPattern) Match(tag string) (component, version string, ok bool) {
	match := p.re.FindStringSubmatchIndex(tag)
	if match == nil {
		return "", "", false
	}
	if p.component > 0 {
		component = tag[match[2*p.component]:match[2*p.component+1]]
	}
	version = tag
	if p.firstWildcard > 0 {
		version = tag[match[2*p.firstWildcard]:]
	}
	return component, version, true
}

func compileTagPatterns(patterns []string) ([]*tagPattern, error) {
	compiled := make([]*tagPattern, 0, len(patterns))
	for _, pattern := range patterns {
		p, err := compileTagPattern(pattern)
		if err != nil {
			return nil, fmt.Errorf("github.tags: invalid pattern %q: %w", pattern, err)
		}
		compiled = append(compiled, p)
	}
	return compiled, nil
}
