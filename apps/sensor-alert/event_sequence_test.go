// SPDX-License-Identifier: Apache-2.0

package sensoralert

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

func completionFor(command *application.RequestCommand, state application.CommandState) *application.RequestCompleted {
	return &application.RequestCompleted{
		RequestID: command.IdempotencyKey, EntityID: command.EntityID,
		Action: command.Action, State: state,
	}
}

func commandByAction(t *testing.T, commands []*application.RequestCommand, action string) *application.RequestCommand {
	t.Helper()
	for _, command := range commands {
		if command.Action == action {
			return command
		}
	}
	t.Fatalf("missing %s command in %+v", action, commands)
	return nil
}

func assertSingleAlertState(t *testing.T, alerts []AlertRecord, state, sensor string) AlertRecord {
	t.Helper()
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want exactly one", alerts)
	}
	if alerts[0].State != state || (sensor != "" && alerts[0].Sensor != sensor) {
		t.Fatalf("alert = %+v, want state=%s sensor=%s", alerts[0], state, sensor)
	}
	return alerts[0]
}

func assertLedMask(t *testing.T, command *application.RequestCommand, want int) {
	t.Helper()
	var args map[string]int
	if err := json.Unmarshal([]byte(command.ArgsJSON), &args); err != nil {
		t.Fatalf("decode LED args %q: %v", command.ArgsJSON, err)
	}
	if args["mask"] != want {
		t.Fatalf("LED mask = %d, want %d (args=%s)", args["mask"], want, command.ArgsJSON)
	}
}

