package sensoralert

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

const testInstance = "instance-a"

type fakeWriter struct {
	mu      sync.Mutex
	effects []*application.ApplicationEffect
}

func (w *fakeWriter) Send(_ context.Context, effect *application.ApplicationEffect) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.effects = append(w.effects, effect)
	return nil
}

func (w *fakeWriter) snapshot() []*application.ApplicationEffect {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*application.ApplicationEffect(nil), w.effects...)
}

func (w *fakeWriter) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.effects = nil
}

func (w *fakeWriter) commands() []*application.RequestCommand {
	var out []*application.RequestCommand
	for _, effect := range w.snapshot() {
		if command, ok := effect.Union.(*application.RequestCommand); ok {
			out = append(out, command)
		}
	}
	return out
}

func (w *fakeWriter) alerts(t *testing.T) []AlertRecord {
	t.Helper()
	var out []AlertRecord
	for _, effect := range w.snapshot() {
		record, ok := effect.Union.(*application.UpsertDomainRecord)
		if !ok || record.RecordType != alertRecordType {
			continue
		}
		var alert AlertRecord
		if err := json.Unmarshal([]byte(record.DataJSON), &alert); err != nil {
			t.Fatal(err)
		}
		out = append(out, alert)
	}
	return out
}

type harness struct {
	svc    *Service
	st     *instanceState
	writer *fakeWriter
	now    time.Time
}

func newHarness(t *testing.T, cfg Config, bindings []application.Binding) *harness {
	t.Helper()
	svc := New()
	h := &harness{svc: svc, now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	svc.now = func() time.Time { return h.now }
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance, Config: raw, ConfigRevision: 1,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("configure: resp=%+v err=%v", resp, err)
	}
	bindResp, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance, Bindings: bindings,
	})
	if err != nil || !bindResp.Valid {
		t.Fatalf("bindings: resp=%+v err=%v", bindResp, err)
	}
	st, err := svc.lookup(testInstance, false)
	if err != nil {
		t.Fatal(err)
	}
	writer := &fakeWriter{}
	st.mu.Lock()
	st.route = writer
	st.mu.Unlock()
	h.st = st
	h.writer = writer
	return h
}

func (h *harness) arm(t *testing.T) {
	t.Helper()
	resp, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobArm, ArgsJSON: `{}`, IdempotencyKey: "arm-1",
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("arm: resp=%+v err=%v", resp, err)
	}
}

func (h *harness) event(t *testing.T, sequence uint64, union application.ApplicationEventUnion) {
	t.Helper()
	if err := h.svc.handleEvent(context.Background(), &application.ApplicationEvent{
		PluginInstanceID: testInstance, Sequence: sequence, SchemaVersion: application.SchemaVersion, Union: union,
	}); err != nil {
		t.Fatalf("event %d: %v", sequence, err)
	}
}

func (h *harness) observation(t *testing.T, sequence uint64, role, entity, capability, property string, value any, quality string) {
	t.Helper()
	payload := map[string]any{
		"entity_id": entity, "capability": capability, "property": property,
		"value": value, "quality": quality, "observed_at": h.now.Format(time.RFC3339Nano), "sequence": int64(sequence),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	h.event(t, sequence, &application.CapabilityEvent{
		RequirementID: role, EntityID: entity, EventType: PropertyObservedEvent, PayloadJSON: string(raw),
		OccurredAt: h.now.Format(time.RFC3339Nano),
	})
}

func TestArmDisarmAreIdempotent(t *testing.T) {
	h := newHarness(t, DefaultConfig(), []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	first := h.writer.snapshot()
	if len(first) != 1 {
		t.Fatalf("first arm effects = %d, want record only", len(first))
	}
	alerts := h.writer.alerts(t)
	if len(alerts) != 1 || alerts[0].State != stateArmed {
		t.Fatalf("arm alert = %+v", alerts)
	}
	second, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobArm, ArgsJSON: `{}`, IdempotencyKey: "arm-2",
	})
	if err != nil || !second.Status.IsOK() {
		t.Fatalf("second arm: %+v %v", second, err)
	}
	if len(h.writer.snapshot()) != 1 {
		t.Fatal("repeated arm emitted duplicate effects")
	}
	h.writer.reset()
	disarm, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobDisarm, ArgsJSON: `{}`, IdempotencyKey: "disarm-1",
	})
	if err != nil || !disarm.Status.IsOK() {
		t.Fatalf("disarm: %+v %v", disarm, err)
	}
	if len(h.writer.commands()) != 1 || h.writer.commands()[0].Action != actionLED {
		t.Fatalf("disarm commands = %+v", h.writer.commands())
	}
	alerts = h.writer.alerts(t)
	if len(alerts) != 1 || alerts[0].State != stateDisarmed {
		t.Fatalf("disarm alert = %+v", alerts)
	}
	if _, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: jobDisarm, ArgsJSON: `{}`, IdempotencyKey: "disarm-2",
	}); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.snapshot()) != 2 {
		t.Fatal("repeated disarm emitted duplicate effects")
	}
}

func TestTemperatureAlertCooldownAndRecovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CooldownS = 60
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()

	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	if got := h.writer.alerts(t); len(got) != 1 || got[0].State != stateTriggered || got[0].Sensor != TemperatureRequirement {
		t.Fatalf("trigger alert = %+v", got)
	}
	commands := h.writer.commands()
	if len(commands) != 2 || commands[0].Action != actionTone || commands[1].Action != actionLED {
		t.Fatalf("trigger commands = %+v", commands)
	}
	var toneArgs ToneConfig
	if err := json.Unmarshal([]byte(commands[0].ArgsJSON), &toneArgs); err != nil || toneArgs != (ToneConfig{FrequencyHz: 1000, DurationMs: 200}) {
		t.Fatalf("tone args = %s err=%v", commands[0].ArgsJSON, err)
	}
	if commands[0].IdempotencyKey == "" || commands[0].IdempotencyKey == commands[1].IdempotencyKey {
		t.Fatal("commands must have distinct idempotency keys")
	}

	h.writer.reset()
	h.now = h.now.Add(10 * time.Second)
	h.observation(t, 2, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 36, "good")
	if len(h.writer.snapshot()) != 0 {
		t.Fatalf("cooldown emitted effects: %+v", h.writer.snapshot())
	}

	h.now = h.now.Add(10 * time.Second)
	h.observation(t, 3, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 25, "good")
	alerts := h.writer.alerts(t)
	if len(alerts) != 1 || alerts[0].State != stateRecovered {
		t.Fatalf("recovery alert = %+v", alerts)
	}
	commands = h.writer.commands()
	if len(commands) != 1 || commands[0].Action != actionLED {
		t.Fatalf("recovery commands = %+v", commands)
	}
	var ledArgs map[string]int
	if err := json.Unmarshal([]byte(commands[0].ArgsJSON), &ledArgs); err != nil || ledArgs["mask"] != 0 {
		t.Fatalf("recovery led args = %s err=%v", commands[0].ArgsJSON, err)
	}

	h.writer.reset()
	h.now = h.now.Add(20 * time.Second)
	h.observation(t, 4, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	if len(h.writer.snapshot()) != 0 {
		t.Fatal("cooldown was not enforced after recovery")
	}
	h.now = h.now.Add(31 * time.Second)
	h.observation(t, 5, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	if got := h.writer.alerts(t); len(got) != 1 || got[0].State != stateTriggered {
		t.Fatalf("post-cooldown alert = %+v", got)
	}
}

func TestSilentRecordsButDoesNotSound(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Silent = true
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()
	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	if got := h.writer.alerts(t); len(got) != 1 || got[0].State != stateTriggered {
		t.Fatalf("alert = %+v", got)
	}
	for _, command := range h.writer.commands() {
		if command.Action == actionTone {
			t.Fatalf("silent alert emitted tone: %+v", command)
		}
	}
}

func TestContactAndVibrationEvents(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContactEnabled = true
	cfg.VibrationEnabled = true
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: ContactRequirement, EntityID: "hall-1"},
		{RequirementID: VibrationRequirement, EntityID: "vibration-1"},
	})
	h.arm(t)
	h.writer.reset()
	h.event(t, 1, &application.CapabilityEvent{RequirementID: ContactRequirement, EntityID: "hall-1", EventType: HallCloseEvent})
	if got := h.writer.alerts(t); len(got) != 1 || got[0].Sensor != ContactRequirement || got[0].State != stateTriggered {
		t.Fatalf("contact alert = %+v", got)
	}
	h.writer.reset()
	h.event(t, 2, &application.CapabilityEvent{RequirementID: ContactRequirement, EntityID: "hall-1", EventType: HallAwayEvent})
	if got := h.writer.alerts(t); len(got) != 1 || got[0].State != stateRecovered {
		t.Fatalf("contact recovery = %+v", got)
	}
	h.writer.reset()
	h.event(t, 3, &application.CapabilityEvent{RequirementID: VibrationRequirement, EntityID: "vibration-1", EventType: VibrationQuakeEvent})
	if got := h.writer.alerts(t); len(got) != 1 || got[0].Sensor != VibrationRequirement || got[0].State != stateTriggered {
		t.Fatalf("vibration alert = %+v", got)
	}
}

