// Copyright © 2026 Nik Ogura <nik.ogura@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package discovery finds the Crossplane-owned custom resource definitions in
// a cluster and keeps a live registry of them, so new providers are picked up
// without a configuration change or a restart.
package discovery

import (
	"path"
	"strings"
)

// Matcher decides whether a CustomResourceDefinition describes a
// Crossplane-owned kind.
//
// Two signals are combined, because neither is sufficient on its own:
//
//   - Category. Every core Crossplane CRD and every provider-installed managed
//     resource carries the category "crossplane", which makes it the robust
//     primary signal and the reason no per-kind enumeration is needed.
//   - API group glob. A handful of kinds ship with no categories at all —
//     ProviderConfig being the one operators notice — so a group glob picks up
//     what the category misses.
//
// A CRD matching either signal is collected, unless an exclusion drops it.
type Matcher struct {
	categories    map[string]struct{}
	groups        []string
	excludeKinds  map[string]struct{}
	excludeGroups []string
}

// NewMatcher builds a Matcher from configured category names and group globs.
// Kind exclusions are matched case-insensitively; group exclusions are globs.
func NewMatcher(categories []string, groups []string, excludeKinds []string, excludeGroups []string) (matcher Matcher) {
	matcher = Matcher{
		categories:    toSet(categories),
		groups:        groups,
		excludeKinds:  toSet(excludeKinds),
		excludeGroups: excludeGroups,
	}

	return matcher
}

// Matches reports whether a CRD with the supplied API group, kind and declared
// categories should be collected.
func (m Matcher) Matches(group string, kind string, categories []string) (matched bool) {
	if m.excluded(group, kind) {
		return matched
	}

	for _, category := range categories {
		_, wanted := m.categories[strings.ToLower(category)]
		if wanted {
			matched = true
			return matched
		}
	}

	matched = matchesAnyGlob(group, m.groups)

	return matched
}

// excluded reports whether an exclusion rule drops this CRD.
func (m Matcher) excluded(group string, kind string) (drop bool) {
	_, kindExcluded := m.excludeKinds[strings.ToLower(kind)]
	if kindExcluded {
		drop = true
		return drop
	}

	drop = matchesAnyGlob(group, m.excludeGroups)

	return drop
}

// matchesAnyGlob reports whether the value matches any of the shell-style
// patterns. A malformed pattern never matches rather than aborting discovery:
// a typo in one glob must not blind the exporter to every other kind.
func matchesAnyGlob(value string, patterns []string) (matched bool) {
	for _, pattern := range patterns {
		ok, err := path.Match(pattern, value)
		if err == nil && ok {
			matched = true
			return matched
		}
	}

	return matched
}

// toSet builds a lower-cased lookup set from a list.
func toSet(values []string) (set map[string]struct{}) {
	set = make(map[string]struct{}, len(values))

	for _, value := range values {
		set[strings.ToLower(value)] = struct{}{}
	}

	return set
}
