package scheduledcompartment

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

const testDisplayEntity = "dev/display"

// These are caller-provided capability args, not application-generated glyphs.
func testDisplayPolicy() *DisplayPolicy {
	return &DisplayPolicy{
		ReminderArgs: json.RawMessage(`{"digits":[0,0,0,0,0,0,0,1]}`),
		MissedArgs:   json.RawMessage(`{"digits":[0,0,0,0,0,0,0,2]}`),
		IdleArgs:     json.RawMessage(`{"mode":"clock"}`),
	}
}

func newDisplayApp(t *testing.T, n int) *practicalApp {
	t.Helper()
	p := newPracticalApp(t, n)
	cfg := practicalConfig(n)
	cfg.Display = testDisplayPolicy()
	cfg.Compartments[0].Name = "早餐药格"
	configureDisplayTest(t, p, cfg, append(practicalBindings(n), application.Binding{RequirementID: "local-display", EntityID: testDisplayEntity}))
	return p
}

func configureDisplayTest(t *testing.T, p *practicalApp, cfg Config, bindings []application.Binding) {
	t.Helper()
	resp, err := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("configure display: %+v, %v", resp, err)
	}
	bound, err := p.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: bindings})
	if err != nil || !bound.Valid {
		t.Fatalf("bind display: %+v, %v", bound, err)
	}
}

func displayRequests(effects []*application.ApplicationEffect) []*application.RequestCommand {
	var out []*application.RequestCommand
	for _, effect := range effects {
		if cmd, ok := effect.Union.(*application.RequestCommand); ok && cmd.Action == displayAction {
			out = append(out, cmd)
		}
	}
	return out
}

func requireDisplayRequest(t *testing.T, effects []*application.ApplicationEffect, want json.RawMessage) *application.RequestCommand {
	t.Helper()
	cmds := displayRequests(effects)
	if len(cmds) != 1 {
		t.Fatalf("display commands=%d, want one", len(cmds))
	}
	args, err := canonicalDisplayArgs(want)
	if err != nil || cmds[0].ArgsJSON != args || cmds[0].EntityID != testDisplayEntity {
		t.Fatalf("display did not forward explicit args: %+v, %v", cmds[0], err)
	}
	return cmds[0]
}

func displayRecordData(t *testing.T, effects []*application.ApplicationEffect) map[string]any {
	t.Helper()
	var out map[string]any
	for _, effect := range effects {
		record, ok := effect.Union.(*application.UpsertDomainRecord)
		if !ok || record.RecordType != "display" || record.RecordID != "status" {
			continue
		}
		if err := json.Unmarshal([]byte(record.DataJSON), &out); err != nil {
			t.Fatal(err)
		}
	}
	if out == nil {
		t.Fatal("missing display status domain record")
	}
	return out
}

func (p *practicalApp) displayAck(t *testing.T, cmd *application.RequestCommand, state application.CommandState) {
	t.Helper()
	p.event(t, &application.RequestCompleted{RequestID: cmd.IdempotencyKey, EntityID: cmd.EntityID, Action: cmd.Action, State: state, ResultJSON: "opaque display outcome", ErrorCode: "example-result"})
}

func confirmDisplayWindow(t *testing.T, p *practicalApp, id string) map[string]any {
	t.Helper()
	resp, err := p.job(jobConfirmWindow, mustJSON(windowArgs{WindowID: id}), "confirm-"+id)
	return requireJobResult(t, resp, err)
}

