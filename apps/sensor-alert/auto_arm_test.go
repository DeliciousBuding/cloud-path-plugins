// SPDX-License-Identifier: Apache-2.0

package sensoralert

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

// configureWithAutoArm applies a configuration without requiring an effect
// stream, mirroring the host flow where ConfigureInstance can precede
// HandleEvents.
func configureWithAutoArm(t *testing.T, svc *Service, cfg Config, revision uint32) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance, Config: raw, ConfigRevision: revision,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("configure rev %d: resp=%+v err=%v", revision, resp, err)
	}
}

func validateTemperatureBinding(t *testing.T, svc *Service) {
	t.Helper()
	resp, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings:         []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "temperature-1"}},
	})
	if err != nil || !resp.Valid {
		t.Fatalf("validate bindings: resp=%+v err=%v", resp, err)
	}
}

// attachRouteAndFlush simulates the first event of an instance, which attaches
// the effect stream and flushes any deferred auto-arm record.
func attachRouteAndFlush(t *testing.T, svc *Service, writer *fakeWriter) {
	t.Helper()
	if err := svc.attachRoute(testInstance, writer, new(int)); err != nil {
		t.Fatal(err)
	}
	if err := svc.handleEvent(context.Background(), &application.ApplicationEvent{
		PluginInstanceID: testInstance, Sequence: 1, SchemaVersion: application.SchemaVersion,
		Union: &application.RequestCompleted{RequestID: "no-such-request"},
	}); err != nil {
		t.Fatal(err)
	}
}

func runJobSummary(t *testing.T, svc *Service, jobID, idempotencyKey string) map[string]any {
	t.Helper()
	resp, err := svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobID, ArgsJSON: `{}`, IdempotencyKey: idempotencyKey,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("job %s: resp=%+v err=%v", jobID, resp, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(resp.ResultJSON), &parsed); err != nil {
		t.Fatalf("job %s result %s: %v", jobID, resp.ResultJSON, err)
	}
	return parsed
}

func TestAutoArmDefaultsToArmedAndFlushesRecordOnFirstEvent(t *testing.T) {
	svc := New()
	writer := &fakeWriter{}
	configureWithAutoArm(t, svc, DefaultConfig(), 1)
	validateTemperatureBinding(t, svc)

	st, err := svc.lookup(testInstance, false)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	armed, state, alertState, pending := st.armed, st.state, st.alert.State, st.armRecordPending
	st.mu.Unlock()
	if !armed || state != stateArmed || alertState != stateArmed {
		t.Fatalf("auto-arm did not arm: armed=%v state=%s alert=%s", armed, state, alertState)
	}
	if !pending {
		t.Fatal("arm record should be pending until an effect stream is attached")
	}
	if effects := writer.snapshot(); len(effects) != 0 {
		t.Fatalf("configure without a route emitted effects: %+v", effects)
	}

	attachRouteAndFlush(t, svc, writer)
	alerts := writer.alerts(t)
	if len(alerts) != 1 || alerts[0].State != stateArmed {
		t.Fatalf("deferred arm record = %+v", alerts)
	}
	st.mu.Lock()
	pending = st.armRecordPending
	st.mu.Unlock()
	if pending {
		t.Fatal("arm record was not cleared after flush")
	}

	// A second event must not duplicate the arm record.
	if err := svc.handleEvent(context.Background(), &application.ApplicationEvent{
		PluginInstanceID: testInstance, Sequence: 2, SchemaVersion: application.SchemaVersion,
		Union: &application.RequestCompleted{RequestID: "another-request"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := writer.alerts(t); len(got) != 1 {
		t.Fatalf("arm record duplicated: %+v", got)
	}
}

func TestAutoArmDisabledKeepsDisarmedSemantics(t *testing.T) {
	svc := New()
	cfg := DefaultConfig()
	cfg.AutoArm = false
	configureWithAutoArm(t, svc, cfg, 1)
	validateTemperatureBinding(t, svc)

	status := runJobSummary(t, svc, jobStatus, "status-1")
	if status["state"] != stateDisarmed || status["armed"] != false {
		t.Fatalf("auto_arm=false status = %+v", status)
	}
}

func TestAutoArmIsIdempotentAcrossReconfigure(t *testing.T) {
	svc := New()
	writer := &fakeWriter{}
	cfg := DefaultConfig()
	configureWithAutoArm(t, svc, cfg, 1)
	validateTemperatureBinding(t, svc)
	attachRouteAndFlush(t, svc, writer)
	if alerts := writer.alerts(t); len(alerts) != 1 || alerts[0].State != stateArmed {
		t.Fatalf("auto-arm record = %+v", alerts)
	}

	writer.reset()
	// Same revision and content: the whole configure block is skipped.
	configureWithAutoArm(t, svc, cfg, 1)
	if effects := writer.snapshot(); len(effects) != 0 {
		t.Fatalf("same-revision configure emitted effects: %+v", effects)
	}

	// A real config change keeps the armed state without re-emitting.
	next := cfg
	next.TemperatureMax = 30
	configureWithAutoArm(t, svc, next, 2)
	if effects := writer.snapshot(); len(effects) != 0 {
		t.Fatalf("config change re-emitted the arm record: %+v", effects)
	}
	st, err := svc.lookup(testInstance, false)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	armed, state := st.armed, st.state
	st.mu.Unlock()
	if !armed || state != stateArmed {
		t.Fatalf("state after reconfigure: armed=%v state=%s", armed, state)
	}
}

func TestArmDisarmStatusResultsCarryLocalizableState(t *testing.T) {
	h := newHarness(t, DefaultConfig(), []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
	})

	// The console already localizes `state` (armed/disarmed). The app must not
	// ship its own prose, or the English UI would render Chinese text.
	armed := runJobSummary(t, h.svc, jobArm, "arm-summary")
	if armed["state"] != stateArmed || armed["armed"] != true {
		t.Fatalf("arm result = %+v", armed)
	}

	status := runJobSummary(t, h.svc, jobStatus, "status-armed")
	if status["state"] != stateArmed {
		t.Fatalf("armed status result = %+v", status)
	}

	disarmed := runJobSummary(t, h.svc, jobDisarm, "disarm-summary")
	if disarmed["state"] != stateDisarmed || disarmed["armed"] != false {
		t.Fatalf("disarm result = %+v", disarmed)
	}

	status = runJobSummary(t, h.svc, jobStatus, "status-disarmed")
	if status["state"] != stateDisarmed {
		t.Fatalf("disarmed status result = %+v", status)
	}

	// Replay of the arm idempotency key still reports the armed state.
	replayed := runJobSummary(t, h.svc, jobArm, "arm-summary")
	if replayed["state"] != stateArmed {
		t.Fatalf("replayed arm result = %+v", replayed)
	}
}
