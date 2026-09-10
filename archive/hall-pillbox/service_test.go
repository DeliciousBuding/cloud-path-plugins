// SPDX-License-Identifier: Apache-2.0

package hallpillbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const testInstance = "pillbox-1"

const (
	hallEntity    = "dev/hall"
	keyEntity     = "dev/key1"
	buzzerEntity  = "dev/buzzer"
	displayEntity = "dev/display"
)

var testClock = time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)

type effectCapture struct {
	mu      sync.Mutex
	effects []*application.ApplicationEffect
	calls   int
	failAt  int
}

func (c *effectCapture) Send(_ context.Context, effect *application.ApplicationEffect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.failAt == c.calls {
		return status.Errorf(status.CodeUnavailable, "injected effect failure")
	}
	c.effects = append(c.effects, effect)
	return nil
}

func (c *effectCapture) take() []*application.ApplicationEffect {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.effects
	c.effects = nil
	return out
}

func (c *effectCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.effects)
}

type fakeEventReader struct {
	ch chan *application.ApplicationEvent
}

func (r *fakeEventReader) Recv(ctx context.Context) (*application.ApplicationEvent, error) {
	select {
	case event, ok := <-r.ch:
		if !ok {
			return nil, io.EOF
		}
		return event, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fixture struct {
	svc      *Service
	sink     *effectCapture
	clock    atomic.Value
	sequence uint64
}

func testConfig(withDisplay bool) Config {
	cfg := Config{
		Timezone:    "UTC",
		Compartment: "pillbox",
		Schedule:    []WindowSpec{{ID: "morning", Start: "08:00", End: "08:30"}},
		Reminder:    &Reminder{Freq: 4, Duration: 3},
	}
	if withDisplay {
		cfg.Display = &DisplayPolicy{
			ReminderArgs: json.RawMessage(`{"digits":[0,0,0,0,0,0,0,1]}`),
			MissedArgs:   json.RawMessage(`{"digits":[0,0,0,0,0,0,0,2]}`),
			IdleArgs:     json.RawMessage(`{"mode":"clock"}`),
		}
	}
	return cfg
}

func testBindings(withDisplay, withConfirm bool) []application.Binding {
	out := []application.Binding{
		{RequirementID: openingRequirement, EntityID: hallEntity},
		{RequirementID: reminderOutputRequirement, EntityID: buzzerEntity},
	}
	if withConfirm {
		out = append(out, application.Binding{RequirementID: confirmRequirement, EntityID: keyEntity})
	}
	if withDisplay {
		out = append(out, application.Binding{RequirementID: localDisplayRequirement, EntityID: displayEntity})
	}
	return out
}

func newFixture(t *testing.T, withDisplay bool) *fixture {
	t.Helper()
	f := &fixture{svc: New(), sink: &effectCapture{}}
	f.clock.Store(testClock)
	f.svc.now = func() time.Time { return f.clock.Load().(time.Time) }

	cfg, err := json.Marshal(testConfig(withDisplay))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance,
		Config:           cfg,
		ConfigRevision:   1,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("configure: %+v, %v", resp, err)
	}
	bound, err := f.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings:         testBindings(withDisplay, true),
	})
	if err != nil || !bound.Valid {
		t.Fatalf("bindings: %+v, %v", bound, err)
	}
	f.svc.writers = map[string]application.ApplicationEffectWriter{testInstance: f.sink}
	return f
}

func (f *fixture) now() time.Time {
	return f.clock.Load().(time.Time)
}

func (f *fixture) advance(d time.Duration) {
	f.clock.Store(f.now().Add(d))
}

func (f *fixture) job(jobID, args, key string) (*application.RunJobResponse, error) {
	return f.svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance,
		JobID:            jobID,
		ArgsJSON:         args,
		IdempotencyKey:   key,
	})
}