func TestDisplayPolicyRequiresThreeBoundedObjects(t *testing.T) {
	for _, raw := range []string{"null", "[]", `"clock"`, "false", "123", "{}", `{"value":"` + strings.Repeat("x", maxDisplayArgsBytes) + `"}`, `{"value":"` + strings.Repeat("<", 400) + `"}`} {
		p := newPracticalApp(t, 1)
		cfg := practicalConfig(1)
		cfg.Display = testDisplayPolicy()
		cfg.Display.ReminderArgs = json.RawMessage(raw)
		resp, err := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(mustJSON(cfg)), ConfigRevision: 2})
		if err != nil || resp.Status.IsOK() || !strings.Contains(resp.Status.Error(), "display.reminder_args") {
			t.Fatalf("invalid display object %q accepted: %+v %v", raw, resp, err)
		}
		if p.svc.instance(testInstance).config.Display != nil {
			t.Fatal("invalid policy replaced prior config")
		}
	}
	for _, field := range []string{"reminder", "missed", "idle"} {
		policy := testDisplayPolicy()
		switch field {
		case "reminder":
			policy.ReminderArgs = nil
		case "missed":
			policy.MissedArgs = nil
		case "idle":
			policy.IdleArgs = nil
		}
		if policy.Validate() == nil {
			t.Fatalf("missing %s args got an invented default", field)
		}
	}
	policy := testDisplayPolicy()
	policy.ReminderArgs = json.RawMessage(`{"codes":[0,0,0,0,0,0,0,1]}`)
	if err := policy.Validate(); err != nil {
		t.Fatalf("explicit capability object rejected: %v", err)
	}
}

func TestDisplayIsOptInAndRequiresItsBinding(t *testing.T) {
	for _, tc := range []struct {
		name            string
		policy, binding bool
	}{{"neither", false, false}, {"policy only", true, false}, {"binding only", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPracticalApp(t, 1)
			cfg := practicalConfig(1)
			bindings := practicalBindings(1)
			if tc.policy {
				cfg.Display = testDisplayPolicy()
			}
			if tc.binding {
				bindings = append(bindings, application.Binding{RequirementID: "local-display", EntityID: testDisplayEntity})
			}
			configureDisplayTest(t, p, cfg, bindings)
			p.start(t, "no-visual", "c1", 1)
			effects := p.sink.take()
			if len(effects) != 3 || len(displayRequests(effects)) != 0 {
				t.Fatalf("display without opt-in and binding: %+v", effects)
			}
			confirmDisplayWindow(t, p, "no-visual")
			if effects = p.sink.take(); len(effects) != 2 || len(displayRequests(effects)) != 0 {
				t.Fatal("unconfigured idle output was guessed")
			}
		})
	}
}

