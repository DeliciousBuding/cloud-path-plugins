package scheduledcompartment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

// All effects stay in memory; no socket, device, filesystem or buzzer is used.
type effectCapture struct {
	mu      sync.Mutex
	effects []*application.ApplicationEffect
	calls   int
	failAt  int
}

func (c *effectCapture) Send(_ context.Context, e *application.ApplicationEffect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.failAt == c.calls {
		return status.Errorf(status.CodeUnavailable, "injected stream failure")
	}
	c.effects = append(c.effects, e)
	return nil
}

func (c *effectCapture) take() []*application.ApplicationEffect {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.effects
	c.effects = nil
	return out
}

type practicalApp struct {
	svc      *Service
	sink     *effectCapture
	clock    atomic.Value
	sequence uint64
}

func practicalConfig(n int) Config {
	// Audible by default in tests: the silent path is asserted separately
	// (TestExplicitSilentReminderAndWindowStatus).
	cfg := Config{Timezone: "UTC", Reminder: &Reminder{Freq: 1, Duration: 1}, Schedule: []WindowSpec{{ID: "morning", Compartment: "c1", Start: "08:00", End: "08:30"}}}
	for i := 1; i <= n; i++ {
		cfg.Compartments = append(cfg.Compartments, Compartment{ID: fmt.Sprintf("c%d", i)})
	}
	return cfg
}

func practicalBindings(n int) []application.Binding {
	out := []application.Binding{{RequirementID: "reminder-output", EntityID: buzzerEntityID}}
	for i := 1; i <= n; i++ {
		out = append(out, application.Binding{RequirementID: "compartments", EntityID: fmt.Sprintf("dev/key-%d", i)})
	}
	return out
}

func newPracticalApp(t *testing.T, n int) *practicalApp {
	t.Helper()
	p := &practicalApp{svc: New(), sink: &effectCapture{}}
	p.clock.Store(time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC))
	p.svc.now = func() time.Time { return p.clock.Load().(time.Time) }
	cfg, err := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(practicalConfig(n))), ConfigRevision: 1})
	if err != nil || !cfg.Status.IsOK() {
		t.Fatalf("configure: %+v, %v", cfg, err)
	}
	bound, err := p.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: practicalBindings(n)})
	if err != nil || !bound.Valid {
		t.Fatalf("bindings: %+v, %v", bound, err)
	}
	p.svc.writers = map[string]application.ApplicationEffectWriter{testInstance: p.sink}
	return p
}

func (p *practicalApp) now() time.Time          { return p.clock.Load().(time.Time) }
func (p *practicalApp) advance(d time.Duration) { p.clock.Store(p.now().Add(d)) }

func (p *practicalApp) job(jobID, args, key string) (*application.RunJobResponse, error) {
	return p.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobID, ArgsJSON: args, IdempotencyKey: key})
}

func configureWithoutSchedule(t *testing.T, p *practicalApp) {
	t.Helper()
	cfg := practicalConfig(1)
	cfg.Schedule = []WindowSpec{{ID: "later", Compartment: "c1", Start: "09:00", End: "09:30"}}
	resp, err := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("configure without schedule: %+v, %v", resp, err)
	}
}

func (p *practicalApp) start(t *testing.T, id, comp string, minutes int) map[string]any {
	t.Helper()
	resp, err := p.job(jobStartReminder, mustJSON(startReminderArgs{CompartmentID: comp, Minutes: minutes, WindowID: id}), id)
	return requireJobResult(t, resp, err)
}

func requireJobResult(t *testing.T, resp *application.RunJobResponse, err error) map[string]any {
	t.Helper()
	if err != nil || resp == nil || !resp.Status.IsOK() {
		t.Fatalf("job failed: %+v, %v", resp, err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp.ResultJSON), &result); err != nil {
		t.Fatalf("invalid result: %v", err)
	}
	return result
}

func requireErrorCode(t *testing.T, err error, code status.Code) {
	t.Helper()
	var st *status.Status
	if !errors.As(err, &st) || st.Code != code {
		t.Fatalf("error=%v, want %v", err, code)
	}
}

func (p *practicalApp) event(t *testing.T, u application.ApplicationEventUnion) {
	t.Helper()
	p.sequence++
	if err := p.svc.handleEvent(&application.ApplicationEvent{PluginInstanceID: testInstance, Sequence: p.sequence, Union: u}); err != nil {
		t.Fatalf("event: %v", err)
	}
}