func TestRequestCompletedAndCrossInstanceIsolation(t *testing.T) {
	cfg := DefaultConfig()
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: SoundRequirement, EntityID: "buzzer-1"},
	})
	h.arm(t)
	h.writer.reset()
	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	commands := h.writer.commands()
	if len(commands) != 1 {
		t.Fatalf("commands = %+v", commands)
	}
	h.event(t, 2, &application.RequestCompleted{
		RequestID: commands[0].IdempotencyKey, EntityID: "other-entity", Action: commands[0].Action,
		State: application.CommandStateSucceeded,
	})
	st, _ := h.svc.lookup(testInstance, false)
	st.mu.Lock()
	if st.lastCommand != nil || len(st.pending) != 1 {
		t.Fatalf("mismatched completion changed state: last=%+v pending=%+v", st.lastCommand, st.pending)
	}
	st.mu.Unlock()
	h.event(t, 3, &application.RequestCompleted{
		RequestID: commands[0].IdempotencyKey, EntityID: commands[0].EntityID, Action: commands[0].Action,
		State: application.CommandStateSucceeded,
	})
	st.mu.Lock()
	if st.lastCommand == nil || st.lastCommand.State != "succeeded" || len(st.pending) != 0 {
		t.Fatalf("completion not handled: last=%+v pending=%+v", st.lastCommand, st.pending)
	}
	st.mu.Unlock()

	// A second instance keeps independent state and route.
	raw, _ := json.Marshal(cfg)
	_, err := h.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: "instance-b", Config: raw, ConfigRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: "instance-b", Bindings: []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "temperature-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stB, _ := h.svc.lookup("instance-b", false)
	stB.mu.Lock()
	if stB.armed || stB.state != stateDisarmed || len(stB.active) != 0 {
		t.Fatalf("instance b inherited state: %+v", stB)
	}
	stB.mu.Unlock()
}

func TestStatusAndFreshnessJobs(t *testing.T) {
	h := newHarness(t, DefaultConfig(), []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "temperature-1"}})
	status, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobStatus, ArgsJSON: `{}`})
	if err != nil || !status.Status.IsOK() {
		t.Fatalf("status: %+v %v", status, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(status.ResultJSON), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["state"] != stateDisarmed {
		t.Fatalf("status result = %s", status.ResultJSON)
	}
	fresh, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobCheckFreshness, ArgsJSON: `{}`})
	if err != nil || !fresh.Status.IsOK() {
		t.Fatalf("freshness: %+v %v", fresh, err)
	}
	var report struct {
		Sensors []freshnessSensor `json:"sensors"`
	}
	if err := json.Unmarshal([]byte(fresh.ResultJSON), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Sensors) != 1 || report.Sensors[0].Fresh {
		t.Fatalf("freshness report = %+v", report)
	}
}