func (f *fixture) mustJob(t *testing.T, jobID, args, key string) map[string]any {
	t.Helper()
	resp, err := f.job(jobID, args, key)
	if err != nil || resp == nil || !resp.Status.IsOK() {
		t.Fatalf("job %s: %+v, %v", jobID, resp, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp.ResultJSON), &out); err != nil {
		t.Fatalf("job %s result: %v", jobID, err)
	}
	return out
}

func (f *fixture) event(t *testing.T, union application.ApplicationEventUnion) {
	t.Helper()
	f.sequence++
	if err := f.svc.handleEvent(&application.ApplicationEvent{
		PluginInstanceID: testInstance,
		Sequence:         f.sequence,
		SchemaVersion:    application.SchemaVersion,
		Union:            union,
	}); err != nil {
		t.Fatalf("event: %v", err)
	}
}

func (f *fixture) window(t *testing.T, id string) windowTrack {
	t.Helper()
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	w := f.svc.instance(testInstance).windows[id]
	if w == nil {
		t.Fatalf("missing window %q", id)
	}
	return *w
}

func requireErrorCode(t *testing.T, err error, code status.Code) {
	t.Helper()
	var st *status.Status
	if !errors.As(err, &st) || st.Code != code {
		t.Fatalf("error = %v, want status code %v", err, code)
	}
}

func recordsOf(effects []*application.ApplicationEffect) []*application.UpsertDomainRecord {
	var out []*application.UpsertDomainRecord
	for _, effect := range effects {
		if record, ok := effect.Union.(*application.UpsertDomainRecord); ok && record.RecordType == "window" {
			out = append(out, record)
		}
	}
	return out
}

func commandsOf(effects []*application.ApplicationEffect) []*application.RequestCommand {
	var out []*application.RequestCommand
	for _, effect := range effects {
		if command, ok := effect.Union.(*application.RequestCommand); ok {
			out = append(out, command)
		}
	}
	return out
}

func recordData(t *testing.T, record *application.UpsertDomainRecord) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(record.DataJSON), &data); err != nil {
		t.Fatalf("record data: %v", err)
	}
	return data
}

func commandWithKey(effects []*application.ApplicationEffect, key string) *application.RequestCommand {
	for _, command := range commandsOf(effects) {
		if command.IdempotencyKey == key {
			return command
		}
	}
	return nil
}

func assertCommandKeys(t *testing.T, effects []*application.ApplicationEffect) {
	t.Helper()
	seen := map[string]bool{}
	for _, command := range commandsOf(effects) {
		if command.IdempotencyKey == "" {
			t.Fatalf("device command %s/%s has empty idempotency key", command.EntityID, command.Action)
		}
		if seen[command.IdempotencyKey] {
			t.Fatalf("duplicate idempotency key %q in one effect batch", command.IdempotencyKey)
		}
		seen[command.IdempotencyKey] = true
	}
}