func TestSilentReminderHasPendingVisualEvidence(t *testing.T) {
	p := newDisplayApp(t, 1)
	initial, err := p.job(jobWindowCheck, "{}", "empty-instance-heartbeat")
	requireJobResult(t, initial, err)
	if len(p.sink.take()) != 0 {
		t.Fatal("empty instance heartbeat emitted an idle display command")
	}
	result := p.start(t, "visible", "c1", 2)
	effects := p.sink.take()
	request := requireDisplayRequest(t, effects, testDisplayPolicy().ReminderArgs)
	if !strings.HasPrefix(request.IdempotencyKey, displayRequestPrefix) || request.IdempotencyKey == "reminder-visible" {
		t.Fatal("display and reminder requests share an ID")
	}
	for _, effect := range effects {
		if cmd, ok := effect.Union.(*application.RequestCommand); ok && cmd.Action == buzzerAction {
			var policy Reminder
			if err := json.Unmarshal([]byte(cmd.ArgsJSON), &policy); err != nil || policy != (Reminder{}) {
				t.Fatalf("visual mode made buzzer audible: %s", cmd.ArgsJSON)
			}
		}
	}
	record := displayRecordData(t, effects)
	current := record["current"].(map[string]any)
	if record["desired_state"] != displayReminder || current["state"] != "pending" || current["request_id"] != request.IdempotencyKey || current["done_at"] != "" || record["queued"] != false {
		t.Fatalf("sent was presented as displayed: %+v", record)
	}
	if result["display"].(map[string]any)["current"].(map[string]any)["state"] != "pending" {
		t.Fatalf("Job hides display pending: %+v", result)
	}
	http, err := p.svc.HandleRequest(context.Background(), &application.PluginHTTPRequest{Method: "GET", Context: application.RequestContext{InstanceID: testInstance}})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(http.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["display"].(map[string]any)["current"].(map[string]any)["request_id"] != request.IdempotencyKey {
		t.Fatalf("read-only status hides display correlation: %s", http.Body)
	}
	p.displayAck(t, request, application.CommandStateSucceeded)
	done := displayRecordData(t, p.sink.take())["current"].(map[string]any)
	if done["state"] != "succeeded" || done["done_at"] == "" {
		t.Fatalf("final display receipt missing: %+v", done)
	}
	w := p.window(t, "visible")
	if w.State != windowOpened || w.ReminderState != "pending" {
		t.Fatalf("display ACK confirmed collection or buzzer: %+v", w)
	}
}

func TestDisplayACKCorrelationAndPendingAreIndependent(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "ack-split", "c1", 2)
	request := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
	confirmDisplayWindow(t, p, "ack-split") // idle is desired, but reminder display is still pending
	queued := displayRecordData(t, p.sink.take())
	if queued["desired_state"] != displayIdle || queued["queued"] != true {
		t.Fatalf("idle was not queued: %+v", queued)
	}
	for _, state := range []application.CommandState{application.CommandStateUnspecified, application.CommandStateCreated, application.CommandStateDispatched, application.CommandStateAccepted, application.CommandStateRunning} {
		p.displayAck(t, request, state)
		if len(p.sink.take()) != 0 {
			t.Fatalf("intermediate state %v released display slot", state)
		}
	}
	for _, wrong := range []*application.RequestCompleted{
		{RequestID: "display-unknown", EntityID: request.EntityID, Action: displayAction, State: application.CommandStateSucceeded},
		{RequestID: request.IdempotencyKey, EntityID: "other-display", Action: displayAction, State: application.CommandStateSucceeded},
		{RequestID: request.IdempotencyKey, EntityID: request.EntityID, Action: buzzerAction, State: application.CommandStateSucceeded},
	} {
		p.event(t, wrong)
		if len(p.sink.take()) != 0 {
			t.Fatal("unrelated ACK changed display state")
		}
	}
	p.event(t, &application.RequestCompleted{RequestID: "reminder-ack-split", EntityID: buzzerEntityID, Action: buzzerAction, State: application.CommandStateSucceeded})
	if len(displayRequests(p.sink.take())) != 0 || p.svc.instance(testInstance).display.Current.State != "pending" {
		t.Fatal("buzzer result released the display queue")
	}
	p.advance(time.Minute) // even a passed command deadline is not a fabricated final ACK
	resp, err := p.job(jobWindowCheck, "{}", "heartbeat")
	requireJobResult(t, resp, err)
	if len(p.sink.take()) != 0 || p.svc.instance(testInstance).display.Current.State != "pending" {
		t.Fatal("heartbeat invented display completion")
	}
	p.displayAck(t, request, application.CommandStateSucceeded)
	idle := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().IdleArgs)
	if idle.IdempotencyKey == request.IdempotencyKey {
		t.Fatal("visual transition reused a request ID")
	}
}

func TestDisplayTerminalOutcomesStayHonestAndDoNotHeartbeatRetry(t *testing.T) {
	for state, want := range map[application.CommandState]string{application.CommandStateSucceeded: "succeeded", application.CommandStateFailed: "failed", application.CommandStateTimedOut: "timedout", application.CommandStateCancelled: "cancelled"} {
		t.Run(want, func(t *testing.T) {
			p := newDisplayApp(t, 1)
			p.start(t, "outcome", "c1", 2)
			request := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
			p.displayAck(t, request, state)
			effects := p.sink.take()
			record := displayRecordData(t, effects)
			current := record["current"].(map[string]any)
			if len(displayRequests(effects)) != 0 || current["state"] != want || current["result_json"] != "opaque display outcome" || current["error_code"] != "example-result" || current["done_at"] == "" {
				t.Fatalf("wrong display outcome: %+v", record)
			}
			for i := 0; i < 3; i++ {
				resp, err := p.job(jobWindowCheck, "{}", "")
				requireJobResult(t, resp, err)
				if len(p.sink.take()) != 0 {
					t.Fatal("unchanged aggregate retried display on a heartbeat")
				}
			}
			p.displayAck(t, request, application.CommandStateFailed)
			if len(p.sink.take()) != 0 || p.svc.instance(testInstance).display.Current.State != want {
				t.Fatal("duplicate/conflicting terminal ACK rewrote the outcome")
			}
			w := p.window(t, "outcome")
			if w.State != windowOpened || w.ReminderState != "pending" {
				t.Fatal("display outcome changed collection or buzzer result")
			}
		})
	}
}