func TestOptionalBindingsAndUnknownRequirement(t *testing.T) {
	svc := New()
	resp, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: "only-temp", Bindings: []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "t"}}})
	if err != nil || !resp.Valid {
		t.Fatalf("optional binding: %+v %v", resp, err)
	}
	st, _ := svc.lookup("only-temp", false)
	st.mu.Lock()
	if !st.bindingsValid {
		t.Fatal("empty optional binding set was not marked valid")
	}
	st.mu.Unlock()
	bad, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: "bad", Bindings: []application.Binding{{RequirementID: "unknown", EntityID: "x"}}})
	if err != nil || bad.Valid || len(bad.Issues) == 0 {
		t.Fatalf("unknown requirement: %+v %v", bad, err)
	}
}

func TestSecondEffectStreamForSameInstanceIsRejected(t *testing.T) {
	svc := New()
	first := &fakeWriter{}
	second := &fakeWriter{}
	ownerA := new(int)
	ownerB := new(int)
	if err := svc.attachRoute(testInstance, first, ownerA); err != nil {
		t.Fatal(err)
	}
	if err := svc.attachRoute(testInstance, second, ownerB); err == nil {
		t.Fatal("second effect stream for the same instance id was accepted")
	}
	if err := svc.attachRoute(testInstance, first, ownerA); err != nil {
		t.Fatalf("same owner reattach failed: %v", err)
	}
}

func TestHandleEventsEndsAtEOF(t *testing.T) {
	svc := New()
	writer := &fakeWriter{}
	err := svc.HandleEvents(context.Background(), &sliceReader{}, writer)
	if err != nil {
		t.Fatal(err)
	}
}

type sliceReader struct{}

func (*sliceReader) Recv(context.Context) (*application.ApplicationEvent, error) { return nil, io.EOF }
func TestConfigUpdatePreservesActiveAlertUntilRecovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CooldownS = 0
	h := newHarness(t, cfg, []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "temperature-1"},
		{RequirementID: LightRequirement, EntityID: "led-1"},
	})
	h.arm(t)
	h.writer.reset()
	h.observation(t, 1, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 35, "good")
	if got := h.writer.alerts(t); len(got) != 1 || got[0].State != stateTriggered {
		t.Fatalf("trigger alert = %+v", got)
	}

	h.writer.reset()
	next := cfg
	next.TemperatureMax = 40
	raw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance, Config: raw, ConfigRevision: 2,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("config update: resp=%+v err=%v", resp, err)
	}
	st, err := h.svc.lookup(testInstance, false)
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	if st.state != stateTriggered || len(st.active) != 1 {
		t.Fatalf("config update lost active alert: state=%s active=%+v", st.state, st.active)
	}
	st.mu.Unlock()
	if effects := h.writer.snapshot(); len(effects) != 0 {
		t.Fatalf("config update emitted unexpected effects: %+v", effects)
	}

	h.now = h.now.Add(time.Second)
	h.observation(t, 2, TemperatureRequirement, "temperature-1", TemperatureCapability, "value", 30, "good")
	recovered := assertSingleAlertState(t, h.writer.alerts(t), stateRecovered, TemperatureRequirement)
	if recovered.RecoveredAt == nil || recovered.Value == nil || *recovered.Value != 30 {
		t.Fatalf("recovered after config update = %+v", recovered)
	}
	commands := h.writer.commands()
	if len(commands) != 1 || commands[0].Action != actionLED {
		t.Fatalf("recovery commands after config update = %+v", commands)
	}
	assertLedMask(t, commands[0], 0)
	st.mu.Lock()
	if len(st.active) != 0 {
		t.Fatalf("active alerts not cleared after recovery: %+v", st.active)
	}
	st.mu.Unlock()
}
