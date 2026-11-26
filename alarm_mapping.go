package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/tenantoptions"
	"gopkg.in/yaml.v3"
)

// AlarmMappingCategory is the tenant option category which holds the alarm
// mapping: one option per alarm type with the value "SEVERITY|description"
const AlarmMappingCategory = "alarm.type.mapping"

// AlarmMappingSeverities are the severities an alarm type can be mapped to.
// NONE drops the alarms of the type.
var AlarmMappingSeverities = []string{"CRITICAL", "MAJOR", "MINOR", "WARNING", "NONE"}

// Alarm mapping states
const (
	AlarmMappingStatePresent = "present"
	AlarmMappingStateAbsent  = "absent"
)

// AlarmMappingStates are the supported alarm mapping states
var AlarmMappingStates = []string{AlarmMappingStatePresent, AlarmMappingStateAbsent}

// Alarm mapping sync modes
const (
	AlarmMappingModeAdditive = "additive"
	AlarmMappingModeExact    = "exact"
)

// AlarmMappingModes are the supported alarm mapping sync modes
var AlarmMappingModes = []string{AlarmMappingModeAdditive, AlarmMappingModeExact}

// AlarmMappingSectionSpec is the alarmMapping section: the mappings and how
// they are synced. In the manifest it is either an object with mode and
// mappings, or just the list of mappings (additive).
type AlarmMappingSectionSpec struct {
	Mode     string             `yaml:"mode" json:"mode,omitempty" jsonschema:"description=additive (default) syncs only the listed mappings (state absent deletes one); exact also deletes the mappings of alarm types which are not listed so the list is the complete alarm mapping of the tenant (mappings added in the UI are deleted too),enum=additive,enum=exact"`
	Mappings []AlarmMappingSpec `yaml:"mappings" json:"mappings,omitempty" jsonschema:"description=Mappings by alarm type"`
}

// Exact reports whether the mappings which are not listed are deleted
func (s *AlarmMappingSectionSpec) Exact() bool {
	return s != nil && strings.TrimSpace(s.Mode) == AlarmMappingModeExact
}

// UnmarshalYAML accepts the list of mappings (additive) or an object with
// mode and mappings
func (s *AlarmMappingSectionSpec) UnmarshalYAML(node *yaml.Node) error {
	// Decode strictly (unknown fields are errors), like the rest of the
	// manifest
	decode := func(target any) error {
		decoder := yaml.NewDecoder(strings.NewReader(nodeYAML(node)))
		decoder.KnownFields(true)
		if err := decoder.Decode(target); err != nil {
			return fmt.Errorf("line %d: alarmMapping: %w", node.Line, err)
		}
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		*s = AlarmMappingSectionSpec{}
		return decode(&s.Mappings)
	case yaml.MappingNode:
		type plain AlarmMappingSectionSpec
		var value plain
		if err := decode(&value); err != nil {
			return err
		}
		*s = AlarmMappingSectionSpec(value)
		return nil
	default:
		return fmt.Errorf("line %d: alarmMapping must be a list of mappings or an object with mode and mappings", node.Line)
	}
}

// MarshalJSON writes the list of mappings unless the mode is exact, so
// additive manifests (such as exports) keep the short form
func (s AlarmMappingSectionSpec) MarshalJSON() ([]byte, error) {
	if !s.Exact() {
		return json.Marshal(s.Mappings)
	}
	type plain AlarmMappingSectionSpec
	return json.Marshal(plain(s))
}

// JSONSchemaExtend accepts the list of mappings or the object
func (AlarmMappingSectionSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	mappings, _ := schema.Properties.Get("mappings")
	object := *schema
	object.Description = "The mappings and how they are synced"
	list := *mappings
	list.Description = "The mappings, synced additively (the same as mode additive)"
	*schema = jsonschema.Schema{OneOf: []*jsonschema.Schema{&list, &object}}
}

// AlarmSeverity is the severity of an alarm mapping. It is case-insensitive
// in the manifest and converted to upper case when it is read.
type AlarmSeverity string

// UnmarshalYAML converts the severity to upper case
func (s *AlarmSeverity) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: severity must be one of %s", node.Line, strings.Join(AlarmMappingSeverities, ", "))
	}
	*s = AlarmSeverity(strings.ToUpper(strings.TrimSpace(node.Value)))
	return nil
}

