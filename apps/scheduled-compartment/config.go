package scheduledcompartment

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	// Embed the IANA timezone database so timezone names (e.g. "Asia/Shanghai")
	// resolve on every host, including Windows where the registry zone names
	// differ from IANA names.
	_ "time/tzdata"
)

// Config is the bounded instance configuration for the Scheduled Compartment
// application. It is intentionally device-agnostic: it carries a timezone,
// the configured compartments, a list of daily schedule windows and a bounded
// reminder policy. It never references a Driver ID, a port or any
// vendor-specific field.
type Config struct {
	Timezone     string        `json:"timezone"`
	Compartments []Compartment `json:"compartments"`
	Schedule     []WindowSpec  `json:"schedule"`
	// Reminder configures the reminder-output effect payload (buzzer freq and
	// duration steps). Zero values fall back to the quiet defaults.
	Reminder *Reminder `json:"reminder,omitempty"`
}

// Reminder is the bounded reminder policy for the buzzer capability action.
// Freq and Duration are hardware step levels (0-9) as defined by the buzzer
// capability; defaults are deliberately quiet and short.
type Reminder struct {
	Freq     int `json:"freq"`
	Duration int `json:"duration"`
}

// defaultReminder is the fallback when the config omits the reminder policy:
// the lowest meaningful volume step and a short single beep. An explicit
// reminder block is honoured as-is (including a deliberately silent one).
var defaultReminder = Reminder{Freq: 1, Duration: 1}

// ResolvedReminder returns the effective reminder policy with defaults applied.
func (c Config) ResolvedReminder() Reminder {
	if c.Reminder == nil {
		return defaultReminder
	}
	return *c.Reminder
}

// Compartment is one managed compartment. ID is the stable, local key used to
// bind a schedule window to a compartment entity.
type Compartment struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// WindowSpec declares one daily schedule window in the instance timezone.
// Start and End are "HH:MM" (24-hour) in the configured timezone. End must be
// strictly after Start (midnight-crossing windows are not supported).
type WindowSpec struct {
	ID          string `json:"id"`
	Compartment string `json:"compartment"`
	Start       string `json:"start"`
	End         string `json:"end"`
}

// Validate checks the whole config against the bounded schema. It returns a
// descriptive error on the first set of violations; a valid config returns nil.
func (c Config) Validate() error {
	var errs []string

	if strings.TrimSpace(c.Timezone) == "" {
		errs = append(errs, "timezone is required")
	} else if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Sprintf("timezone %q is not a valid IANA name", c.Timezone))
	}

	if len(c.Compartments) == 0 {
		errs = append(errs, "compartments must contain at least one compartment")
	}
	compartmentSeen := map[string]bool{}
	for i, cp := range c.Compartments {
		id := strings.TrimSpace(cp.ID)
		if id == "" {
			errs = append(errs, fmt.Sprintf("compartments[%d].id is required", i))
			continue
		}
		if compartmentSeen[id] {
			errs = append(errs, fmt.Sprintf("duplicate compartment id %q", id))
		}
		compartmentSeen[id] = true
	}

	if len(c.Schedule) == 0 {
		errs = append(errs, "schedule must contain at least one window")
	}
	windowSeen := map[string]bool{}
	for i, w := range c.Schedule {
		id := strings.TrimSpace(w.ID)
		if id == "" {
			errs = append(errs, fmt.Sprintf("schedule[%d].id is required", i))
		} else if windowSeen[id] {
			errs = append(errs, fmt.Sprintf("duplicate window id %q", id))
		}
		windowSeen[id] = true

		if w.Compartment == "" {
			errs = append(errs, fmt.Sprintf("schedule[%d].compartment is required", i))
		} else if !compartmentSeen[w.Compartment] {
			errs = append(errs, fmt.Sprintf("schedule[%d].compartment %q is not a configured compartment", i, w.Compartment))
		}

		start, ok := parseHHMM(w.Start)
		if !ok {
			errs = append(errs, fmt.Sprintf("schedule[%d].start %q is not a valid HH:MM time", i, w.Start))
		}
		end, ok := parseHHMM(w.End)
		if !ok {
			errs = append(errs, fmt.Sprintf("schedule[%d].end %q is not a valid HH:MM time", i, w.End))
		}
		if ok && !end.After(start) {
			errs = append(errs, fmt.Sprintf("schedule[%d].end %q must be after start %q", i, w.End, w.Start))
		}
	}

	if c.Reminder != nil {
		if c.Reminder.Freq < 0 || c.Reminder.Freq > 9 {
			errs = append(errs, fmt.Sprintf("reminder.freq %d is outside the buzzer step range 0-9", c.Reminder.Freq))
		}
		if c.Reminder.Duration < 0 || c.Reminder.Duration > 9 {
			errs = append(errs, fmt.Sprintf("reminder.duration %d is outside the buzzer step range 0-9", c.Reminder.Duration))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// parseHHMM parses a "HH:MM" 24-hour clock string used by schedule windows.
func parseHHMM(s string) (time.Time, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// UnmarshalConfig parses and validates the instance config JSON. Invalid JSON
// or invalid semantics yield a non-OK status-worthy error.
func UnmarshalConfig(data []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