func TestDescriptorAndManifestIdentity(t *testing.T) {
	desc, err := New().Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.ApplicationID != pluginIDValue || desc.Version != pluginVersion || desc.DeclarativeOnly {
		t.Fatalf("descriptor = %+v", desc)
	}
	if len(desc.Requirements) != 4 {
		t.Fatalf("requirements = %d, want 4", len(desc.Requirements))
	}
	wantRequirements := map[string]struct {
		cap  string
		card string
		min  uint32
	}{
		openingRequirement:        {hallCap, "one", 0},
		confirmRequirement:        {keyCap, "zero-or-one", 0},
		reminderOutputRequirement: {buzzerCap, "one", 0},
		localDisplayRequirement:   {displayCap, "zero-or-one", 0},
	}
	for _, got := range desc.Requirements {
		want, ok := wantRequirements[got.ID]
		if !ok || got.Capability != want.cap || got.Cardinality != want.card || got.MinItems != want.min {
			t.Fatalf("requirement %+v, want %+v", got, want)
		}
		delete(wantRequirements, got.ID)
	}
	if len(wantRequirements) != 0 {
		t.Fatalf("missing requirements: %v", wantRequirements)
	}
	if len(desc.Jobs) != 4 {
		t.Fatalf("jobs = %d, want 4", len(desc.Jobs))
	}
	wantJobs := map[string]bool{
		jobStartWindow: true, jobConfirmWindow: true, jobCheckWindow: false, jobStatus: true,
	}
	for _, job := range desc.Jobs {
		wantManual, ok := wantJobs[job.ID]
		if !ok || job.ManualOnly != wantManual {
			t.Fatalf("job %+v, want manual_only=%t", job, wantManual)
		}
		delete(wantJobs, job.ID)
	}
	if len(wantJobs) != 0 {
		t.Fatalf("missing jobs: %v", wantJobs)
	}

	manifest, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifestText := strings.ReplaceAll(string(manifest), "\r\n", "\n")
	for _, want := range []string{
		"kind: Application",
		"id: " + pluginIDValue,
		"version: " + pluginVersion,
		"entrypoint: cloud-path-app-hall-pillbox",
		"- id: hall-pillbox",
		"capability: " + hallCap,
		"capability: " + keyCap,
		"capability: " + buzzerCap,
		"capability: " + displayCap,
		"ui:",
		"apiVersion: 1",
		"title: 霍尔药盒",
		"route: hall-pillbox",
		"visibility: always",
		"type: metrics",
		"recordType: window",
		"type: schedule",
		"source: jobs",
		"title: 自动提醒计划",
		"type: actions",
		"source: manual-jobs",
		"type: records",
		"presentation: timeline",
		"type: form",
		"source: config",
		"fields:",
		"emptyText:",
		"format: time",
		"hideWhenEmpty: true",
		"key: state",
		"key: compartment",
		"key: opened_at",
		"key: confirmed_at",
		"key: missed_at",
		"key: app_config.timezone",
		"label: 药格标识",
		"key: app_config.compartment",
		"key: app_config.schedule",
		"type: array",
		"minItems: 1",
		"itemFields:",
		"label: 计划 ID",
		"label: 药格标识（可选）",
		"label: 开始时间",
		"placeholder: \"08:00\"",
		"label: 结束时间",
		"placeholder: \"08:30\"",
		"key: app_config.reminder.freq",
		"key: app_config.reminder.duration",
	} {
		if !strings.Contains(manifestText, want) {
			t.Fatalf("plugin.yaml missing %q", want)
		}
	}
	mirror, err := os.ReadFile("requirements.yaml")
	if err != nil {
		t.Fatal(err)
	}
	scheduleStart := strings.Index(manifestText, "              - type: schedule")
	actionsStart := -1
	if scheduleStart >= 0 {
		actionsStart = strings.Index(manifestText[scheduleStart:], "              - type: actions")
	}
	if scheduleStart < 0 || actionsStart < 0 {
		t.Fatal("schedule/actions section boundary missing")
	}
	scheduleBlock := manifestText[scheduleStart : scheduleStart+actionsStart]
	if !strings.Contains(scheduleBlock, "\n                source: jobs") || strings.Contains(scheduleBlock, "\n                recordType:") {
		t.Fatal("schedule section must use source: jobs and no recordType")
	}
	for _, want := range []string{hallCap, keyCap, buzzerCap, displayCap} {
		if !strings.Contains(string(mirror), want) {
			t.Fatalf("requirements.yaml missing %q", want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if err := testConfig(true).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"missing timezone":    func(c *Config) { c.Timezone = "" },
		"bad timezone":        func(c *Config) { c.Timezone = "not/a-zone" },
		"missing compartment": func(c *Config) { c.Compartment = "" },
		"missing schedule":    func(c *Config) { c.Schedule = nil },
		"bad clock":           func(c *Config) { c.Schedule[0].Start = "8:00" },
		"cross midnight":      func(c *Config) { c.Schedule[0].Start = "23:50"; c.Schedule[0].End = "00:10" },
		"missing reminder":    func(c *Config) { c.Reminder = nil },
		"bad reminder":        func(c *Config) { c.Reminder.Freq = 10 },
		"bad display args":    func(c *Config) { c.Display.ReminderArgs = json.RawMessage(`[]`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(true)
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestBindingCardinality(t *testing.T) {
	f := newFixture(t, true)
	for _, tc := range []struct {
		name string
		in   []application.Binding
		ok   bool
	}{
		{"valid", testBindings(true, true), true},
		{"missing opening", testBindings(true, true)[1:], false},
		{"missing reminder", append(testBindings(true, true)[:1], testBindings(true, true)[2:]...), false},
		{"too many confirm", append(testBindings(true, true), application.Binding{RequirementID: confirmRequirement, EntityID: "dev/key2"}), false},
		{"too many display", append(testBindings(true, true), application.Binding{RequirementID: localDisplayRequirement, EntityID: "dev/display2"}), false},
		{"unknown", append(testBindings(true, true), application.Binding{RequirementID: "driver:vendor", EntityID: "dev/x"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
				PluginInstanceID: testInstance,
				Bindings:         tc.in,
			})
			if err != nil || resp.Valid != tc.ok {
				t.Fatalf("valid=%t want %t: %+v, %v", resp.Valid, tc.ok, resp, err)
			}
		})
	}
}

func TestStartWindowEffectsAndHallOpenConfirmation(t *testing.T) {
	f := newFixture(t, true)
	result := f.mustJob(t, jobStartWindow, `{"window_id":"trial-1","minutes":10}`, "start-1")
	if result["state"] != windowOpened || result["reminder_state"] != reminderPending {
		t.Fatalf("start result = %+v", result)
	}
	startEffects := f.sink.take()
	assertCommandKeys(t, startEffects)
	if len(recordsOf(startEffects)) != 1 {
		t.Fatalf("start effects = %+v", startEffects)
	}
	startRecord := recordData(t, recordsOf(startEffects)[0])
	for _, field := range []string{"id", "state", "reminder_state", "opened_at", "closed_at", "confirmation_source", "schedule_id", "compartment"} {
		if _, ok := startRecord[field]; !ok {
			t.Fatalf("record missing %q: %+v", field, startRecord)
		}
	}
	if startRecord["state"] != windowOpened || startRecord["reminder_state"] != reminderPending || startRecord["display_state"] != reminderPending {
		t.Fatalf("start record = %+v", startRecord)
	}
	if commandWithKey(startEffects, reminderStartPrefix+"trial-1") == nil {
		t.Fatalf("missing buzzer start command: %+v", commandsOf(startEffects))
	}
	if commandWithKey(startEffects, displayReminderPref+"trial-1") == nil {
		t.Fatalf("missing display reminder command: %+v", commandsOf(startEffects))
	}

	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallAwayEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	confirmEffects := f.sink.take()
	assertCommandKeys(t, confirmEffects)
	confirmRecord := recordData(t, recordsOf(confirmEffects)[0])
	if confirmRecord["state"] != windowCompleted || confirmRecord["confirmation_source"] != sourceHall {
		t.Fatalf("confirm record = %+v", confirmRecord)
	}
	if confirmRecord["closed_at"] == "" || confirmRecord["reminder_stop_state"] != reminderPending || confirmRecord["display_state"] != reminderPending {
		t.Fatalf("confirm side effects missing: %+v", confirmRecord)
	}
	if commandWithKey(confirmEffects, reminderStopPrefix+"trial-1") == nil {
		t.Fatalf("missing buzzer stop command: %+v", commandsOf(confirmEffects))
	}
	if commandWithKey(confirmEffects, displayIdlePref+"trial-1") == nil {
		t.Fatalf("missing display idle command: %+v", commandsOf(confirmEffects))
	}
	if f.window(t, "trial-1").State != windowCompleted {
		t.Fatal("window did not complete")
	}
}

func TestSilentReminderSuppressesBuzzerAndKeepsDisplay(t *testing.T) {
	f := newFixture(t, true)
	cfg := testConfig(true)
	cfg.Reminder = &Reminder{Freq: 0, Duration: 0}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance,
		Config:           raw,
		ConfigRevision:   2,
	})
	if err != nil || resp == nil || !resp.Status.IsOK() {
		t.Fatalf("configure silent reminder: %+v, %v", resp, err)
	}

	result := f.mustJob(t, jobStartWindow, `{"window_id":"silent-1","minutes":10}`, "silent-start")
	if result["state"] != windowOpened || result["reminder_state"] != reminderSuppressed {
		t.Fatalf("silent start result = %+v", result)
	}
	if requestID, ok := result["reminder_request_id"]; ok && fmt.Sprint(requestID) != "" {
		t.Fatalf("silent reminder requested a buzzer: %+v", result)
	}
	startEffects := f.sink.take()
	assertCommandKeys(t, startEffects)
	if commandWithKey(startEffects, reminderStartPrefix+"silent-1") != nil {
		t.Fatalf("silent reminder emitted buzzer start: %+v", commandsOf(startEffects))
	}
	if commandWithKey(startEffects, displayReminderPref+"silent-1") == nil {
		t.Fatalf("silent reminder missing display reminder: %+v", commandsOf(startEffects))
	}
	startRecord := recordData(t, recordsOf(startEffects)[0])
	if startRecord["reminder_state"] != reminderSuppressed || startRecord["reminder_request_id"] != "" || startRecord["reminder_stop_state"] != reminderSuppressed {
		t.Fatalf("silent start record = %+v", startRecord)
	}

	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallAwayEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	confirmEffects := f.sink.take()
	assertCommandKeys(t, confirmEffects)
	if commandWithKey(confirmEffects, reminderStopPrefix+"silent-1") != nil {
		t.Fatalf("silent reminder emitted buzzer stop: %+v", commandsOf(confirmEffects))
	}
	if commandWithKey(confirmEffects, displayIdlePref+"silent-1") == nil {
		t.Fatalf("silent confirmation missing display idle: %+v", commandsOf(confirmEffects))
	}
	confirmRecord := recordData(t, recordsOf(confirmEffects)[0])
	if confirmRecord["state"] != windowCompleted || confirmRecord["confirmation_source"] != sourceHall || confirmRecord["reminder_stop_state"] != reminderSuppressed {
		t.Fatalf("silent confirm record = %+v", confirmRecord)
	}
}

func TestHallCloseIgnoredThenAwayConfirms(t *testing.T) {
	f := newFixture(t, false)
	f.mustJob(t, jobStartWindow, `{"window_id":"close-1","minutes":10}`, "close-start")
	f.sink.take()
	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallCloseEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	if effects := f.sink.take(); len(effects) != 0 {
		t.Fatalf("hall close emitted confirmation effects: %+v", effects)
	}
	if f.window(t, "close-1").State != windowOpened {
		t.Fatal("hall close must not confirm an opened window")
	}

	f.advance(10 * time.Second)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallAwayEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	record := recordData(t, recordsOf(f.sink.take())[0])
	if record["state"] != windowCompleted || record["confirmation_source"] != sourceHall {
		t.Fatalf("away after close did not confirm: %+v", record)
	}
}

func TestHallOpenEventConfirms(t *testing.T) {
	f := newFixture(t, false)
	f.mustJob(t, jobStartWindow, `{"window_id":"open-1","minutes":10}`, "open-start")
	f.sink.take()
	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallOpenEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})

	effects := f.sink.take()
	record := recordData(t, recordsOf(effects)[0])
	if record["state"] != windowCompleted || record["confirmation_source"] != sourceHall {
		t.Fatalf("hall open confirmation = %+v", record)
	}
	if f.window(t, "open-1").State != windowCompleted {
		t.Fatal("hall open event must confirm an opened window")
	}
}

func TestHallCloseDoesNotLateConfirmMissedWindow(t *testing.T) {
	f := newFixture(t, false)
	f.mustJob(t, jobStartWindow, `{"window_id":"close-missed-1","minutes":1}`, "close-missed-start")
	f.sink.take()
	f.advance(2 * time.Minute)
	f.mustJob(t, jobCheckWindow, `{"window_id":"close-missed-1"}`, "close-missed-check")
	f.sink.take()

	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallCloseEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	if effects := f.sink.take(); len(effects) != 0 {
		t.Fatalf("hall close emitted late-confirmation effects: %+v", effects)
	}
	if state := f.window(t, "close-missed-1").State; state != windowMissed {
		t.Fatalf("hall close changed missed window to %q", state)
	}
}

func TestHallOpeningEventsRequireBoundOpeningEntity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requirementID string
		entityID      string
		eventType     string
	}{
		{name: "wrong requirement", requirementID: confirmRequirement, entityID: hallEntity, eventType: hallAwayEvent},
		{name: "wrong entity", requirementID: openingRequirement, entityID: "dev/other-hall", eventType: hallOpenEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, false)
			f.mustJob(t, jobStartWindow, `{"window_id":"bound-1","minutes":10}`, "bound-start")
			f.sink.take()
			f.advance(time.Minute)
			f.event(t, &application.CapabilityEvent{
				RequirementID: tc.requirementID,
				EntityID:      tc.entityID,
				EventType:     tc.eventType,
				OccurredAt:    f.now().Format(time.RFC3339Nano),
			})
			if effects := f.sink.take(); len(effects) != 0 {
				t.Fatalf("unbound opening event emitted effects: %+v", effects)
			}
			if state := f.window(t, "bound-1").State; state != windowOpened {
				t.Fatalf("unbound opening event changed state to %q", state)
			}
		})
	}
}