func (p *practicalApp) press(t *testing.T, entity string, at time.Time) {
	t.Helper()
	p.event(t, &application.CapabilityEvent{RequirementID: "compartments", EntityID: entity, EventType: keyPressEvent, OccurredAt: at.Format(time.RFC3339)})
}

func (p *practicalApp) window(t *testing.T, id string) windowTrack {
	t.Helper()
	p.svc.mu.Lock()
	defer p.svc.mu.Unlock()
	w := p.svc.instance(testInstance).windows[id]
	if w == nil {
		t.Fatalf("missing window %q", id)
	}
	return *w
}

func TestSingleCompartmentAndExactBindingCounts(t *testing.T) {
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			p := newPracticalApp(t, n)
			for _, count := range []int{0, 1, 2, 3, 4} {
				resp, err := p.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: practicalBindings(count)})
				if err != nil || resp.Valid != (count == n) {
					t.Fatalf("config=%d bindings=%d: %+v %v", n, count, resp, err)
				}
			}
			other := 1
			if n == 1 {
				other = 3
			}
			resp, err := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(practicalConfig(other))), ConfigRevision: 2})
			if err != nil || resp.Status.IsOK() || resp.AppliedRevision != 0 {
				t.Fatalf("count-changing config accepted: %+v %v", resp, err)
			}
			if st := p.svc.instance(testInstance); len(st.config.Compartments) != n || st.configRev != 1 {
				t.Fatal("rejected configure mutated previous config")
			}
		})
	}
	bare := New()
	resp, err := bare.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: practicalBindings(1)})
	if err != nil || !resp.Valid {
		t.Fatalf("structural pre-config validation rejected: %+v %v", resp, err)
	}
	if bare.instance(testInstance).config != nil {
		t.Fatal("pre-config validation must not fabricate application settings")
	}
}

func TestBindingOrderControlsOnlyItsConfiguredCompartment(t *testing.T) {
	p := newPracticalApp(t, 3)
	cfg := practicalConfig(3)
	cfg.Compartments = []Compartment{{ID: "c3"}, {ID: "c1"}, {ID: "c2"}}
	resp, _ := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2})
	if !resp.Status.IsOK() {
		t.Fatal(resp.Status)
	}
	// Neither the config IDs nor the entity IDs are lexically sorted. Other
	// requirement bindings are deliberately interleaved.
	bindings := []application.Binding{
		{RequirementID: "compartments", EntityID: c2},
		{RequirementID: "reminder-output", EntityID: buzzerEntityID},
		{RequirementID: "compartments", EntityID: c3},
		{RequirementID: "local-display", EntityID: "dev/display"},
		{RequirementID: "compartments", EntityID: c1},
	}
	bound, _ := p.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: bindings})
	if !bound.Valid {
		t.Fatal(bound.Issues)
	}
	for _, comp := range []string{"c3", "c1", "c2"} {
		p.start(t, "manual-"+comp, comp, 2)
	}
	p.sink.take()
	p.press(t, c2, p.now())
	if p.window(t, "manual-c3").State != windowCompleted || p.window(t, "manual-c1").State != windowOpened || p.window(t, "manual-c2").State != windowOpened {
		t.Fatal("key entity order did not map to config order")
	}
	p.press(t, c3, p.now())
	if p.window(t, "manual-c1").State != windowCompleted || p.window(t, "manual-c2").State != windowOpened {
		t.Fatal("second key confirmed wrong compartment")
	}
	p.press(t, c1, p.now())
	if p.window(t, "manual-c2").State != windowCompleted {
		t.Fatal("third key did not confirm its compartment")
	}
}

func TestNoHotRemappingOfOpenWindows(t *testing.T) {
	p := newPracticalApp(t, 3)
	p.start(t, "active", "c1", 2)
	cfg := practicalConfig(3)
	cfg.Compartments[0], cfg.Compartments[1] = cfg.Compartments[1], cfg.Compartments[0]
	resp, _ := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2})
	if resp.Status.IsOK() {
		t.Fatal("open-window config remapping accepted")
	}
	bindings := practicalBindings(3)
	bindings[1], bindings[2] = bindings[2], bindings[1]
	result, _ := p.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: bindings})
	if result.Valid {
		t.Fatal("open-window entity remapping accepted")
	}
	p.press(t, c1, p.now())
	if p.window(t, "active").State != windowCompleted {
		t.Fatal("rejection damaged the original binding")
	}
}

