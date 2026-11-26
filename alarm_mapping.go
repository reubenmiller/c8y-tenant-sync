package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/tenantoptions"
)

// AlarmMappingCategory is the tenant option category which holds the alarm
// mapping: one option per alarm type with the value "SEVERITY|text"
const AlarmMappingCategory = "alarm.type.mapping"

// AlarmMappingSeverities are the severities an alarm type can be mapped to.
// NONE drops the alarms of the type.
var AlarmMappingSeverities = []string{"CRITICAL", "MAJOR", "MINOR", "WARNING", "NONE"}

// AlarmMappingSpec maps alarm types to a new severity and text (the Alarm
// mapping page of the Administration application).
//
// Cumulocity stores it as the tenant options of the alarm.type.mapping
// category with the value "SEVERITY|text"; the manifest uses dedicated
// fields instead of that encoded string.
type AlarmMappingSpec map[string]AlarmMappingEntry

// JSONSchemaExtend requires non-empty alarm types
func (AlarmMappingSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.PropertyNames = &jsonschema.Schema{MinLength: uint64Ptr(1)}
}

func uint64Ptr(v uint64) *uint64 { return &v }

// AlarmMappingEntry is the mapping of one alarm type
type AlarmMappingEntry struct {
	Severity string `yaml:"severity" json:"severity" jsonschema:"description=New severity of the alarms of this type; NONE drops the alarms,enum=CRITICAL,enum=MAJOR,enum=MINOR,enum=WARNING,enum=NONE"`
	Text     string `yaml:"text" json:"text,omitempty" jsonschema:"description=New alarm text; leave it out to keep the text of the alarm"`
}

// Value encodes the entry as the tenant option value ("SEVERITY|text")
func (e AlarmMappingEntry) Value() string {
	return e.Severity + "|" + e.Text
}

// String describes the entry in the run output
func (e AlarmMappingEntry) String() string {
	if e.Text == "" {
		return "severity " + e.Severity + " (keep text)"
	}
	return fmt.Sprintf("severity %s, text %q", e.Severity, e.Text)
}

// parseAlarmMappingValue decodes a tenant option value ("SEVERITY|text";
// the text may be missing)
func parseAlarmMappingValue(value string) AlarmMappingEntry {
	severity, text, _ := strings.Cut(value, "|")
	return AlarmMappingEntry{Severity: severity, Text: text}
}

// describeAlarmMappingChange lists the changed fields of an entry
func describeAlarmMappingChange(current, desired AlarmMappingEntry) string {
	var changes []string
	if current.Severity != desired.Severity {
		changes = append(changes, fmt.Sprintf("severity %s → %s", current.Severity, desired.Severity))
	}
	if current.Text != desired.Text {
		changes = append(changes, fmt.Sprintf("text %q → %q", current.Text, desired.Text))
	}
	return strings.Join(changes, "; ")
}

// validateAlarmMapping checks the alarm types and the severities of the
// mapping, and that tenantOptions does not set the same options
func validateAlarmMapping(mapping AlarmMappingSpec, options []TenantOptionSpec) []string {
	var errs []string
	for _, alarmType := range slices.Sorted(maps.Keys(mapping)) {
		if strings.TrimSpace(alarmType) == "" {
			errs = append(errs, "alarmMapping: the alarm type must not be empty")
			continue
		}
		entry := mapping[alarmType]
		switch {
		case entry.Severity == "":
			errs = append(errs, fmt.Sprintf("alarmMapping.%s: severity is required (one of %s)", alarmType, strings.Join(AlarmMappingSeverities, ", ")))
		case !slices.Contains(AlarmMappingSeverities, entry.Severity):
			errs = append(errs, fmt.Sprintf("alarmMapping.%s: unknown severity %q: supported values are %s", alarmType, entry.Severity, strings.Join(AlarmMappingSeverities, ", ")))
		}
	}
	for i, opt := range options {
		if opt.Category != AlarmMappingCategory {
			continue
		}
		if _, ok := mapping[opt.Key]; ok {
			errs = append(errs, fmt.Sprintf("tenantOptions[%d]: %s/%s is also set in alarmMapping (set it in one place only)", i, opt.Category, opt.Key))
		}
	}
	return errs
}

// SyncAlarmMapping sets the alarm mapping of the tenant. It is additive:
// alarm types which are not in the manifest keep their mapping. All
// changes are written with one request. A dry run compares the mapping with
// the tenant when it can read it.
func (s *Syncer) SyncAlarmMapping(ctx context.Context, mapping AlarmMappingSpec) error {
	if len(mapping) == 0 {
		return nil
	}
	alarmTypes := slices.Sorted(maps.Keys(mapping))

	reader := s.Client
	if s.DryRun {
		reader = s.dryRunReader()
	}
	if reader == nil {
		// Dry run without a session: list the desired mapping only
		for _, alarmType := range alarmTypes {
			s.record(SectionAlarmMapping, alarmType, ActionPlanned, "set "+mapping[alarmType].String(), nil)
		}
		return nil
	}

	existing := reader.Tenants.Options.ListByCategory(ctx, tenantoptions.ListByCategoryOptions{Category: AlarmMappingCategory})
	if existing.Err != nil {
		s.record(SectionAlarmMapping, AlarmMappingCategory, ActionFailed, "read the alarm mapping", existing.Err)
		return nil
	}

	// Entries are compared by their fields, so "MAJOR" and "MAJOR|" in the
	// tenant are the same mapping
	changes := map[string]string{}
	details := map[string]string{}
	for _, alarmType := range alarmTypes {
		desired := mapping[alarmType]
		value, ok := existing.Data[alarmType]
		switch current := parseAlarmMappingValue(value); {
		case !ok:
			changes[alarmType] = desired.Value()
			details[alarmType] = desired.String()
		case current != desired:
			changes[alarmType] = desired.Value()
			details[alarmType] = describeAlarmMappingChange(current, desired)
		}
	}

	var writeErr error
	if len(changes) > 0 && !s.DryRun {
		writeErr = s.Client.Tenants.Options.UpdateByCategory(ctx, tenantoptions.UpdateByCategoryOption{
			Category: AlarmMappingCategory,
			Body:     changes,
		}).Err
	}

	for _, alarmType := range alarmTypes {
		detail, changed := details[alarmType]
		_, exists := existing.Data[alarmType]
		switch {
		case !changed:
			s.record(SectionAlarmMapping, alarmType, ActionUnchanged, mapping[alarmType].String(), nil)
		case s.DryRun:
			s.record(SectionAlarmMapping, alarmType, ActionPlanned, detail, nil)
		case writeErr != nil:
			s.record(SectionAlarmMapping, alarmType, ActionFailed, "set "+mapping[alarmType].String(), writeErr)
		case exists:
			s.record(SectionAlarmMapping, alarmType, ActionUpdated, detail, nil)
		default:
			s.record(SectionAlarmMapping, alarmType, ActionCreated, detail, nil)
		}
	}
	return nil
}

// alarmMapping reads the alarm mapping of the tenant
func (e *Exporter) alarmMapping(ctx context.Context) (AlarmMappingSpec, error) {
	result := e.Client.Tenants.Options.ListByCategory(ctx, tenantoptions.ListByCategoryOptions{Category: AlarmMappingCategory})
	if result.Err != nil {
		return nil, result.Err
	}
	if len(result.Data) == 0 {
		return nil, nil
	}
	mapping := make(AlarmMappingSpec, len(result.Data))
	for alarmType, value := range result.Data {
		mapping[alarmType] = parseAlarmMappingValue(value)
	}
	return mapping, nil
}