func TestOverlappingDisplayPrioritizesMissedThenPendingThenIdle(t *testing.T) {
	for _, lateFirst := range []bool{false, true} {
		name := "confirm-open-first"
		if lateFirst {
			name = "confirm-late-first"
		}
		t.Run(name, func(t *testing.T) {
			p := newDisplayApp(t, 2)
			p.start(t, "short", "c1", 1)
			reminder := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
			p.displayAck(t, reminder, application.CommandStateSucceeded)
			p.sink.take()
			p.start(t, "long", "c2", 3)
			if len(displayRequests(p.sink.take())) != 0 {
				t.Fatal("second pending window redundantly wrote the display")
			}
			p.advance(time.Minute)
			resp, err := p.job(jobWindowCheck, "{}", "expire-short")
			requireJobResult(t, resp, err)
			missed := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().MissedArgs)
			p.displayAck(t, missed, application.CommandStateSucceeded)
			p.sink.take()
			if lateFirst {
				p.press(t, c1, p.now()) // physical late confirmation leaves another pending window
				reminder = requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
				if p.window(t, "short").State != windowCompletedLate || p.window(t, "long").State != windowOpened {
					t.Fatal("late confirmation affected another window")
				}
				p.displayAck(t, reminder, application.CommandStateSucceeded)
				p.sink.take()
				confirmDisplayWindow(t, p, "long")
			} else {
				confirmDisplayWindow(t, p, "long")
				if len(displayRequests(p.sink.take())) != 0 || p.svc.instance(testInstance).display.Desired.State != displayMissed {
					t.Fatal("one confirmation hid the remaining missed window")
				}
				confirmDisplayWindow(t, p, "short")
			}
			requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().IdleArgs)
		})
	}
}

func TestQueuedIdleIsCoalescedAwayWhenANewWindowOpens(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "first", "c1", 2)
	reminder := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
	confirmDisplayWindow(t, p, "first")
	if len(displayRequests(p.sink.take())) != 0 {
		t.Fatal("idle was sent before the previous display completed")
	}
	p.start(t, "second", "c1", 2)
	record := displayRecordData(t, p.sink.take())
	if record["queued"] != false || record["desired_state"] != displayReminder {
		t.Fatalf("obsolete idle was not discarded: %+v", record)
	}
	p.displayAck(t, reminder, application.CommandStateSucceeded)
	if len(displayRequests(p.sink.take())) != 0 || p.svc.instance(testInstance).display.Current.Target.State != displayReminder {
		t.Fatal("stale idle overwrote the new reminder")
	}
	if p.window(t, "second").State != windowOpened {
		t.Fatal("display completion confirmed the new window")
	}
}

func TestPendingIdleFinishesBeforeANewerReminderIsSent(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "first", "c1", 2)
	reminder := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
	p.displayAck(t, reminder, application.CommandStateSucceeded)
	p.sink.take()
	confirmDisplayWindow(t, p, "first")
	idle := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().IdleArgs)
	p.start(t, "second", "c1", 2)
	effects := p.sink.take()
	record := displayRecordData(t, effects)
	if len(displayRequests(effects)) != 0 || record["queued"] != true || record["current"].(map[string]any)["request_id"] != idle.IdempotencyKey {
		t.Fatal("new reminder raced the pending idle")
	}
	p.displayAck(t, idle, application.CommandStateSucceeded)
	effects = p.sink.take()
	next := requireDisplayRequest(t, effects, testDisplayPolicy().ReminderArgs)
	record = displayRecordData(t, effects)
	if next.IdempotencyKey == reminder.IdempotencyKey || record["current"].(map[string]any)["state"] != "pending" || record["last_completed"].(map[string]any)["request_id"] != idle.IdempotencyKey {
		t.Fatalf("lost independent request/outcome history: %+v", record)
	}
	p.displayAck(t, idle, application.CommandStateFailed)
	if len(p.sink.take()) != 0 || p.svc.instance(testInstance).display.Current.RequestID != next.IdempotencyKey || p.svc.instance(testInstance).display.Current.State != "pending" {
		t.Fatal("old idle ACK overwrote the newer display request")
	}
}