func TestManualStartIsRealPendingAndIdempotent(t *testing.T) {
	p := newPracticalApp(t, 1)
	result := p.start(t, "trial-1", "c1", 2)
	for key, want := range map[string]any{"window_id": "trial-1", "state": windowOpened, "source": sourceManual, "reminder_state": "pending", "reminder_request_id": "reminder-trial-1", "effects_status": "submitted"} {
		if result[key] != want {
			t.Fatalf("result[%s]=%v, want %v", key, result[key], want)
		}
	}
	effects := p.sink.take()
	if len(effects) != 3 || countRequestCommand(effects) != 1 || windowStateOf(effects, "trial-1") != windowOpened {
		t.Fatalf("not a real start batch: %+v", effects)
	}
	command, ok := effects[1].Union.(*application.RequestCommand)
	if !ok || command.EntityID != buzzerEntityID || command.IdempotencyKey != "reminder-trial-1" || command.Deadline != p.now().Add(2*time.Minute).Format(time.RFC3339) {
		t.Fatalf("wrong command: %+v", command)
	}
	var policy Reminder
	if err := json.Unmarshal([]byte(command.ArgsJSON), &policy); err != nil || policy != (Reminder{Freq: 1, Duration: 1}) {
		t.Fatalf("configured audible policy must be emitted as-is: %+v %v", policy, err)
	}
	task, ok := effects[2].Union.(*application.ScheduleTask)
	if !ok || task.ScheduleID != windowTaskID("trial-1") || task.Cron != windowCheckCron {
		t.Fatalf("missing real check task: %+v", task)
	}
	first := p.window(t, "trial-1")
	p.advance(30 * time.Second)
	repeat := p.start(t, "trial-1", "c1", 2)
	if !reflect.DeepEqual(result, repeat) {
		t.Fatalf("same key changed replay result: %v vs %v", result, repeat)
	}
	// The stable window ID also deduplicates a fresh HTTP idempotency key.
	resp, err := p.job(jobStartReminder, `{"minutes":2,"window_id":"trial-1","compartment_id":"c1"}`, "new-http-key")
	requireJobResult(t, resp, err)
	if p.window(t, "trial-1").End != first.End || len(p.sink.take()) != 0 {
		t.Fatal("retry extended or re-emitted a reminder")
	}
	_, err = p.job(jobStartReminder, `{"minutes":3,"window_id":"trial-1","compartment_id":"c1"}`, "new-http-key")
	requireErrorCode(t, err, status.CodeInvalidArgument)
	_, err = p.job(jobStartReminder, `{"minutes":3,"window_id":"trial-1","compartment_id":"c1"}`, "another-key")
	requireErrorCode(t, err, status.CodeInvalidArgument)
}

func TestManualArgsFailClosed(t *testing.T) {
	p := newPracticalApp(t, 1)
	args := []string{
		`{}`, `null`, `[]`, `{`,
		`{"compartment_id":"c1","minutes":0,"window_id":"x"}`,
		`{"compartment_id":"c1","minutes":121,"window_id":"x"}`,
		`{"compartment_id":"c1","minutes":1.5,"window_id":"x"}`,
		`{"compartment_id":"c1","minutes":"2","window_id":"x"}`,
		`{"compartment_id":"c1","minutes":null,"window_id":"x"}`,
		`{"compartment_id":"unknown","minutes":1,"window_id":"x"}`,
		`{"compartment_id":" c1","minutes":1,"window_id":"x"}`,
		`{"compartment_id":"c1","minutes":1,"window_id":" x"}`,
		`{"compartment_id":"c1","minutes":1,"window_id":"schedule:reserved"}`,
		`{"compartment_id":"c1","minutes":1,"window_id":"x","source":"schedule"}`,
		`{"compartment_id":"c1","minutes":1,"window_id":"x"} {}`,
		mustJSON(startReminderArgs{CompartmentID: "c1", Minutes: 1, WindowID: strings.Repeat("x", 129)}),
		mustJSON(startReminderArgs{CompartmentID: "c1", Minutes: 1, WindowID: strings.Repeat("药", 100)}),
	}
	for _, raw := range args {
		_, err := p.job(jobStartReminder, raw, "")
		requireErrorCode(t, err, status.CodeInvalidArgument)
	}
	if len(p.svc.instance(testInstance).windows) != 0 || len(p.sink.take()) != 0 {
		t.Fatal("invalid arguments had business effects")
	}
	for _, raw := range []string{`{}`, `{"window_id":null}`, `{"window_id":" x"}`, `{"window_id":"x","compartment_id":"c1"}`} {
		_, err := p.job(jobConfirmWindow, raw, "")
		requireErrorCode(t, err, status.CodeInvalidArgument)
	}
	p.start(t, "maximum", "c1", 120)
	if w := p.window(t, "maximum"); w.End.Sub(w.Start) != 120*time.Minute {
		t.Fatal("inclusive upper duration bound rejected")
	}
}