func TestKeyFallbackAndDashboardConfirmation(t *testing.T) {
	f := newFixture(t, false)
	f.mustJob(t, jobStartWindow, `{"window_id":"key-1","minutes":10}`, "key-start")
	f.sink.take()
	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: confirmRequirement,
		EntityID:      keyEntity,
		EventType:     keyPressEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	keyRecord := recordData(t, recordsOf(f.sink.take())[0])
	if keyRecord["state"] != windowCompleted || keyRecord["confirmation_source"] != sourceKey {
		t.Fatalf("key confirmation = %+v", keyRecord)
	}

	f.mustJob(t, jobStartWindow, `{"window_id":"dashboard-1","minutes":10}`, "dashboard-start")
	f.sink.take()
	f.mustJob(t, jobConfirmWindow, `{"window_id":"dashboard-1","source":"dashboard"}`, "dashboard-confirm")
	dashboardRecord := recordData(t, recordsOf(f.sink.take())[0])
	if dashboardRecord["state"] != windowCompleted || dashboardRecord["confirmation_source"] != sourceDashboard {
		t.Fatalf("dashboard confirmation = %+v", dashboardRecord)
	}
	if _, err := f.job(jobConfirmWindow, `{"window_id":"dashboard-1","source":"key"}`, "bad-source"); err == nil {
		t.Fatal("non-dashboard source accepted")
	}
}