func TestConcurrentDisplayReceiptAndNewWindowKeepLatestVisualState(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "first", "c1", 2)
	reminder := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
	p.displayAck(t, reminder, application.CommandStateSucceeded)
	p.sink.take()
	confirmDisplayWindow(t, p, "first")
	idle := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().IdleArgs)
	done := make(chan error, 2)
	go func() {
		_, err := p.job(jobStartReminder, mustJSON(startReminderArgs{CompartmentID: "c1", Minutes: 2, WindowID: "second"}), "second")
		done <- err
	}()
	go func() {
		done <- p.svc.handleEvent(&application.ApplicationEvent{PluginInstanceID: testInstance, Sequence: p.sequence + 1, Union: &application.RequestCompleted{RequestID: idle.IdempotencyKey, EntityID: idle.EntityID, Action: displayAction, State: application.CommandStateSucceeded}})
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("display state transition deadlocked")
		}
	}
	requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
	if p.svc.instance(testInstance).display.Current.Target.State != displayReminder || p.svc.instance(testInstance).display.Current.State != "pending" {
		t.Fatal("concurrent transition left an old idle as current")
	}
}

func TestDisplayReceiptsAreScopedToTheirInstance(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "same-window", "c1", 2)
	first := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
	const other = "app-other"
	cfg := practicalConfig(1)
	cfg.Display = testDisplayPolicy()
	resp, err := p.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: other, Config: []byte(mustJSON(cfg)), ConfigRevision: 1})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("other configure: %+v %v", resp, err)
	}
	bound, err := p.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: other, Bindings: append(practicalBindings(1), application.Binding{RequirementID: "local-display", EntityID: testDisplayEntity})})
	if err != nil || !bound.Valid {
		t.Fatalf("other bind: %+v %v", bound, err)
	}
	otherSink := &effectCapture{}
	p.svc.writers[other] = otherSink
	job, err := p.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: other, JobID: jobStartReminder, ArgsJSON: mustJSON(startReminderArgs{CompartmentID: "c1", Minutes: 2, WindowID: "same-window"})})
	requireJobResult(t, job, err)
	second := requireDisplayRequest(t, otherSink.take(), testDisplayPolicy().ReminderArgs)
	if first.IdempotencyKey == second.IdempotencyKey {
		t.Fatal("instances share a display request ID")
	}
	if err := p.svc.handleEvent(&application.ApplicationEvent{PluginInstanceID: other, Sequence: 1, Union: &application.RequestCompleted{RequestID: first.IdempotencyKey, EntityID: testDisplayEntity, Action: displayAction, State: application.CommandStateSucceeded}}); err != nil {
		t.Fatal(err)
	}
	if len(otherSink.take()) != 0 || len(p.sink.take()) != 0 || p.svc.instance(other).display.Current.State != "pending" {
		t.Fatal("cross-instance ACK was applied")
	}
}

func TestWindowLabelsCaptureNameAndIgnoreCommandOutcomes(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "named", "c1", 2)
	effects := p.sink.take()
	request := requireDisplayRequest(t, effects, testDisplayPolicy().ReminderArgs)
	title, _ := windowFieldOf(effects, "named", "title")
	summary, _ := windowFieldOf(effects, "named", "summary")
	if title != "早餐药格：待确认取药" || summary == "" || p.window(t, "named").CompartmentName != "早餐药格" {
		t.Fatalf("missing captured display name: %v / %v", title, summary)
	}
	p.displayAck(t, request, application.CommandStateSucceeded)
	if windowStateOf(p.sink.take(), "named") != "" {
		t.Fatal("display receipt rewrote collection record")
	}
	p.event(t, &application.RequestCompleted{RequestID: "reminder-named", EntityID: buzzerEntityID, Action: buzzerAction, State: application.CommandStateSucceeded})
	effects = p.sink.take()
	afterTitle, _ := windowFieldOf(effects, "named", "title")
	afterSummary, _ := windowFieldOf(effects, "named", "summary")
	if afterTitle != title || afterSummary != summary || p.window(t, "named").State != windowOpened {
		t.Fatal("buzzer ACK claimed collection in the presentation")
	}
	cfg := practicalConfig(1)
	cfg.Display = testDisplayPolicy()
	cfg.Compartments[0].Name = "改名后的药格"
	configureDisplayTest(t, p, cfg, append(practicalBindings(1), application.Binding{RequirementID: "local-display", EntityID: testDisplayEntity}))
	p.advance(time.Second)
	p.press(t, c1, p.now())
	effects = p.sink.take()
	afterTitle, _ = windowFieldOf(effects, "named", "title")
	afterSummary, _ = windowFieldOf(effects, "named", "summary")
	if afterTitle != "早餐药格：已人工确认取药" || !strings.Contains(afterSummary.(string), "不代表药物已经吞服") || p.window(t, "named").State != windowCompleted {
		t.Fatalf("historical name/confirmation meaning changed: %v / %v", afterTitle, afterSummary)
	}
	p.start(t, "renamed", "c1", 2)
	if got, _ := windowFieldOf(p.sink.take(), "renamed", "title"); got != "改名后的药格：待确认取药" {
		t.Fatalf("new window did not capture new name: %v", got)
	}
}

