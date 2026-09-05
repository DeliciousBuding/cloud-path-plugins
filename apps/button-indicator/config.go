package buttonindicator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	// Embed the IANA timezone database so timezone names (e.g. "Asia/Shanghai")
	// resolve on every host. The release binary is built on linux CI and runs
	// on Windows where no system zoneinfo exists — without this import the
	// config validation rejects every non-UTC timezone (2026-09-05 D3 real-
	// board run: "Asia/Shanghai is not a valid IANA name").
	_ "time/tzdata"
)

// Config is the bounded instance configuration for the Button Indicator
// application. It is device-agnostic: a timezone for cron interpretation, the
// declarative heartbeat cron and a beep toggle. It never references a Driver
// ID, a port or any vendor-specific field.
type Config struct {
	Timezone string `json:"timezone"`
	// HeartbeatCron is a 5-field cron expression (minute hour dom month dow)
	// interpreted in Timezone. The scheduled job writes a heartbeat domain
	// record on every dispatch, exercising the Core Durable Scheduler.
	HeartbeatCron string `json:"heartbeat_cron"`
	// BeepOnPress optionally emits a short buzzer beep on every press.
	// Default false: the walking light is the primary feedback.
	BeepOnPress bool `json:"beep_on_press"`
}

// defaultHeartbeatCron is the fallback when the config omits the cron: a
// quiet every-five-minutes heartbeat.
const defaultHeartbeatCron = "*/5 * * * *"

// ResolvedHeartbeatCron returns the effective heartbeat cron with the default
// applied.
func (c Config) ResolvedHeartbeatCron() string {
	if strings.TrimSpace(c.HeartbeatCron) == "" {
		return defaultHeartbeatCron
	}
	return c.HeartbeatCron
}

// Validate checks the config against the bounded schema. Timezone defaults to
// UTC when empty; the cron must be a 5-field expression (full grammar is
// validated by the Core scheduler when the effect lands, this is the early
// rejection so typos fail at configure time instead of silently disabling the
// heartbeat).
func (c Config) Validate() error {
	var errs []string

	tz := strings.TrimSpace(c.Timezone)
	if tz == "" {
		// default UTC, nothing to check
	} else if _, err := time.LoadLocation(tz); err != nil {
		errs = append(errs, fmt.Sprintf("timezone %q is not a valid IANA name", tz))
	}

	if fields := strings.Fields(c.ResolvedHeartbeatCron()); len(fields) != 5 {
		errs = append(errs, fmt.Sprintf("heartbeat_cron %q must have exactly 5 fields (minute hour dom month dow)",
			c.ResolvedHeartbeatCron()))
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// UnmarshalConfig parses and validates the instance config JSON. Invalid JSON
// or invalid semantics yield a descriptive error.
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