func TestDashboardConfirmationCannotAcknowledgeANewerWindow(t *testing.T) {
	p := newPracticalApp(t, 1)
	p.start(t, "old", "c1", 1)
	p.advance(2 * time.Minute)
	p.start(t, "new", "c1", 2)
	p.sink.take()
	resp, err := p.job(jobConfirmWindow, `{"window_id":"old"}`, "confirm-old")
	result := requireJobResult(t, resp, err)
	if result["state"] != windowCompletedLate || result["confirmation_source"] != "dashboard" || result["reminder_state"] != "pending" {
		t.Fatalf("untruthful confirmation: %v", result)
	}
	if p.window(t, "new").State != windowOpened {
		t.Fatal("old ID confirmed newer window")
	}
	if !hasCancelTask(p.sink.take(), windowTaskID("old")) {
		t.Fatal("did not cancel the exact old window task")
	}
	before := p.window(t, "old")
	p.advance(10 * time.Second)
	resp, err = p.job(jobConfirmWindow, `{"window_id":"old"}`, "fresh-retry")
	requireJobResult(t, resp, err)
	if p.window(t, "old").ConfirmedAt != before.ConfirmedAt || len(p.sink.take()) != 0 {
		t.Fatal("repeated explicit confirmation rewrote history")
	}
	_, err = p.job(jobConfirmWindow, `{"window_id":"new"}`, "confirm-old")
	requireErrorCode(t, err, status.CodeInvalidArgument)
	_, err = p.job(jobConfirmWindow, `{"window_id":"missing"}`, "confirm-missing")
	requireErrorCode(t, err, status.CodeNotFound)
	if p.window(t, "new").State != windowOpened || len(p.sink.take()) != 0 {
		t.Fatal("invalid confirmation affected a window")
	}
	// Idempotency keys are scoped by Job, not shared across different actions.
	resp, err = p.job(jobConfirmWindow, `{"window_id":"new"}`, "new")
	result = requireJobResult(t, resp, err)
	if result["state"] != windowCompleted {
		t.Fatalf("cross-job key collision: %v", result)
	}
}

func TestExpiryBoundaryAndLatePhysicalConfirmation(t *testing.T) {
	for _, checkFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(checkFirst), func(t *testing.T) {
			p := newPracticalApp(t, 1)
			configureWithoutSchedule(t, p)
			p.start(t, "one-minute", "c1", 1)
			p.sink.take()
			p.advance(time.Minute - time.Second)
			resp, err := p.job(jobWindowCheck, `{}`, "before-end")
			result := requireJobResult(t, resp, err)
			if len(result["missed"].([]any)) != 0 || len(p.sink.take()) != 0 {
				t.Fatal("window expired before its deadline")
			}
			p.advance(time.Second)
			if checkFirst {
				resp, err = p.job(jobWindowCheck, `{}`, "at-end")
				requireJobResult(t, resp, err)
				effects := p.sink.take()
				if windowStateOf(effects, "one-minute") != windowMissed || !hasNotification(effects) {
					t.Fatal("automatic check did not record missed confirmation")
				}
			}
			p.press(t, c1, p.now())
			w := p.window(t, "one-minute")
			if w.State != windowCompletedLate || w.ConfirmationSource != "key" || w.MissedAt.IsZero() || w.ReminderState != "pending" {
				t.Fatalf("deadline key was not a late user confirmation: %+v", w)
			}
		})
	}
}

func TestKeyRoutingAndDelayedEventsAreDeterministic(t *testing.T) {
	p := newPracticalApp(t, 1)
	p.start(t, "older", "c1", 1)
	oldPress := p.now().Add(30 * time.Second)
	p.advance(2 * time.Minute)
	p.start(t, "newer", "c1", 2)
	p.sink.take()
	p.press(t, c2, p.now()) // reserved for a different application
	p.event(t, &application.CapabilityEvent{RequirementID: "another-requirement", EntityID: c1, EventType: keyPressEvent, OccurredAt: p.now().Format(time.RFC3339)})
	p.event(t, &application.CapabilityEvent{RequirementID: "compartments", EntityID: c1, EventType: keyPressEvent, OccurredAt: "invalid"})
	p.press(t, c1, p.now().Add(time.Hour))
	if len(p.sink.take()) != 0 {
		t.Fatal("unbound, wrong-requirement, invalid or future key was applied")
	}
	p.press(t, c1, oldPress)
	if p.window(t, "older").State != windowCompleted || p.window(t, "newer").State != windowOpened {
		t.Fatal("delayed event targeted a window that did not exist at press time")
	}
	p.sink.take()
	p.press(t, c1, p.now())
	if p.window(t, "newer").State != windowCompleted {
		t.Fatal("current press did not target latest window")
	}
	p.sink.take()
	p.press(t, c1, p.now())
	if len(p.sink.take()) != 0 {
		t.Fatal("duplicate press fell back to an older window")
	}
}