// JSONSchema offers the severities for completion and accepts them in any
// case
func (AlarmSeverity) JSONSchema() *jsonschema.Schema {
	enum := make([]any, len(AlarmMappingSeverities))
	var patterns []string
	for i, severity := range AlarmMappingSeverities {
		enum[i] = severity
		var pattern strings.Builder
		for _, r := range severity {
			pattern.WriteString("[" + string(r) + strings.ToLower(string(r)) + "]")
		}
		patterns = append(patterns, pattern.String())
	}
	return &jsonschema.Schema{
		Description: "New severity of the alarms of this type (case-insensitive); NONE drops the alarms",
		AnyOf: []*jsonschema.Schema{
			{Type: "string", Enum: enum},
			{Type: "string", Pattern: "^(" + strings.Join(patterns, "|") + ")$"},
		},
	}
}

// AlarmMappingSpec maps the alarms of a type to a new severity and
// description (the Alarm mapping page of the Administration application).
//
// Cumulocity stores the mapping as the tenant options of the
// alarm.type.mapping category (key: the alarm type, value:
// "SEVERITY|description"); the manifest uses dedicated fields instead of that
// encoded string.
type AlarmMappingSpec struct {
	Type        string        `yaml:"type" json:"type" jsonschema:"description=Alarm type to map,minLength=1,example=c8y_UnavailabilityAlarm"`
	Severity    AlarmSeverity `yaml:"severity" json:"severity,omitempty"`
	Description string        `yaml:"description" json:"description,omitempty" jsonschema:"description=New alarm text; leave it out to keep the text of the alarm"`
	// State is present (the default) or absent
	State string `yaml:"state" json:"state,omitempty" jsonschema:"description=present (default) creates or updates the mapping; absent deletes the mapping of the type if it exists (severity and description are then optional and ignored),enum=present,enum=absent"`
}

// Absent reports whether the mapping is to be deleted
func (m AlarmMappingSpec) Absent() bool {
	return strings.TrimSpace(m.State) == AlarmMappingStateAbsent
}

// JSONSchemaExtend requires a severity unless the state is absent (an
// absent entry may keep its severity and description, so switching the
// state is a one-line change)
func (AlarmMappingSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	state := jsonschema.NewProperties()
	state.Set("state", &jsonschema.Schema{Const: AlarmMappingStateAbsent})
	schema.If = &jsonschema.Schema{Properties: state, Required: []string{"state"}}
	schema.Else = &jsonschema.Schema{Required: []string{"severity"}}
}

// Value encodes the mapping as the tenant option value
// ("SEVERITY|description")
func (m AlarmMappingSpec) Value() string {
	return string(m.Severity) + "|" + m.Description
}

// String describes the mapping in the run output
func (m AlarmMappingSpec) String() string {
	if m.Description == "" {
		return "severity " + string(m.Severity) + " (keep description)"
	}
	return fmt.Sprintf("severity %s, description %q", m.Severity, m.Description)
}

// parseAlarmMapping decodes a tenant option of the alarm mapping
// ("SEVERITY|description"; the description may be missing). The severity
// is converted to upper case like in the manifest.
func parseAlarmMapping(alarmType, value string) AlarmMappingSpec {
	severity, description, _ := strings.Cut(value, "|")
	return AlarmMappingSpec{Type: alarmType, Severity: AlarmSeverity(strings.ToUpper(severity)), Description: description}
}

// describeAlarmMappingChange lists the changed fields of a mapping
func describeAlarmMappingChange(current, desired AlarmMappingSpec) string {
	var changes []string
	if current.Severity != desired.Severity {
		changes = append(changes, fmt.Sprintf("severity %s → %s", current.Severity, desired.Severity))
	}
	if current.Description != desired.Description {
		changes = append(changes, fmt.Sprintf("description %q → %q", current.Description, desired.Description))
	}
	return strings.Join(changes, "; ")
}