func TestTemperatureLifecycleSequence(t *testing.T) {
	h := newHarness(t, DefaultConfig(), []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()

	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	triggered := assertSingleAlertState(t, h.writer.alerts(t), stateTriggered, TemperatureRequirement)
	if triggered.Value == nil || *triggered.Value != 35 || triggered.Threshold == nil || *triggered.Threshold != 28 {
		t.Fatalf("triggered values = %+v", triggered)
	}
	if triggered.TriggeredAt == nil || triggered.RecoveredAt != nil {
		t.Fatalf("triggered timestamps = %+v", triggered)
	}
	commands := h.writer.commands()
	if len(commands) != 2 {
		t.Fatalf("trigger commands = %+v", commands)
	}
	tone := commandByAction(t, commands, actionTone)
	led := commandByAction(t, commands, actionLED)
	var toneArgs ToneConfig
	if err := json.Unmarshal([]byte(tone.ArgsJSON), &toneArgs); err != nil {
		t.Fatal(err)
	}
	if toneArgs != (ToneConfig{FrequencyHz: 1000, DurationMs: 200}) {
		t.Fatalf("tone args = %+v", toneArgs)
	}
	assertLedMask(t, led, 255)

	// Complete out of order: the application must settle both independently.
	h.event(t, 2, completionFor(led, application.CommandStateSucceeded))
	h.event(t, 3, completionFor(tone, application.CommandStateSucceeded))
	st, err := h.svc.lookup(testInstance, false)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	if len(st.pending) != 0 || st.lastCommand == nil || st.lastCommand.State != "succeeded" {
		t.Fatalf("completion state: pending=%+v last=%+v", st.pending, st.lastCommand)
	}
	st.mu.Unlock()

	h.writer.reset()
	h.now = h.now.Add(10 * time.Second)
	h.observation(t, 4, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 20, "good")
	recovered := assertSingleAlertState(t, h.writer.alerts(t), stateRecovered, TemperatureRequirement)
	if recovered.RecoveredAt == nil || recovered.Value == nil || *recovered.Value != 20 {
		t.Fatalf("recovered values = %+v", recovered)
	}
	recoveryCommands := h.writer.commands()
	if len(recoveryCommands) != 1 || recoveryCommands[0].Action != actionLED {
		t.Fatalf("recovery commands = %+v", recoveryCommands)
	}
	assertLedMask(t, recoveryCommands[0], 0)
	h.event(t, 5, completionFor(recoveryCommands[0], application.CommandStateSucceeded))

	h.writer.reset()
	disarm, err := h.svc.RunJob(t.Context(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobDisarm, ArgsJSON: `{}`, IdempotencyKey: "disarm-sequence",
	})
	if err != nil || !disarm.Status.IsOK() {
		t.Fatalf("disarm: resp=%+v err=%v", disarm, err)
	}
	assertSingleAlertState(t, h.writer.alerts(t), stateDisarmed, "")
	disarmCommands := h.writer.commands()
	if len(disarmCommands) != 1 || disarmCommands[0].Action != actionLED {
		t.Fatalf("disarm commands = %+v", disarmCommands)
	}
	assertLedMask(t, disarmCommands[0], 0)
}

func TestIlluminanceLowHighRecoverySequence(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CooldownS = 0
	cfg.LightMin = floatPtr(20)
	cfg.LightMax = floatPtr(80)
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: IlluminanceRequirement, EntityID: "light-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()

	h.observation(t, 1, IlluminanceRequirement, "light-1", IlluminanceCapability, "value", 10, "good")
	low := assertSingleAlertState(t, h.writer.alerts(t), stateTriggered, IlluminanceRequirement)
	if low.Threshold == nil || *low.Threshold != 20 || low.Summary == "" || low.Severity != "warning" {
		t.Fatalf("low alert = %+v", low)
	}
	lowCommands := h.writer.commands()
	if len(lowCommands) != 2 {
		t.Fatalf("low commands = %+v", lowCommands)
	}
	h.event(t, 2, completionFor(commandByAction(t, lowCommands, actionTone), application.CommandStateSucceeded))
	h.event(t, 3, completionFor(commandByAction(t, lowCommands, actionLED), application.CommandStateSucceeded))

	h.writer.reset()
	h.observation(t, 4, IlluminanceRequirement, "light-1", IlluminanceCapability, "value", 50, "good")
	recovered := assertSingleAlertState(t, h.writer.alerts(t), stateRecovered, IlluminanceRequirement)
	if recovered.Value == nil || *recovered.Value != 50 {
		t.Fatalf("light recovery = %+v", recovered)
	}
	recoveryCommands := h.writer.commands()
	if len(recoveryCommands) != 1 || recoveryCommands[0].Action != actionLED {
		t.Fatalf("light recovery commands = %+v", recoveryCommands)
	}
	assertLedMask(t, recoveryCommands[0], 0)
	h.event(t, 5, completionFor(recoveryCommands[0], application.CommandStateSucceeded))

	h.writer.reset()
	h.observation(t, 6, IlluminanceRequirement, "light-1", IlluminanceCapability, "value", 100, "good")
	high := assertSingleAlertState(t, h.writer.alerts(t), stateTriggered, IlluminanceRequirement)
	if high.Threshold == nil || *high.Threshold != 80 || high.Summary == "" {
		t.Fatalf("high alert = %+v", high)
	}
	highCommands := h.writer.commands()
	if len(highCommands) != 2 {
		t.Fatalf("high commands = %+v", highCommands)
	}
}

func TestBinarySensorEventAndObservationDeduplicate(t *testing.T) {
	tests := []struct {
		name        string
		role        string
		capability  string
		enable      func(*Config)
		triggerType string
		recoverType string
		entity      string
	}{
		{
			name: "contact", role: ContactRequirement, capability: HallCapability,
			enable:      func(cfg *Config) { cfg.ContactEnabled = true },
			triggerType: HallCloseEvent, recoverType: HallAwayEvent, entity: "hall-1",
		},
		{
			name: "vibration", role: VibrationRequirement, capability: VibrationCapability,
			enable:      func(cfg *Config) { cfg.VibrationEnabled = true },
			triggerType: VibrationQuakeEvent, recoverType: VibrationCapability + "/calm", entity: "vibration-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.enable(&cfg)
			h := newHarness(t, cfg, []application.Binding{
				{RequirementID: tc.role, EntityID: tc.entity},
				{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
				{RequirementID: LightRequirement, EntityID: "led-1"},
			})
			h.arm(t)
			h.writer.reset()

			h.event(t, 1, &application.CapabilityEvent{RequirementID: tc.role, EntityID: tc.entity, EventType: tc.triggerType})
			assertSingleAlertState(t, h.writer.alerts(t), stateTriggered, tc.role)
			triggerEffects := len(h.writer.snapshot())
			if triggerEffects != 3 {
				t.Fatalf("trigger effects = %d, want alert + tone + LED", triggerEffects)
			}

			// The board also reports the same binary state as a property observation.
			// It must not create a second alert or a second actuator command.
			h.observation(t, 2, tc.role, tc.entity, tc.capability, "state", 1, "good")
			if got := len(h.writer.snapshot()); got != triggerEffects {
				t.Fatalf("observation duplicated trigger: effects=%d want=%d", got, triggerEffects)
			}

			h.writer.reset()
			h.event(t, 3, &application.CapabilityEvent{RequirementID: tc.role, EntityID: tc.entity, EventType: tc.recoverType})
			assertSingleAlertState(t, h.writer.alerts(t), stateRecovered, tc.role)
			recoveryCommands := h.writer.commands()
			if len(recoveryCommands) != 1 || recoveryCommands[0].Action != actionLED {
				t.Fatalf("recovery commands = %+v", recoveryCommands)
			}
			assertLedMask(t, recoveryCommands[0], 0)
			recoveryEffects := len(h.writer.snapshot())

			// The inverse property observation is another duplicate source.
			h.observation(t, 4, tc.role, tc.entity, tc.capability, "state", 0, "good")
			if got := len(h.writer.snapshot()); got != recoveryEffects {
				t.Fatalf("observation duplicated recovery: effects=%d want=%d", got, recoveryEffects)
			}
		})
	}
}

func TestRequestCompletedOnlySettlesTerminalMatchingCommand(t *testing.T) {
	h := newHarness(t, DefaultConfig(), []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()
	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	commands := h.writer.commands()
	if len(commands) != 2 {
		t.Fatalf("commands = %+v", commands)
	}
	tone := commandByAction(t, commands, actionTone)
	led := commandByAction(t, commands, actionLED)
	st, err := h.svc.lookup(testInstance, false)
	if err != nil {
		t.Fatal(err)
	}

	for i, state := range []application.CommandState{
		application.CommandStateUnspecified,
		application.CommandStateCreated,
		application.CommandStateDispatched,
		application.CommandStateAccepted,
		application.CommandStateRunning,
	} {
		h.event(t, uint64(i+2), completionFor(tone, state))
		st.mu.Lock()
		if len(st.pending) != 2 || st.lastCommand != nil {
			t.Fatalf("non-terminal state %v changed state: pending=%+v last=%+v", state, st.pending, st.lastCommand)
		}
		st.mu.Unlock()
	}

	h.event(t, 7, &application.RequestCompleted{
		RequestID: tone.IdempotencyKey, EntityID: "other-entity", Action: tone.Action,
		State: application.CommandStateSucceeded,
	})
	h.event(t, 8, &application.RequestCompleted{
		RequestID: tone.IdempotencyKey, EntityID: tone.EntityID, Action: "other-action",
		State: application.CommandStateSucceeded,
	})
	st.mu.Lock()
	if len(st.pending) != 2 || st.lastCommand != nil {
		t.Fatalf("mismatched completion changed state: pending=%+v last=%+v", st.pending, st.lastCommand)
	}
	st.mu.Unlock()

	h.event(t, 9, &application.RequestCompleted{
		RequestID: tone.IdempotencyKey, EntityID: tone.EntityID, Action: tone.Action,
		State: application.CommandStateFailed, ErrorCode: "DEVICE_REJECTED",
	})
	st.mu.Lock()
	if len(st.pending) != 1 || st.lastCommand == nil || st.lastCommand.Action != actionTone || st.lastCommand.State != "failed" || st.lastCommand.ErrorCode != "DEVICE_REJECTED" {
		t.Fatalf("tone failure not settled: pending=%+v last=%+v", st.pending, st.lastCommand)
	}
	st.mu.Unlock()

	h.event(t, 10, &application.RequestCompleted{
		RequestID: led.IdempotencyKey, EntityID: led.EntityID, Action: led.Action,
		State: application.CommandStateTimedOut,
	})
	st.mu.Lock()
	if len(st.pending) != 0 || st.lastCommand == nil || st.lastCommand.Action != actionLED || st.lastCommand.State != "timedout" {
		t.Fatalf("LED timeout not settled: pending=%+v last=%+v", st.pending, st.lastCommand)
	}
	st.mu.Unlock()

	// Replayed terminal completion must not overwrite the current result.
	h.event(t, 11, &application.RequestCompleted{
		RequestID: tone.IdempotencyKey, EntityID: tone.EntityID, Action: tone.Action,
		State: application.CommandStateCancelled,
	})
	st.mu.Lock()
	if st.lastCommand == nil || st.lastCommand.Action != actionLED || st.lastCommand.State != "timedout" {
		t.Fatalf("duplicate completion changed last command: %+v", st.lastCommand)
	}
	st.mu.Unlock()
}

func TestMultipleActiveConditionsRecoverOnlyAfterAllClear(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContactEnabled = true
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: ContactRequirement, EntityID: "hall-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()

	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	h.event(t, 2, &application.CapabilityEvent{RequirementID: ContactRequirement, EntityID: "hall-1", EventType: HallCloseEvent})
	before := len(h.writer.snapshot())

	h.observation(t, 3, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 20, "good")
	if got := len(h.writer.snapshot()); got != before {
		t.Fatalf("partial recovery emitted effects: got=%d before=%d", got, before)
	}

	h.writer.reset()
	h.event(t, 4, &application.CapabilityEvent{RequirementID: ContactRequirement, EntityID: "hall-1", EventType: HallAwayEvent})
	assertSingleAlertState(t, h.writer.alerts(t), stateRecovered, ContactRequirement)
	commands := h.writer.commands()
	if len(commands) != 1 || commands[0].Action != actionLED {
		t.Fatalf("all-clear commands = %+v", commands)
	}
	assertLedMask(t, commands[0], 0)

	h.writer.reset()
	disarm, err := h.svc.RunJob(t.Context(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobDisarm, ArgsJSON: `{}`, IdempotencyKey: "disarm-all-clear",
	})
	if err != nil || !disarm.Status.IsOK() {
		t.Fatalf("disarm: resp=%+v err=%v", disarm, err)
	}
	assertSingleAlertState(t, h.writer.alerts(t), stateDisarmed, "")
}