func scheduleTick(id, comp string, start time.Time, duration time.Duration) *application.ScheduleTick {
	return &application.ScheduleTick{ScheduleID: "window-" + id, OccurredAt: start.Format(time.RFC3339), WindowJSON: mustJSON(map[string]string{"id": id, "compartment": comp, "start": start.Format(time.RFC3339), "end": start.Add(duration).Format(time.RFC3339)})}
}

func TestAutomaticWindowCheckOpensDueDailyWindow(t *testing.T) {
	p := newPracticalApp(t, 1)

	resp, err := p.job(jobWindowCheck, `{}`, "minute-open")
	requireJobResult(t, resp, err)
	effects := p.sink.take()
	id := scheduleOccurrenceID("morning", p.now())
	if p.window(t, id).State != windowOpened || countRequestCommand(effects) != 1 {
		t.Fatalf("automatic window-check did not open due schedule: %+v", effects)
	}

	resp, err = p.job(jobWindowCheck, `{}`, "minute-repeat")
	requireJobResult(t, resp, err)
	if len(p.sink.take()) != 0 {
		t.Fatal("automatic window-check replayed an already-open occurrence")
	}
}

func TestDailyScheduleUsesIndependentStableOccurrences(t *testing.T) {
	p := newPracticalApp(t, 1)
	first := p.now()
	tick := scheduleTick("morning", "c1", first, 30*time.Minute)
	p.event(t, tick)
	firstID := scheduleOccurrenceID("morning", first)
	effects := p.sink.take()
	if countRequestCommand(effects) != 1 || p.window(t, firstID).Source != sourceSchedule {
		t.Fatal("schedule did not use real reminder path")
	}
	p.event(t, tick)
	if len(p.sink.take()) != 0 {
		t.Fatal("same occurrence was emitted twice")
	}
	p.press(t, c1, p.now())
	p.sink.take()
	p.advance(24 * time.Hour)
	p.event(t, scheduleTick("morning", "c1", p.now(), 30*time.Minute))
	secondID := scheduleOccurrenceID("morning", p.now())
	if secondID == firstID || countRequestCommand(p.sink.take()) != 1 || p.window(t, secondID).State != windowOpened {
		t.Fatal("yesterday's ID suppressed today's reminder")
	}
	resp, err := p.job(jobConfirmWindow, mustJSON(windowArgs{WindowID: firstID}), "old-day")
	requireJobResult(t, resp, err)
	if p.window(t, secondID).State != windowOpened || len(p.sink.take()) != 0 {
		t.Fatal("yesterday's confirmation affected today's occurrence")
	}
}

func TestExpiredAndFutureScheduleTicksNeverRequestAStaleReminder(t *testing.T) {
	p := newPracticalApp(t, 1)
	p.event(t, scheduleTick("future", "c1", p.now().Add(time.Hour), time.Minute))
	p.event(t, &application.ScheduleTick{WindowJSON: "malformed"})
	if len(p.svc.instance(testInstance).windows) != 0 || len(p.sink.take()) != 0 {
		t.Fatal("invalid/future schedule created a window")
	}
	old := p.now().Add(-time.Hour)
	p.event(t, scheduleTick("stale", "c1", old, time.Minute))
	effects := p.sink.take()
	w := p.window(t, scheduleOccurrenceID("stale", old))
	if countRequestCommand(effects) != 0 || w.State != windowMissed || w.ReminderState != "not_requested" || reminderRequestID(&w) != "" || !w.OpenedAt.IsZero() {
		t.Fatalf("stale schedule pretended to send a reminder: %+v", w)
	}
}