func TestMissedAndLateConfirmation(t *testing.T) {
	f := newFixture(t, true)
	f.mustJob(t, jobStartWindow, `{"window_id":"late-1","minutes":1}`, "late-start")
	f.sink.take()
	f.advance(2 * time.Minute)
	f.mustJob(t, jobCheckWindow, `{"window_id":"late-1"}`, "check-1")
	missedEffects := f.sink.take()
	missedRecord := recordData(t, recordsOf(missedEffects)[0])
	if missedRecord["state"] != windowMissed || missedRecord["missed_at"] == "" {
		t.Fatalf("missed record = %+v", missedRecord)
	}
	if commandWithKey(missedEffects, displayMissedPref+"late-1") == nil {
		t.Fatalf("missing missed display command: %+v", commandsOf(missedEffects))
	}

	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallAwayEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	lateEffects := f.sink.take()
	lateRecord := recordData(t, recordsOf(lateEffects)[0])
	if lateRecord["state"] != windowCompletedLate || lateRecord["confirmation_source"] != sourceHall || lateRecord["missed_at"] == "" {
		t.Fatalf("late confirmation = %+v", lateRecord)
	}
	if commandWithKey(lateEffects, reminderStopPrefix+"late-1") == nil || commandWithKey(lateEffects, displayIdlePref+"late-1") == nil {
		t.Fatalf("late confirmation missing stop/idle: %+v", commandsOf(lateEffects))
	}
}

