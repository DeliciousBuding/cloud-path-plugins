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
// application. Modes, schedules and feedback are expressed only in terms of
// bound capabilities, never a Driver ID, a port or a vendor-specific field.
type Config struct {
	// Mode defaults to walking-light. call is an alias for service-call.
	Mode     string `json:"mode"`
	Timezone string `json:"timezone"`
	// HeartbeatCron is a 5-field cron expression (minute hour dom month dow)
	// interpreted in Timezone. The scheduled job writes a heartbeat domain
	// record on every dispatch, exercising the Core Durable Scheduler.
	HeartbeatCron string `json:"heartbeat_cron"`
	// BeepOnPress optionally emits a short buzzer beep on a walking-light
	// press or a newly created service call (not on coalesced presses).
	// Default false: indication is silent.
	BeepOnPress bool `json:"beep_on_press"`
}

const (
	modeWalkingLight = "walking-light"
	modeServiceCall  = "service-call"
)

// ResolvedMode preserves the original behavior unless call mode is explicit.
func (c Config) ResolvedMode() string {
	switch mode := strings.TrimSpace(c.Mode); mode {
	case "":
		return modeWalkingLight
	case "call":
		return modeServiceCall
	default:
		return mode
	}
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

	if mode := c.ResolvedMode(); mode != modeWalkingLight && mode != modeServiceCall {
		errs = append(errs, "mode must be walking-light, call or service-call")
	}

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