func TestScheduledTaskCallbackRoutesToWindowCheck(t *testing.T) {
	p := newPracticalApp(t, 1)
	p.start(t, "task-window", "c1", 1)
	p.sink.take()
	p.advance(time.Minute)
	resp, err := p.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: testInstance, JobID: windowTaskID("task-window"), JobType: "scheduled", ArgsJSON: mustJSON(windowArgs{WindowID: "task-window"}), IdempotencyKey: "task-dispatch"})
	requireJobResult(t, resp, err)
	if p.window(t, "task-window").State != windowMissed {
		t.Fatal("actual Host scheduled callback was ignored")
	}
	p.sink.take()
	resp, err = p.job(jobWindowCheck, `{}`, "minute-loop")
	requireJobResult(t, resp, err)
	if len(p.sink.take()) != 0 {
		t.Fatal("automatic and per-window checks duplicated expiry effects")
	}
	_, err = p.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: testInstance, JobID: windowTaskID("other"), JobType: "scheduled", ArgsJSON: mustJSON(windowArgs{WindowID: "task-window"})})
	requireErrorCode(t, err, status.CodeInvalidArgument)
}

func TestOnlyCorrelatedTerminalReceiptsSetReminderOutcome(t *testing.T) {
	terminal := map[application.CommandState]string{application.CommandStateSucceeded: "succeeded", application.CommandStateFailed: "failed", application.CommandStateTimedOut: "timedout", application.CommandStateCancelled: "cancelled"}
	for state, want := range terminal {
		t.Run(want, func(t *testing.T) {
			p := newPracticalApp(t, 1)
			p.start(t, "receipt", "c1", 1)
			p.sink.take()
			receipt := application.RequestCompleted{RequestID: "reminder-receipt", EntityID: buzzerEntityID, Action: buzzerAction, State: application.CommandStateRunning}
			p.event(t, &receipt)
			wrong := receipt
			wrong.State = state
			wrong.EntityID = "another-output"
			p.event(t, &wrong)
			wrong.EntityID = buzzerEntityID
			wrong.Action = "other-action"
			p.event(t, &wrong)
			if p.window(t, "receipt").ReminderState != "pending" || len(p.sink.take()) != 0 {
				t.Fatal("pending or unrelated receipt was called success")
			}
			receipt.State = state
			receipt.ResultJSON = "opaque outcome detail"
			receipt.ErrorCode = "example-outcome"
			p.advance(time.Second)
			p.event(t, &receipt)
			w := p.window(t, "receipt")
			if w.State != windowOpened || w.ReminderState != want || w.ReminderResult != receipt.ResultJSON || w.ReminderErrorCode != receipt.ErrorCode || w.ReminderDoneAt.IsZero() {
				t.Fatalf("wrong terminal record: %+v", w)
			}
			p.sink.take()
			p.advance(time.Second)
			p.event(t, &receipt)
			receipt.State = application.CommandStateFailed
			receipt.ResultJSON = "conflicting later outcome"
			p.event(t, &receipt)
			if after := p.window(t, "receipt"); after.ReminderDoneAt != w.ReminderDoneAt || after.ReminderResult != w.ReminderResult || len(p.sink.take()) != 0 {
				t.Fatal("terminal replay rewrote the settled outcome")
			}
		})
	}
}

func TestMissingStreamAndPartialDeliveryNeverReturnFakeSuccess(t *testing.T) {
	p := newPracticalApp(t, 1)
	delete(p.svc.writers, testInstance)
	_, err := p.job(jobStartReminder, `{"compartment_id":"c1","minutes":1,"window_id":"no-stream"}`, "no-stream")
	requireErrorCode(t, err, status.CodeUnavailable)
	if st := p.svc.instance(testInstance); len(st.windows) != 0 || len(st.jobs) != 0 {
		t.Fatal("missing stream mutated or cached a successful operation")
	}
	p.svc.writers[testInstance] = p.sink
	p.sink.failAt = 2 // record sent; command send fails
	_, err = p.job(jobStartReminder, `{"compartment_id":"c1","minutes":1,"window_id":"partial"}`, "partial")
	requireErrorCode(t, err, status.CodeUnavailable)
	effects := p.sink.take()
	if len(effects) != 1 || p.window(t, "partial").ReminderState != "pending" || len(p.svc.instance(testInstance).jobs) != 0 {
		t.Fatal("partial batch was cached as success")
	}
	p.sink.failAt = 0
	_, err = p.job(jobStartReminder, `{"compartment_id":"c1","minutes":1,"window_id":"partial"}`, "partial")
	requireErrorCode(t, err, status.CodeUnavailable)
	if len(p.sink.take()) != 0 {
		t.Fatal("uncertain batch was blindly retried")
	}
}