func TestMissedAndLateWindowLabelsDoNotChangeMachineIdentity(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.start(t, "labels", "c1", 1)
	p.sink.take()
	p.advance(time.Minute)
	resp, err := p.job(jobWindowCheck, "{}", "labels-expire")
	requireJobResult(t, resp, err)
	effects := p.sink.take()
	title, _ := windowFieldOf(effects, "labels", "title")
	if title != "早餐药格：已超时" || windowStateOf(effects, "labels") != windowMissed {
		t.Fatalf("missed label/state: %v", title)
	}
	result := confirmDisplayWindow(t, p, "labels")
	effects = p.sink.take()
	title, _ = windowFieldOf(effects, "labels", "title")
	if title != "早餐药格：迟到确认取药" || windowStateOf(effects, "labels") != windowCompletedLate || result["window_id"] != "labels" || result["reminder_request_id"] != "reminder-labels" {
		t.Fatalf("late label changed machine identity: %v / %v", title, result)
	}
	if p.window(t, "labels").ReminderState != "pending" {
		t.Fatal("confirmation fabricated a reminder ACK")
	}
}

func TestJobSchemasExposeChineseFieldHelpOverRPC(t *testing.T) {
	a := newTestApp(t, time.Now())
	defer a.close()
	desc, err := a.cli.Describe(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string][]string{jobStartReminder: {"compartment_id", "minutes", "window_id"}, jobConfirmWindow: {"window_id"}, jobWindowCheck: {"window_id"}}
	for _, job := range desc.Jobs {
		var schema struct {
			Properties map[string]struct{ Title, Description string }
		}
		if err := json.Unmarshal([]byte(job.InputSchemaJSON), &schema); err != nil {
			t.Fatal(err)
		}
		for _, field := range expected[job.ID] {
			prop := schema.Properties[field]
			if strings.TrimSpace(prop.Title) == "" || strings.TrimSpace(prop.Description) == "" {
				t.Fatalf("%s.%s lacks UI guidance: %s", job.ID, field, job.InputSchemaJSON)
			}
		}
	}
}

func TestDisplayTransitionsAcrossPublicRPCWire(t *testing.T) {
	start := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	a := newTestApp(t, start)
	defer a.close()
	cfg := practicalConfig(1)
	cfg.Display = testDisplayPolicy()
	cfg.Compartments[0].Name = "示例药格"
	if resp := a.configure(mustJSON(cfg)); !resp.Status.IsOK() {
		t.Fatal(resp.Status)
	}
	if resp := a.validate(append(practicalBindings(1), application.Binding{RequirementID: "local-display", EntityID: testDisplayEntity})); !resp.Valid {
		t.Fatal(resp.Issues)
	}
	a.openStream()
	a.send(1, scheduleTick("wire-visual", "c1", start, time.Minute))
	effects := a.waitEffects(5, 20*time.Millisecond)
	reminder := requireDisplayRequest(t, effects, testDisplayPolicy().ReminderArgs)
	a.send(2, &application.RequestCompleted{RequestID: reminder.IdempotencyKey, EntityID: testDisplayEntity, Action: displayAction, State: application.CommandStateSucceeded})
	record := displayRecordData(t, a.waitEffects(1, 20*time.Millisecond))
	if record["current"].(map[string]any)["state"] != "succeeded" {
		t.Fatalf("wire outcome missing: %+v", record)
	}
	a.send(3, &application.CapabilityEvent{RequirementID: "compartments", EntityID: c1, EventType: keyPressEvent, OccurredAt: start.Format(time.RFC3339)})
	requireDisplayRequest(t, a.waitEffects(4, 20*time.Millisecond), testDisplayPolicy().IdleArgs)
}

