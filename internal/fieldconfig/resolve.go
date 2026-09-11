package fieldconfig

import (
	"fmt"
	"sort"
	"strings"
)

// Resolved is one field as it applies to one company: the declaration, plus
// whatever that company decided about it.
type Resolved struct {
	Field
	// Requirement is the effective answer — the company's override if it made
	// one, otherwise the declared default.
	Requirement Requirement `json:"requirement"`

	// EffectiveLabel is the company's wording if it set one.
	EffectiveLabel string `json:"label"`

	// Overridden says the answer came from configuration rather than the
	// default. A configurator needs this to show what has been touched, and to
	// offer "reset" as distinct from "set to the same value".
	Overridden bool `json:"overridden"`
}

// Override is one stored decision.
type Override struct {
	Entity        Entity
	Key           string
	Requirement   Requirement
	LabelOverride string
}

// Config is a company's effective configuration for one entity.
type Config struct {
	Entity Entity              `json:"entity"`
	Fields map[string]Resolved `json:"-"`
}

// Resolve folds a company's overrides over the declared catalogue.
//
// Overrides for keys the code no longer declares are dropped rather than
// surfaced as fields. They are kept in the database on purpose — so "who still
// configures something that no longer exists" stays answerable — but honouring
// one here would resurrect a field nothing renders or reads.
//
// An override on a Locked field is likewise ignored. Such a row should not
// exist, because Set refuses to write one; ignoring it here means a row that
// arrived some other way (a manual fix, a restored backup) cannot make an order
// unsubmittable by hiding its loading point.
func Resolve(entity Entity, overrides []Override) Config {
	cfg := Config{Entity: entity, Fields: make(map[string]Resolved, len(Catalog[entity]))}

	for key, f := range Catalog[entity] {
		cfg.Fields[key] = Resolved{Field: f, Requirement: f.Default, EffectiveLabel: f.Label}
	}

	for _, o := range overrides {
		if o.Entity != entity {
			continue
		}
		current, declared := cfg.Fields[o.Key]
		if !declared || current.Locked {
			continue
		}
		if o.Requirement.Valid() {
			current.Requirement = o.Requirement
			current.Overridden = true
		}
		if o.LabelOverride != "" {
			current.EffectiveLabel = o.LabelOverride
			current.Overridden = true
		}
		cfg.Fields[o.Key] = current
	}

	return cfg
}

// Ordered returns the effective fields in presentation order, for a form
// renderer and for the configurator screen.
func (c Config) Ordered() []Resolved {
	out := make([]Resolved, 0, len(c.Fields))
	for _, f := range c.Fields {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Visible reports whether a field should be drawn and accepted at all.
func (c Config) Visible(key string) bool {
	f, ok := c.Fields[key]
	return ok && f.Requirement != Hidden
}

// RequirementOf returns the effective requirement, or Optional for a key this
// entity does not declare. Optional rather than Hidden, because an unknown key
// is a caller mistake and should not cause the stricter refusal path to fire on
// something the catalogue has no opinion about.
func (c Config) RequirementOf(key string) Requirement {
	if f, ok := c.Fields[key]; ok {
		return f.Requirement
	}
	return Optional
}

// ValidationError lists what was wrong with a submission, all of it at once.
//
// All of it at once matters: a form that reports one missing field per attempt
// makes filling in a nine-field agreement a nine-round trip.
type ValidationError struct {
	Missing   []string `json:"missing,omitempty"`
	NotWanted []string `json:"notWanted,omitempty"`
	labels    map[string]string
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, 2)
	if len(e.Missing) > 0 {
		parts = append(parts, "missing required: "+strings.Join(e.labelsFor(e.Missing), ", "))
	}
	if len(e.NotWanted) > 0 {
		parts = append(parts, "not enabled for your company: "+strings.Join(e.labelsFor(e.NotWanted), ", "))
	}
	return strings.Join(parts, "; ")
}

func (e *ValidationError) labelsFor(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if label, ok := e.labels[k]; ok {
			out = append(out, label)
			continue
		}
		out = append(out, k)
	}
	return out
}

// Validate checks a submission against the effective configuration.
//
// present is the set of keys the caller actually supplied a value for. It is
// passed in rather than derived from a struct because "supplied" and "non-zero"
// are different questions: a quantity of 0 and an omitted quantity are not the
// same submission, and a reflection-based check cannot tell them apart.
//
// A hidden field carrying a value is refused, not ignored. Silently dropping it
// looks identical to saving it from the caller's side, and the difference
// surfaces weeks later as a kecamatan somebody swears they filled in.
func (c Config) Validate(present map[string]bool) error {
	var missing, notWanted []string

	for key, f := range c.Fields {
		switch f.Requirement {
		case Required:
			if !present[key] {
				missing = append(missing, key)
			}
		case Hidden:
			if present[key] {
				notWanted = append(notWanted, key)
			}
		}
	}

	if len(missing) == 0 && len(notWanted) == 0 {
		return nil
	}

	sort.Strings(missing)
	sort.Strings(notWanted)

	labels := make(map[string]string, len(c.Fields))
	for key, f := range c.Fields {
		labels[key] = f.EffectiveLabel
	}
	return &ValidationError{Missing: missing, NotWanted: notWanted, labels: labels}
}

// CheckOverride reports whether a company may set this requirement on this
// field, so the refusal happens where the change is made rather than becoming a
// row that quietly does nothing.
func CheckOverride(entity Entity, key string, r Requirement) error {
	f, ok := Catalog[entity][key]
	if !ok {
		return fmt.Errorf("fieldconfig: %s has no field %q", entity, key)
	}
	if f.Locked {
		return fmt.Errorf("fieldconfig: %q cannot be reconfigured", f.Label)
	}
	if !r.Valid() {
		return fmt.Errorf("fieldconfig: %q is not a requirement (required, optional, hidden)", r)
	}
	return nil
}