func TestRestartDoesNotInventWindowRecovery(t *testing.T) {
	original := newPracticalApp(t, 1)
	original.start(t, "before-restart", "c1", 1)
	restarted := newPracticalApp(t, 1)
	_, err := restarted.job(jobConfirmWindow, `{"window_id":"before-restart"}`, "after-restart")
	requireErrorCode(t, err, status.CodeNotFound)
	if len(restarted.sink.take()) != 0 {
		t.Fatal("restart fabricated a window confirmation")
	}
}

type blockingCapture struct {
	sink    *effectCapture
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingCapture) Send(ctx context.Context, e *application.ApplicationEffect) error {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.sink.Send(ctx, e)
}

func TestConcurrentStartAndConfirmKeepEffectBatchesOrdered(t *testing.T) {
	p := newPracticalApp(t, 1)
	block := &blockingCapture{sink: p.sink, entered: make(chan struct{}), release: make(chan struct{})}
	p.svc.writers[testInstance] = block
	startDone := make(chan error, 1)
	confirmDone := make(chan error, 1)
	go func() {
		_, err := p.job(jobStartReminder, `{"compartment_id":"c1","minutes":1,"window_id":"concurrent"}`, "start")
		startDone <- err
	}()
	select {
	case <-block.entered:
	case <-time.After(5 * time.Second):
		close(block.release)
		t.Fatal("start never emitted a record")
	}
	go func() { _, err := p.job(jobConfirmWindow, `{"window_id":"concurrent"}`, "confirm"); confirmDone <- err }()
	close(block.release)
	for _, done := range []chan error{startDone, confirmDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent job deadlock")
		}
	}
	effects := p.sink.take()
	if len(effects) != 5 || windowStateOf(effects[:3], "concurrent") != windowOpened || windowStateOf(effects[3:], "concurrent") != windowCompleted {
		t.Fatalf("stale start record overwrote the confirmation: %+v", effects)
	}
}

func TestManualJobsAcrossPublicRPCWire(t *testing.T) {
	a := newTestApp(t, time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC))
	defer a.close()
	if r := a.configure(mustJSON(practicalConfig(1))); !r.Status.IsOK() {
		t.Fatal(r.Status)
	}
	if r := a.validate(practicalBindings(1)); !r.Valid {
		t.Fatal(r.Issues)
	}
	a.openStream()
	a.send(1, &application.InstanceLifecycle{State: "running"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.svc.mu.Lock()
		ready := a.svc.writers[testInstance] != nil
		a.svc.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("instance stream not registered")
		}
		time.Sleep(time.Millisecond)
	}
	resp, err := a.cli.RunJob(a.ctx, &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobStartReminder, ArgsJSON: `{"compartment_id":"c1","minutes":1,"window_id":"wire-window"}`, IdempotencyKey: "wire-start"})
	result := requireJobResult(t, resp, err)
	if result["reminder_state"] != "pending" || countRequestCommand(a.waitEffects(3, 20*time.Millisecond)) != 1 {
		t.Fatalf("wire job did not request real reminder: %v", result)
	}
	resp, err = a.cli.RunJob(a.ctx, &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobConfirmWindow, ArgsJSON: `{"window_id":"wire-window"}`, IdempotencyKey: "wire-confirm"})
	result = requireJobResult(t, resp, err)
	if result["state"] != windowCompleted || windowStateOf(a.waitEffects(2, 20*time.Millisecond), "wire-window") != windowCompleted {
		t.Fatal("wire confirmation did not execute real business path")
	}
}

func TestScheduleTimesAndCompartmentIDsMatchHostContract(t *testing.T) {
	for _, clock := range []string{"8:00", "08:0", "08:00 ", " 08:00"} {
		cfg := practicalConfig(1)
		cfg.Schedule[0].Start = clock
		if cfg.Validate() == nil {
			t.Fatalf("accepted noncanonical schedule time %q", clock)
		}
	}
	cfg := practicalConfig(1)
	cfg.Compartments[0].ID = " c1"
	if cfg.Validate() == nil {
		t.Fatal("accepted ID that would not map to its binding")
	}
}