// validateAlarmMapping checks the mode, the alarm types and the severities
// of the mapping, and that tenantOptions does not set the same options
func validateAlarmMapping(section *AlarmMappingSectionSpec, options []TenantOptionSpec) []string {
	if section == nil {
		return nil
	}
	var errs []string
	path := "alarmMapping"
	if section.Mode != "" {
		path = "alarmMapping.mappings"
		if !slices.Contains(AlarmMappingModes, section.Mode) {
			errs = append(errs, fmt.Sprintf("alarmMapping.mode: unknown mode %q: supported values are %s", section.Mode, strings.Join(AlarmMappingModes, ", ")))
		}
	}
	types := make(map[string]bool, len(section.Mappings))
	for i, mapping := range section.Mappings {
		item := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case strings.TrimSpace(mapping.Type) == "":
			errs = append(errs, item+": type is required")
		case types[mapping.Type]:
			errs = append(errs, fmt.Sprintf("%s: duplicate type %s", item, mapping.Type))
		}
		types[mapping.Type] = true
		if mapping.State != "" && !slices.Contains(AlarmMappingStates, mapping.State) {
			errs = append(errs, fmt.Sprintf("%s: unknown state %q: supported values are %s", item, mapping.State, strings.Join(AlarmMappingStates, ", ")))
		}
		// An absent entry may keep its severity and description (they are
		// ignored), but a severity which is set must still be valid, so
		// switching the state back to present works
		switch {
		case mapping.Severity == "" && mapping.Absent():
		case mapping.Severity == "":
			errs = append(errs, fmt.Sprintf("%s: severity is required (one of %s)", item, strings.Join(AlarmMappingSeverities, ", ")))
		case !slices.Contains(AlarmMappingSeverities, string(mapping.Severity)):
			errs = append(errs, fmt.Sprintf("%s: unknown severity %q: supported values are %s", item, mapping.Severity, strings.Join(AlarmMappingSeverities, ", ")))
		}
	}
	for i, opt := range options {
		if opt.Category != AlarmMappingCategory {
			continue
		}
		switch {
		case types[opt.Key]:
			errs = append(errs, fmt.Sprintf("tenantOptions[%d]: %s/%s is also set in alarmMapping (set it in one place only)", i, opt.Category, opt.Key))
		case section.Exact():
			errs = append(errs, fmt.Sprintf("tenantOptions[%d]: %s/%s would be deleted by alarmMapping mode exact (move it to alarmMapping)", i, opt.Category, opt.Key))
		}
	}
	return errs
}

// mergeAlarmMapping merges the alarmMapping sections of two manifests: the
// mappings by type (the later file wins) and the mode of the later file
// when it sets one
func mergeAlarmMapping(base, over *AlarmMappingSectionSpec) *AlarmMappingSectionSpec {
	if base == nil && over == nil {
		return nil
	}
	out := &AlarmMappingSectionSpec{}
	var baseMappings, overMappings []AlarmMappingSpec
	if base != nil {
		out.Mode = base.Mode
		baseMappings = base.Mappings
	}
	if over != nil {
		if over.Mode != "" {
			out.Mode = over.Mode
		}
		overMappings = over.Mappings
	}
	out.Mappings = mergeByKey(baseMappings, overMappings, func(m AlarmMappingSpec) string { return m.Type })
	return out
}