func TestRequestCompletedUpdatesReminderAndDisplay(t *testing.T) {
	f := newFixture(t, true)
	f.mustJob(t, jobStartWindow, `{"window_id":"ack-1","minutes":10}`, "ack-start")
	startEffects := f.sink.take()
	buzzer := commandWithKey(startEffects, reminderStartPrefix+"ack-1")
	display := commandWithKey(startEffects, displayReminderPref+"ack-1")
	if buzzer == nil || display == nil {
		t.Fatal("start commands missing")
	}

	f.event(t, &application.RequestCompleted{
		RequestID:  buzzer.IdempotencyKey,
		EntityID:   buzzer.EntityID,
		Action:     buzzer.Action,
		State:      application.CommandStateFailed,
		ResultJSON: `{"device":"err"}`,
		ErrorCode:  "badarg",
	})
	buzzerRecord := recordData(t, recordsOf(f.sink.take())[0])
	if buzzerRecord["reminder_state"] != "failed" || buzzerRecord["reminder_error_code"] != "badarg" {
		t.Fatalf("buzzer receipt not recorded: %+v", buzzerRecord)
	}

	f.event(t, &application.RequestCompleted{
		RequestID:  display.IdempotencyKey,
		EntityID:   display.EntityID,
		Action:     display.Action,
		State:      application.CommandStateSucceeded,
		ResultJSON: `{"ok":true}`,
	})
	displayRecord := recordData(t, recordsOf(f.sink.take())[0])
	if displayRecord["display_state"] != "succeeded" || displayRecord["display_done_at"] == "" {
		t.Fatalf("display receipt not recorded: %+v", displayRecord)
	}

	f.advance(time.Minute)
	f.event(t, &application.CapabilityEvent{
		RequirementID: openingRequirement,
		EntityID:      hallEntity,
		EventType:     hallOpenEvent,
		OccurredAt:    f.now().Format(time.RFC3339Nano),
	})
	confirmEffects := f.sink.take()
	stop := commandWithKey(confirmEffects, reminderStopPrefix+"ack-1")
	idle := commandWithKey(confirmEffects, displayIdlePref+"ack-1")
	if stop == nil || idle == nil {
		t.Fatal("confirm commands missing")
	}
	f.event(t, &application.RequestCompleted{
		RequestID: stop.IdempotencyKey,
		EntityID:  stop.EntityID,
		Action:    stop.Action,
		State:     application.CommandStateSucceeded,
	})
	stopRecord := recordData(t, recordsOf(f.sink.take())[0])
	if stopRecord["reminder_stop_state"] != "succeeded" {
		t.Fatalf("stop receipt not recorded: %+v", stopRecord)
	}
	f.event(t, &application.RequestCompleted{
		RequestID: idle.IdempotencyKey,
		EntityID:  idle.EntityID,
		Action:    idle.Action,
		State:     application.CommandStateTimedOut,
	})
	idleRecord := recordData(t, recordsOf(f.sink.take())[0])
	if idleRecord["display_state"] != "timedout" {
		t.Fatalf("idle display receipt not recorded: %+v", idleRecord)
	}
}