func TestExplicitSilentReminderAndWindowStatus(t *testing.T) {
	p := newPracticalApp(t, 1)
	cfg := practicalConfig(1)
	cfg.Reminder = &Reminder{Freq: 0, Duration: 0}
	resp, _ := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2})
	if !resp.Status.IsOK() {
		t.Fatal(resp.Status)
	}
	result := p.start(t, "silent", "c1", 1)
	effects := p.sink.take()
	// Silent policy: window record + durable check task and NO buzzer command.
	// The reference firmware rejects freq=0 with badarg; emitting a doomed
	// command is not honesty, it is a manufactured failed receipt.
	if countRequestCommand(effects) != 0 {
		t.Fatal("silent policy still emitted a buzzer command")
	}
	if result["reminder_state"] != "suppressed" || result["reminder_request_id"] != "" {
		t.Fatalf("silent result misreported: %v", result)
	}
	http, err := p.svc.HandleRequest(context.Background(), &application.PluginHTTPRequest{Method: "GET", Context: application.RequestContext{InstanceID: testInstance}})
	if err != nil || http.StatusCode != 200 {
		t.Fatalf("status: %+v %v", http, err)
	}
	var body map[string]any
	if err := json.Unmarshal(http.Body, &body); err != nil {
		t.Fatal(err)
	}
	windows := body["window_details"].([]any)
	row := windows[0].(map[string]any)
	if body["runtime_state_persistent"] != false || row["id"] != "silent" || row["reminder_state"] != "suppressed" || row["reminder_request_id"] != "" {
		t.Fatalf("status hides real state/identity: %s", http.Body)
	}
}

// Omitting the reminder block resolves to the silent default and must behave
// exactly like the explicit silent policy: no command, suppressed state.
func TestOmittedReminderDefaultsToSuppressed(t *testing.T) {
	p := newPracticalApp(t, 1)
	cfg := practicalConfig(1)
	cfg.Reminder = nil
	resp, _ := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2})
	if !resp.Status.IsOK() {
		t.Fatal(resp.Status)
	}
	result := p.start(t, "default-silent", "c1", 1)
	if countRequestCommand(p.sink.take()) != 0 || result["reminder_state"] != "suppressed" || result["reminder_request_id"] != "" {
		t.Fatalf("omitted reminder should default to suppressed: %v", result)
	}
}

func TestDashboardConfirmationRejectsClockBeforeWindow(t *testing.T) {
	p := newPracticalApp(t, 1)
	p.start(t, "clock-check", "c1", 1)
	p.sink.take()
	p.advance(-time.Second)
	_, err := p.job(jobConfirmWindow, mustJSON(windowArgs{WindowID: "clock-check"}), "clock-confirm")
	requireErrorCode(t, err, status.CodeFailedPrecondition)
	if len(p.sink.take()) != 0 || p.window(t, "clock-check").State != windowOpened {
		t.Fatal("clock regression fabricated confirmation")
	}
}

func TestManualWindowPreservesDeadlinePrecision(t *testing.T) {
	p := newPracticalApp(t, 1)
	p.advance(123 * time.Millisecond)
	result := p.start(t, "precise", "c1", 1)
	effects := p.sink.take()
	cmd := effects[1].Union.(*application.RequestCommand)
	start, err := time.Parse(time.RFC3339Nano, result["start"].(string))
	if err != nil || !start.Equal(p.now()) {
		t.Fatalf("result lost start precision: %v %v", result, err)
	}
	end, err := time.Parse(time.RFC3339Nano, cmd.Deadline)
	if err != nil || !end.Equal(p.now().Add(time.Minute)) || result["end"] != cmd.Deadline {
		t.Fatalf("business deadline differs from command/result: %s %+v", cmd.Deadline, result)
	}
}

func TestLateCommandReceiptCannotRewriteCollectionState(t *testing.T) {
	for _, state := range []string{windowCompleted, windowMissed, windowCompletedLate} {
		t.Run(state, func(t *testing.T) {
			p := newPracticalApp(t, 1)
			p.start(t, "finished", "c1", 1)
			if state != windowCompleted {
				p.advance(time.Minute)
			}
			if state == windowMissed {
				resp, err := p.job(jobWindowCheck, "{}", "expire")
				requireJobResult(t, resp, err)
			} else {
				resp, err := p.job(jobConfirmWindow, mustJSON(windowArgs{WindowID: "finished"}), "confirm")
				requireJobResult(t, resp, err)
			}
			p.sink.take()
			before := p.window(t, "finished")
			p.event(t, &application.RequestCompleted{RequestID: "reminder-finished", EntityID: buzzerEntityID, Action: buzzerAction, State: application.CommandStateFailed, ResultJSON: "device did not execute reminder"})
			after := p.window(t, "finished")
			if before.State != state || after.State != state || after.ConfirmationSource != before.ConfirmationSource || after.ConfirmedAt != before.ConfirmedAt || after.ReminderState != "failed" {
				t.Fatalf("receipt conflated command result with user confirmation: %+v -> %+v", before, after)
			}
		})
	}
}