// SyncAlarmMapping sets the alarm mapping of the tenant. In the default
// additive mode, alarm types which are not in the manifest keep their
// mapping and only mappings with state absent are deleted; in exact mode
// the mappings of alarm types which are not listed are deleted too. All
// created and updated mappings are written with one request, deletes take
// one request each. A dry run compares the mapping with the tenant when it
// can read it.
func (s *Syncer) SyncAlarmMapping(ctx context.Context, section *AlarmMappingSectionSpec) error {
	if section == nil || (len(section.Mappings) == 0 && !section.Exact()) {
		return nil
	}
	mappings := section.Mappings

	reader := s.Client
	if s.DryRun {
		reader = s.dryRunReader()
	}
	if reader == nil {
		// Dry run without a session: list the desired mapping only
		for _, mapping := range mappings {
			if mapping.Absent() {
				s.record(SectionAlarmMapping, mapping.Type, ActionPlanned, "delete if it exists", nil)
			} else {
				s.record(SectionAlarmMapping, mapping.Type, ActionPlanned, "set "+mapping.String(), nil)
			}
		}
		if section.Exact() {
			s.record(SectionAlarmMapping, AlarmMappingCategory, ActionPlanned, "delete the mappings of alarm types which are not listed (mode exact)", nil)
		}
		return nil
	}

	existing := reader.Tenants.Options.ListByCategory(ctx, tenantoptions.ListByCategoryOptions{Category: AlarmMappingCategory})
	if existing.Err != nil {
		s.record(SectionAlarmMapping, AlarmMappingCategory, ActionFailed, "read the alarm mapping", existing.Err)
		return nil
	}

	// Mappings are compared by their fields, so "MAJOR" and "MAJOR|" in the
	// tenant are the same mapping
	changes := map[string]string{}
	details := map[string]string{}
	listed := make(map[string]bool, len(mappings))
	for _, desired := range mappings {
		listed[desired.Type] = true
		if desired.Absent() {
			continue
		}
		value, ok := existing.Data[desired.Type]
		switch current := parseAlarmMapping(desired.Type, value); {
		case !ok:
			changes[desired.Type] = desired.Value()
			details[desired.Type] = desired.String()
		case current != desired:
			changes[desired.Type] = desired.Value()
			details[desired.Type] = describeAlarmMappingChange(current, desired)
		}
	}

	var writeErr error
	if len(changes) > 0 && !s.DryRun {
		writeErr = s.Client.Tenants.Options.UpdateByCategory(ctx, tenantoptions.UpdateByCategoryOption{
			Category: AlarmMappingCategory,
			Body:     changes,
		}).Err
	}

	for _, mapping := range mappings {
		value, exists := existing.Data[mapping.Type]
		if mapping.Absent() {
			s.deleteAlarmMapping(ctx, mapping.Type, value, exists, "")
			continue
		}
		detail, changed := details[mapping.Type]
		switch {
		case !changed:
			s.record(SectionAlarmMapping, mapping.Type, ActionUnchanged, mapping.String(), nil)
		case s.DryRun:
			s.record(SectionAlarmMapping, mapping.Type, ActionPlanned, detail, nil)
		case writeErr != nil:
			s.record(SectionAlarmMapping, mapping.Type, ActionFailed, "set "+mapping.String(), writeErr)
		case exists:
			s.record(SectionAlarmMapping, mapping.Type, ActionUpdated, detail, nil)
		default:
			s.record(SectionAlarmMapping, mapping.Type, ActionCreated, detail, nil)
		}
	}

	if section.Exact() {
		for _, alarmType := range slices.Sorted(maps.Keys(existing.Data)) {
			if !listed[alarmType] {
				s.deleteAlarmMapping(ctx, alarmType, existing.Data[alarmType], true, "not in the manifest, ")
			}
		}
	}
	return nil
}

// deleteAlarmMapping deletes the mapping of an alarm type (state absent, or
// not listed in exact mode). value is the current mapping in the tenant, if
// it exists; reason prefixes the detail.
func (s *Syncer) deleteAlarmMapping(ctx context.Context, alarmType, value string, exists bool, reason string) {
	if !exists {
		s.record(SectionAlarmMapping, alarmType, ActionUnchanged, "absent", nil)
		return
	}
	detail := reason + "was " + parseAlarmMapping(alarmType, value).String()
	if s.DryRun {
		s.record(SectionAlarmMapping, alarmType, ActionPlanned, "delete ("+detail+")", nil)
		return
	}
	result := s.Client.Tenants.Options.Delete(ctx, tenantoptions.DeleteOptions{Category: AlarmMappingCategory, Key: alarmType})
	if result.Err != nil {
		s.record(SectionAlarmMapping, alarmType, ActionFailed, "delete ("+detail+")", result.Err)
		return
	}
	s.record(SectionAlarmMapping, alarmType, ActionDeleted, detail, nil)
}

// alarmMapping reads the alarm mapping of the tenant, sorted by type. It is
// exported in the default additive mode.
func (e *Exporter) alarmMapping(ctx context.Context) (*AlarmMappingSectionSpec, error) {
	result := e.Client.Tenants.Options.ListByCategory(ctx, tenantoptions.ListByCategoryOptions{Category: AlarmMappingCategory})
	if result.Err != nil {
		return nil, result.Err
	}
	if len(result.Data) == 0 {
		return nil, nil
	}
	section := &AlarmMappingSectionSpec{}
	for _, alarmType := range slices.Sorted(maps.Keys(result.Data)) {
		section.Mappings = append(section.Mappings, parseAlarmMapping(alarmType, result.Data[alarmType]))
	}
	return section, nil
}