func TestCheckWindowAndJobIdempotency(t *testing.T) {
	f := newFixture(t, false)
	f.mustJob(t, jobStartWindow, `{"window_id":"idem-1","minutes":1}`, "idem-start")
	f.sink.take()
	f.advance(2 * time.Minute)

	first := f.mustJob(t, jobCheckWindow, `{"window_id":"idem-1"}`, "check-idem")
	f.sink.take()
	second := f.mustJob(t, jobCheckWindow, `{"window_id":"idem-1"}`, "check-idem")
	if fmt.Sprint(first["missed"]) != fmt.Sprint(second["missed"]) {
		t.Fatalf("idempotent check result changed: %v vs %v", first, second)
	}
	if f.sink.count() != 0 {
		t.Fatal("idempotent check emitted duplicate effects")
	}

	if _, err := f.job(jobStartWindow, `{"window_id":"idem-1","minutes":2}`, "idem-start"); err == nil {
		t.Fatal("same window id with different duration was accepted")
	}
	requireErrorCode(t, func() error {
		_, err := f.job(jobStartWindow, `{"window_id":"other","minutes":1}`, "idem-start")
		return err
	}(), status.CodeInvalidArgument)
}

func TestCheckWindowOpensDueScheduleWithoutScheduleTick(t *testing.T) {
	f := newFixture(t, false)
	id := scheduleOccurrenceID("morning", f.now())

	result := f.mustJob(t, jobCheckWindow, `{}`, "auto-check-1")
	effects := f.sink.take()
	w := f.window(t, id)
	if w.State != windowOpened || w.Source != sourceSchedule || w.ScheduleID != "morning" || !w.OpenedAt.Equal(f.now()) {
		t.Fatalf("due window = %+v", w)
	}
	if fmt.Sprint(result["missed"]) != "[]" {
		t.Fatalf("new due window reported as missed: %+v", result)
	}
	if commandWithKey(effects, reminderStartPrefix+id) == nil {
		t.Fatalf("scheduled buzzer command missing: %+v", commandsOf(effects))
	}

	f.mustJob(t, jobCheckWindow, `{}`, "auto-check-2")
	if f.sink.count() != 0 {
		t.Fatal("second automatic check emitted duplicate effects")
	}
}