func TestQueuedVisualKeepsThePreviousFailureVisible(t *testing.T) {
	for state, want := range map[application.CommandState]string{application.CommandStateFailed: "failed", application.CommandStateTimedOut: "timedout"} {
		t.Run(want, func(t *testing.T) {
			p := newDisplayApp(t, 1)
			p.start(t, "failed-visible", "c1", 1)
			reminder := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
			confirmDisplayWindow(t, p, "failed-visible")
			p.sink.take()
			p.displayAck(t, reminder, state)
			effects := p.sink.take()
			requireDisplayRequest(t, effects, testDisplayPolicy().IdleArgs)
			record := displayRecordData(t, effects)
			last := record["last_completed"].(map[string]any)
			if record["current"].(map[string]any)["state"] != "pending" || last["state"] != want || last["request_id"] != reminder.IdempotencyKey || !strings.Contains(record["summary"].(string), commandOutcomeLabel(want)) {
				t.Fatalf("next pending command hid the previous failure: %+v", record)
			}
			if p.window(t, "failed-visible").State != windowCompleted {
				t.Fatal("display failure rewrote a user confirmation")
			}
		})
	}
}

func TestRemovingDisplayOptInCannotSendAPreviouslyQueuedIdle(t *testing.T) {
	for _, removePolicy := range []bool{true, false} {
		name := "remove-binding"
		if removePolicy {
			name = "remove-policy"
		}
		t.Run(name, func(t *testing.T) {
			p := newDisplayApp(t, 1)
			p.start(t, "disabled", "c1", 1)
			reminder := requireDisplayRequest(t, p.sink.take(), testDisplayPolicy().ReminderArgs)
			confirmDisplayWindow(t, p, "disabled")
			p.sink.take()
			cfg := practicalConfig(1)
			bindings := practicalBindings(1)
			if removePolicy {
				bindings = append(bindings, application.Binding{RequirementID: "local-display", EntityID: testDisplayEntity})
			} else {
				cfg.Display = testDisplayPolicy()
			}
			configureDisplayTest(t, p, cfg, bindings)
			before := displayData(p.svc.instance(testInstance))
			if before["enabled"] != false || before["queued"] != false || before["desired_state"] != "" {
				t.Fatalf("disabled policy still advertises queued output: %+v", before)
			}
			p.displayAck(t, reminder, application.CommandStateSucceeded)
			effects := p.sink.take()
			record := displayRecordData(t, effects)
			if len(displayRequests(effects)) != 0 || record["enabled"] != false || record["current"].(map[string]any)["state"] != "succeeded" {
				t.Fatalf("disabled display emitted reset or discarded final receipt: %+v", record)
			}
		})
	}
}

func TestPartialDisplaySubmissionNeverReturnsDisplayedSuccess(t *testing.T) {
	p := newDisplayApp(t, 1)
	p.sink.failAt = 5 // window, quiet buzzer, task, display pending record, failed display send
	resp, err := p.job(jobStartReminder, mustJSON(startReminderArgs{CompartmentID: "c1", Minutes: 1, WindowID: "partial-display"}), "partial-display")
	if err == nil || resp != nil {
		t.Fatalf("partial display submission reported success: %+v %v", resp, err)
	}
	effects := p.sink.take()
	record := displayRecordData(t, effects)
	if len(displayRequests(effects)) != 0 || record["current"].(map[string]any)["state"] != "pending" || !p.svc.instance(testInstance).deliveryUncertain {
		t.Fatalf("partial send fabricated a final display result: %+v", record)
	}
}