func TestScheduleTickUsesConfiguredSingleCompartment(t *testing.T) {
	f := newFixture(t, false)
	f.event(t, &application.ScheduleTick{
		ScheduleID: "window-morning",
		OccurredAt: f.now().Format(time.RFC3339),
		WindowJSON: `{"id":"morning","compartment":"","start":"2026-09-08T08:00:00Z","end":"2026-09-08T08:30:00Z"}`,
	})
	effects := f.sink.take()
	record := recordData(t, recordsOf(effects)[0])
	if record["state"] != windowOpened || record["compartment"] != "pillbox" || record["schedule_id"] != "morning" {
		t.Fatalf("schedule record = %+v", record)
	}
	if commandWithKey(effects, reminderStartPrefix+scheduleOccurrenceID("morning", f.now())) == nil {
		t.Fatalf("scheduled buzzer command missing: %+v", commandsOf(effects))
	}
}

func TestHandleEventsUsesFakeReaderAndWriter(t *testing.T) {
	f := newFixture(t, true)
	reader := &fakeEventReader{ch: make(chan *application.ApplicationEvent, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.svc.HandleEvents(ctx, reader, f.sink) }()

	reader.ch <- &application.ApplicationEvent{
		PluginInstanceID: testInstance,
		Sequence:         1,
		SchemaVersion:    application.SchemaVersion,
		Union: &application.ScheduleTick{
			ScheduleID: "window-morning",
			OccurredAt: f.now().Format(time.RFC3339),
			WindowJSON: `{"id":"morning","start":"2026-09-08T08:00:00Z","end":"2026-09-08T08:30:00Z"}`,
		},
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.sink.count() == 0 {
		t.Fatal("fake reader/writer did not emit effects")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("HandleEvents: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleEvents did not stop")
	}
}
