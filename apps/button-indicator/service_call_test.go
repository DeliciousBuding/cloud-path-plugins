package buttonindicator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

const callKey = "dev/key-3"

var callBindings = []application.Binding{
	{RequirementID: "button-input", EntityID: callKey},
	{RequirementID: "indicator", EntityID: ledEntity},
	{RequirementID: "acknowledge-input", EntityID: key2},
}

// Captures only public SDK effects. Completion frames below are test stimuli,
// not hardware evidence or a production acknowledgement implementation.
type callCapture struct {
	mu      sync.Mutex
	effects []*application.ApplicationEffect
	attempt int
	failAt  int
}

func (c *callCapture) Send(_ context.Context, eff *application.ApplicationEffect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempt++
	if c.failAt == c.attempt {
		return errors.New("test effect stream failed")
	}
	c.effects = append(c.effects, eff)
	return nil
}

func (c *callCapture) take() []*application.ApplicationEffect {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.effects
	c.effects = nil
	return out
}

type callFixture struct {
	t      *testing.T
	svc    *Service
	writer *callCapture
	clock  atomic.Int64
	seq    uint64
}

func newCallFixture(t *testing.T, config string, bindings []application.Binding) *callFixture {
	t.Helper()
	f := &callFixture{t: t, svc: New(), writer: &callCapture{}}
	f.clock.Store(time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC).UnixNano())
	f.svc.now = func() time.Time { return time.Unix(0, f.clock.Load()) }
	resp, err := f.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance, Config: []byte(config), ConfigRevision: 1,
	})
	if err != nil || !resp.Status.IsOK() {
		t.Fatalf("configure: %+v, %v", resp, err)
	}
	bound, err := f.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: bindings})
	if err != nil || !bound.Valid {
		t.Fatalf("bindings: %+v, %v", bound, err)
	}
	f.svc.writers = map[string]application.ApplicationEffectWriter{testInstance: f.writer}
	t.Cleanup(func() { _, _ = f.svc.Shutdown(context.Background(), &application.ShutdownRequest{}) })
	return f
}

func (f *callFixture) event(ev application.ApplicationEventUnion) {
	f.t.Helper()
	f.seq++
	if err := f.svc.handleEvent(&application.ApplicationEvent{PluginInstanceID: testInstance, Sequence: f.seq, Union: ev}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *callFixture) press(requirement, entity string) {
	f.t.Helper()
	f.event(&application.CapabilityEvent{RequirementID: requirement, EntityID: entity, EventType: keyPressEvent})
}

func (f *callFixture) job(job, args, key string) (*application.RunJobResponse, error) {
	return f.svc.RunJob(context.Background(), &application.RunJobRequest{
		PluginInstanceID: testInstance, JobID: job, ArgsJSON: args, IdempotencyKey: key,
	})
}

func (f *callFixture) mustJob(job, args, key string) string {
	f.t.Helper()
	resp, err := f.job(job, args, key)
	if err != nil || !resp.Status.IsOK() {
		f.t.Fatalf("job %s: %+v, %v", job, resp, err)
	}
	var result struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(resp.ResultJSON), &result); err != nil || result.RequestID == "" {
		f.t.Fatalf("job result: %s, %v", resp.ResultJSON, err)
	}
	return result.RequestID
}

func (f *callFixture) request(key string) string {
	return f.mustJob(jobRequest, `{"confirm":true}`, key)
}

func (f *callFixture) acknowledge(id, key string) string {
	return f.mustJob(jobAcknowledge, mustJSON(map[string]string{"request_id": id}), key)
}

func (f *callFixture) complete(cmd *application.RequestCommand, state application.CommandState, code string) {
	f.t.Helper()
	f.event(&application.RequestCompleted{
		RequestID: cmd.IdempotencyKey, EntityID: cmd.EntityID, Action: cmd.Action,
		State: state, ErrorCode: code, ResultJSON: `{"detail":"test device result"}`,
	})
}

type callSummary struct {
	PendingID    string         `json:"pending_request_id"`
	RequestCount int            `json:"request_count"`
	Scope        string         `json:"runtime_scope"`
	Request      *callRecord    `json:"request"`
	Indicator    *commandResult `json:"indicator"`
	LEDMask      *int           `json:"led_mask"`
}

func readCallSummary(t *testing.T, svc *Service) callSummary {
	t.Helper()
	req := &application.PluginHTTPRequest{Method: "GET"}
	req.Context.InstanceID = testInstance
	resp, err := svc.HandleRequest(context.Background(), req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("summary: %+v, %v", resp, err)
	}
	var out callSummary
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func callEffects(t *testing.T, effects []*application.ApplicationEffect) (*callRecord, []*application.RequestCommand) {
	t.Helper()
	var record *callRecord
	var commands []*application.RequestCommand
	for _, eff := range effects {
		switch u := eff.Union.(type) {
		case *application.UpsertDomainRecord:
			if u.RecordType != callRecordType {
				continue
			}
			record = &callRecord{}
			if err := json.Unmarshal([]byte(u.DataJSON), record); err != nil {
				t.Fatal(err)
			}
			if u.RecordID != record.RequestID || u.Version != fmt.Sprint(record.Revision) {
				t.Fatalf("record identity/version drift: %+v / %+v", u, record)
			}
		case *application.RequestCommand:
			commands = append(commands, u)
		}
	}
	return record, commands
}

func requireNoCallEffects(t *testing.T, f *callFixture) {
	t.Helper()
	if effects := f.writer.take(); len(effects) != 0 {
		t.Fatalf("unexpected effects: %+v", effects)
	}
}

func TestCallConfigModes(t *testing.T) {
	for input, want := range map[string]string{"": modeWalkingLight, "walking-light": modeWalkingLight, "call": modeServiceCall, "service-call": modeServiceCall} {
		cfg, err := UnmarshalConfig([]byte(mustJSON(map[string]string{"mode": input})))
		if err != nil || cfg.ResolvedMode() != want || cfg.BeepOnPress || cfg.ResolvedHeartbeatCron() != defaultHeartbeatCron {
			t.Fatalf("mode %q: %+v, %v", input, cfg, err)
		}
	}
	for _, input := range []string{`{"mode":"service"}`, `{"mode":1}`, `{"mode":"CALL"}`} {
		if _, err := UnmarshalConfig([]byte(input)); err == nil {
			t.Fatalf("invalid mode accepted: %s", input)
		}
	}
}

func TestCallBindingRoles(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bindings []application.Binding
		valid    bool
	}{
		{"optional ack", validBindings, true},
		{"single board", callBindings, true},
		{"cross-device", []application.Binding{{RequirementID: "button-input", EntityID: "request-device/key"}, {RequirementID: "indicator", EntityID: "desk-device/led"}, {RequirementID: "acknowledge-input", EntityID: "third-device/key"}}, true},
		{"two ack inputs", append(append([]application.Binding{}, callBindings...), application.Binding{RequirementID: "acknowledge-input", EntityID: key1}), false},
		{"ambiguous key", []application.Binding{{RequirementID: "button-input", EntityID: callKey}, {RequirementID: "indicator", EntityID: ledEntity}, {RequirementID: "acknowledge-input", EntityID: callKey}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := New().ValidateBinding(context.Background(), &application.ValidateBindingRequest{Bindings: tc.bindings})
			if err != nil || resp.Valid != tc.valid {
				t.Fatalf("validation: %+v / %v", resp, err)
			}
		})
	}
}

func TestCallJobsValidateExplicitInputs(t *testing.T) {
	for _, job := range []string{jobRequest, jobAcknowledge} {
		invalid := []string{"", "null", "[]", "{}", "{} {}", `{"unknown":true}`, `{"CONFIRM":true}`, `{"REQUEST_ID":"x"}`}
		if job == jobRequest {
			invalid = append(invalid, `{"confirm":false}`, `{"confirm":null}`, `{"confirm":"true"}`, `{"confirm":true,"note":null}`, `{"confirm":true,"extra":1}`,
				mustJSON(map[string]any{"confirm": true, "note": strings.Repeat("字", 257)}))
		} else {
			invalid = append(invalid, `{"request_id":""}`, `{"request_id":null}`, `{"request_id":2}`, `{"request_id":" x "}`, `{"request_id":"x","extra":1}`,
				mustJSON(map[string]string{"request_id": strings.Repeat("x", 129)}))
		}
		for _, input := range invalid {
			t.Run(job+"/"+input, func(t *testing.T) {
				f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
				_, err := f.job(job, input, "invalid")
				var st *status.Status
				if !errors.As(err, &st) || st.Code != status.CodeInvalidArgument {
					t.Fatalf("expected INVALID_ARGUMENT, got %v", err)
				}
				requireNoCallEffects(t, f)
			})
		}
	}
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	f.mustJob(jobRequest, mustJSON(map[string]any{"confirm": true, "note": strings.Repeat("字", 256)}), "unicode-note")
	f.writer.take()
	_, err := f.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobRequest, JobType: "scheduled", ArgsJSON: `{"confirm":true}`})
	if err == nil {
		t.Fatal("scheduled service request accepted")
	}
	requireNoCallEffects(t, f)
	legacy := newCallFixture(t, `{}`, callBindings)
	if _, err := legacy.job(jobRequest, `{"confirm":true}`, "legacy"); err == nil {
		t.Fatal("manual call changed legacy mode")
	}
	requireNoCallEffects(t, legacy)
}

func TestCallPendingCoalescesAndDebounces(t *testing.T) {
	f := newCallFixture(t, `{"mode":"call"}`, callBindings)
	f.press("button-input", callKey)
	record, commands := callEffects(t, f.writer.take())
	if record == nil || record.Status != "pending" || record.IndicatorOn.State != "awaiting_result" || len(commands) != 1 || commands[0].ArgsJSON != `{"mask":255}` {
		t.Fatalf("new call: %+v / %+v", record, commands)
	}
	firstID := record.RequestID
	// A duplicate envelope is rejected even after the debounce interval.
	f.clock.Add(int64(time.Second))
	if err := f.svc.handleEvent(&application.ApplicationEvent{PluginInstanceID: testInstance, Sequence: 1, Union: &application.CapabilityEvent{RequirementID: "button-input", EntityID: callKey, EventType: keyPressEvent}}); err != nil {
		t.Fatal(err)
	}
	f.press("button-input", callKey)
	f.press("button-input", callKey)
	if id := f.request("coalesced-console"); id != firstID {
		t.Fatalf("pending call duplicated: %q", id)
	}
	requireNoCallEffects(t, f)
	f.press("acknowledge-input", key2)
	record, commands = callEffects(t, f.writer.take())
	if record.Status != "acknowledged" || record.AcknowledgeEntity != key2 || record.IndicatorOff.State != "awaiting_result" || len(commands) != 1 || commands[0].ArgsJSON != `{"mask":0}` {
		t.Fatalf("acknowledgement: %+v / %+v", record, commands)
	}
	f.clock.Add(int64(100 * time.Millisecond))
	f.press("button-input", callKey) // bounce after acknowledgement cannot reopen
	requireNoCallEffects(t, f)
	f.clock.Add(int64(callDebounce + time.Millisecond))
	f.press("button-input", callKey)
	record, _ = callEffects(t, f.writer.take())
	if record.RequestID == firstID || record.Status != "pending" {
		t.Fatalf("new deliberate press: %+v", record)
	}
	// A wrong routing role or entity cannot acknowledge the new request.
	f.press("button-input", key2)
	f.press("acknowledge-input", "unbound/key")
	f.clock.Add(int64(time.Second))
	f.event(&application.CapabilityEvent{RequirementID: "acknowledge-input", EntityID: key2, EventType: keyPressEvent, OccurredAt: "2026-09-08T07:59:59Z"})
	requireNoCallEffects(t, f)
	if got := readCallSummary(t, f.svc); got.PendingID != record.RequestID || got.RequestCount != 2 {
		t.Fatalf("summary: %+v", got)
	}
}

func TestCallJobsIdempotencyAndOldConfirmation(t *testing.T) {
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	old := f.request("create-1")
	_, on := callEffects(t, f.writer.take())
	f.acknowledge(old, "ack-1")
	_, off := callEffects(t, f.writer.take())
	current := f.request("create-2")
	f.writer.take()
	for _, key := range []string{"ack-1", "another-old-ack", ""} {
		if got := f.acknowledge(old, key); got != old {
			t.Fatalf("old ack retargeted: %q", got)
		}
	}
	if got := f.request("create-1"); got != old {
		t.Fatalf("replayed request created another call: %q", got)
	}
	if _, err := f.job(jobAcknowledge, mustJSON(map[string]string{"request_id": current}), "ack-1"); err == nil {
		t.Fatal("idempotency key reused for a new acknowledgement")
	}
	if _, err := f.job(jobRequest, `{"confirm":true,"note":"different"}`, "create-1"); err == nil {
		t.Fatal("idempotency key reused with different request args")
	}
	if _, err := f.job(jobAcknowledge, `{"request_id":"missing"}`, "unknown"); err == nil {
		t.Fatal("unknown request accepted")
	}
	requireNoCallEffects(t, f)
	// Out-of-order old results update their own domain record only. In
	// particular, they never emit another off or change the current indicator.
	f.complete(off[0], application.CommandStateSucceeded, "")
	record, cmds := callEffects(t, f.writer.take())
	if record.RequestID != old || record.Status != "acknowledged" || len(cmds) != 0 {
		t.Fatalf("old off: %+v / %v", record, cmds)
	}
	f.complete(on[0], application.CommandStateFailed, "DEVICE_FAILED")
	record, cmds = callEffects(t, f.writer.take())
	if record.RequestID != old || record.IndicatorOn.State != "failed" || record.IndicatorOff.State != "succeeded" || len(cmds) != 0 {
		t.Fatalf("old on: %+v / %v", record, cmds)
	}
	f.complete(on[0], application.CommandStateSucceeded, "") // duplicate/contradictory terminal frame
	requireNoCallEffects(t, f)
	if got := readCallSummary(t, f.svc); got.PendingID != current || got.Indicator.CommandID != current+":on" || got.Indicator.State != "awaiting_result" || got.LEDMask != nil {
		t.Fatalf("stale result contaminated current request: %+v", got)
	}
}

func TestCallDeviceTerminalResults(t *testing.T) {
	for sdkState, want := range map[application.CommandState]string{
		application.CommandStateSucceeded: "succeeded", application.CommandStateFailed: "failed", application.CommandStateTimedOut: "timed_out", application.CommandStateCancelled: "cancelled",
	} {
		t.Run(want, func(t *testing.T) {
			f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
			id := f.request("create")
			_, cmds := callEffects(t, f.writer.take())
			for _, bad := range []*application.RequestCompleted{
				{RequestID: cmds[0].IdempotencyKey, EntityID: cmds[0].EntityID, Action: cmds[0].Action, State: application.CommandStateDispatched},
				{RequestID: "other", EntityID: cmds[0].EntityID, Action: cmds[0].Action, State: sdkState},
				{RequestID: cmds[0].IdempotencyKey, EntityID: "other", Action: cmds[0].Action, State: sdkState},
				{RequestID: cmds[0].IdempotencyKey, EntityID: cmds[0].EntityID, Action: "other", State: sdkState},
			} {
				f.event(bad)
			}
			requireNoCallEffects(t, f)
			f.complete(cmds[0], sdkState, "RESULT_CODE")
			record, extra := callEffects(t, f.writer.take())
			if record.Status != "pending" || record.IndicatorOn.State != want || record.IndicatorOn.ErrorCode != "RESULT_CODE" || record.IndicatorOn.ResultJSON != `{"detail":"test device result"}` || len(extra) != 0 {
				t.Fatalf("on result: %+v", record)
			}
			f.acknowledge(id, "ack")
			_, off := callEffects(t, f.writer.take())
			f.complete(off[0], sdkState, "OFF_CODE")
			record, _ = callEffects(t, f.writer.take())
			if record.Status != "acknowledged" || record.IndicatorOff.State != want || record.IndicatorOff.ErrorCode != "OFF_CODE" {
				t.Fatalf("off result: %+v", record)
			}
		})
	}
}

func TestCallRealTimerAndLateResult(t *testing.T) {
	// synctest advances Go's timer clock without waiting ten wall-clock seconds.
	// This exercises the actual AfterFunc path, not a fabricated Core timeout.
	synctest.Test(t, func(t *testing.T) {
		f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
		f.svc.now = time.Now
		id := f.request("create")
		_, cmds := callEffects(t, f.writer.take())
		time.Sleep(callResultWait + time.Millisecond)
		synctest.Wait()
		record, _ := callEffects(t, f.writer.take())
		if record.Status != "pending" || record.IndicatorOn.State != "result_timeout" || record.IndicatorOn.ErrorCode != "RESULT_NOT_OBSERVED" {
			t.Fatalf("deadline: %+v", record)
		}
		f.complete(cmds[0], application.CommandStateSucceeded, "")
		record, _ = callEffects(t, f.writer.take())
		if record.IndicatorOn.State != "succeeded" || record.IndicatorOn.ResultTimeoutAt == "" || record.IndicatorOn.ErrorCode != "" {
			t.Fatalf("late result lost deadline evidence: %+v", record)
		}
		f.acknowledge(id, "ack")
		f.writer.take()
		time.Sleep(callResultWait + time.Millisecond)
		synctest.Wait()
		record, _ = callEffects(t, f.writer.take())
		if record.Status != "acknowledged" || record.IndicatorOff.State != "result_timeout" {
			t.Fatalf("unconfirmed off: %+v", record)
		}
		if got := readCallSummary(t, f.svc); got.PendingID != "" || got.Indicator.State != "result_timeout" || got.LEDMask != nil {
			t.Fatalf("unknown off presented as success: %+v", got)
		}
	})
}

func TestCallEffectFailuresRemainHonest(t *testing.T) {
	t.Run("no stream", func(t *testing.T) {
		f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
		delete(f.svc.writers, testInstance)
		if _, err := f.job(jobRequest, `{"confirm":true}`, "no-stream"); err == nil {
			t.Fatal("job succeeded without a stream")
		}
		if got := readCallSummary(t, f.svc); got.PendingID != "" || got.RequestCount != 0 {
			t.Fatalf("silent dropped call: %+v", got)
		}
	})
	for failAt, want := range map[int]string{1: "not_dispatched", 2: "delivery_unknown"} {
		t.Run(want, func(t *testing.T) {
			f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
			f.writer.failAt = failAt
			if _, err := f.job(jobRequest, `{"confirm":true}`, "uncertain"); err == nil {
				t.Fatal("delivery failure reported success")
			}
			got := readCallSummary(t, f.svc)
			if got.PendingID == "" || got.Indicator.State != want || got.Indicator.ErrorCode != "EFFECT_DELIVERY_FAILED" {
				t.Fatalf("delivery error: %+v", got)
			}
			id := got.PendingID
			f.writer.take()
			f.event(&application.InstanceLifecycle{State: "running"})
			f.writer.take()
			f.acknowledge(id, "ack")
			f.writer.take()
			current := f.request("new")
			f.writer.take()
			if retried := f.request("uncertain"); retried != id {
				t.Fatalf("failed-send retry created another call: %q", retried)
			}
			requireNoCallEffects(t, f)
			if got := readCallSummary(t, f.svc); got.PendingID != current {
				t.Fatalf("retry changed current request: %+v", got)
			}
		})
	}
	t.Run("ack send error", func(t *testing.T) {
		f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
		id := f.request("create")
		f.writer.take()
		f.writer.failAt = 4 // request record + on + acknowledgement record + off
		if _, err := f.job(jobAcknowledge, mustJSON(map[string]string{"request_id": id}), "ack"); err == nil {
			t.Fatal("off send failure reported success")
		}
		got := readCallSummary(t, f.svc)
		if got.Request.Status != "acknowledged" || got.Indicator.State != "delivery_unknown" {
			t.Fatalf("business ack confused with lamp off: %+v", got)
		}
	})
}

func TestCallOptionalSound(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, bound := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%v/bound=%v", enabled, bound), func(t *testing.T) {
				bindings := append([]application.Binding{}, callBindings...)
				if bound {
					bindings = append(bindings, application.Binding{RequirementID: "sound", EntityID: buzzerEntity})
				}
				f := newCallFixture(t, mustJSON(map[string]any{"mode": "service-call", "beep_on_press": enabled}), bindings)
				id := f.request("create")
				record, cmds := callEffects(t, f.writer.take())
				want := 1
				if enabled && bound {
					want = 2
				}
				if len(cmds) != want || (record.Sound != nil) != (want == 2) {
					t.Fatalf("sound default/binding: %+v / %v", record, cmds)
				}
				f.request("same-pending")
				requireNoCallEffects(t, f)
				if want == 2 {
					if cmds[1].EntityID != buzzerEntity || cmds[1].Action != buzzerAct {
						t.Fatalf("sound command: %+v", cmds[1])
					}
					f.complete(cmds[1], application.CommandStateFailed, "SOUND_FAILED")
					record, _ = callEffects(t, f.writer.take())
					if record.Sound.State != "failed" || record.IndicatorOn.State != "awaiting_result" || record.Status != "pending" {
						t.Fatalf("sound contaminated indicator/business: %+v", record)
					}
				}
				f.acknowledge(id, "ack")
				_, cmds = callEffects(t, f.writer.take())
				if len(cmds) != 1 || cmds[0].Action != ledAction {
					t.Fatal("ack unexpectedly beeped")
				}
			})
		}
	}
}

func TestCallConfigurationAndBindingChanges(t *testing.T) {
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	id := f.request("create")
	_, on := callEffects(t, f.writer.take())
	configure := func(config string) *application.ConfigureInstanceResponse {
		resp, err := f.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: testInstance, Config: []byte(config), ConfigRevision: 2})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := configure(`{"mode":"walking-light"}`); resp.Status.IsOK() {
		t.Fatal("mode change abandoned pending call")
	}
	if resp := configure(`{"mode":"call","timezone":"Asia/Shanghai"}`); !resp.Status.IsOK() {
		t.Fatalf("safe config update rejected: %+v", resp)
	}
	changed := append([]application.Binding{}, callBindings...)
	changed[1].EntityID = "another/led"
	resp, _ := f.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: changed})
	if resp.Valid {
		t.Fatal("rebind abandoned pending indicator")
	}
	reordered := []application.Binding{callBindings[2], callBindings[0], callBindings[1]}
	resp, _ = f.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: reordered})
	if !resp.Valid {
		t.Fatalf("unchanged binding set rejected: %+v", resp)
	}
	f.acknowledge(id, "ack")
	_, off := callEffects(t, f.writer.take())
	if resp := configure(`{"mode":"walking-light"}`); resp.Status.IsOK() {
		t.Fatal("mode changed before off result")
	}
	f.complete(on[0], application.CommandStateSucceeded, "")
	f.complete(off[0], application.CommandStateSucceeded, "")
	if resp := configure(`{"mode":"walking-light"}`); !resp.Status.IsOK() {
		t.Fatalf("settled mode change rejected: %+v", resp)
	}
}

func TestCallHeartbeatAndRuntimeRestartBoundary(t *testing.T) {
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	if _, err := f.job(jobBootstrap, "", "bootstrap"); err != nil {
		t.Fatal(err)
	}
	effects := f.writer.take()
	if len(effects) != 1 {
		t.Fatalf("bootstrap effects: %+v", effects)
	}
	if task, ok := effects[0].Union.(*application.ScheduleTask); !ok || task.Cron != defaultHeartbeatCron || task.ScheduleID != heartbeatCaps {
		t.Fatalf("heartbeat changed: %+v", effects[0])
	}
	id := f.request("create")
	f.writer.take()
	if _, err := f.job(jobHeartbeat, "", "heartbeat"); err != nil {
		t.Fatal(err)
	}
	effects = f.writer.take()
	if len(effects) != 1 {
		t.Fatalf("heartbeat effects: %+v", effects)
	}
	record, ok := effects[0].Union.(*application.UpsertDomainRecord)
	if !ok || record.RecordType != "heartbeat" {
		t.Fatalf("heartbeat record: %+v", effects[0])
	}
	var summary callSummary
	if err := json.Unmarshal([]byte(record.DataJSON), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.PendingID != id || summary.Indicator.State != "awaiting_result" || summary.LEDMask != nil {
		t.Fatalf("heartbeat claims an unobserved LED state: %+v", summary)
	}
	fresh := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	if got := readCallSummary(t, fresh.svc); got.PendingID != "" || got.Scope != "current_process_only" {
		t.Fatalf("restart pretended recovery: %+v", got)
	}
	if _, err := fresh.job(jobAcknowledge, mustJSON(map[string]string{"request_id": id}), "old"); err == nil {
		t.Fatal("fresh runtime acknowledged an unrestored request")
	}
	requireNoCallEffects(t, fresh)
	if newID := fresh.request("create"); newID == id {
		t.Fatal("request IDs collide across restarts")
	}
}

func TestCallConcurrentJobsCoalesceAndKeepEffectOrder(t *testing.T) {
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	const workers = 20
	var wg sync.WaitGroup
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := f.job(jobRequest, `{"confirm":true}`, fmt.Sprintf("request-%d", i))
			if err != nil {
				errs <- err
				return
			}
			var result struct {
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal([]byte(resp.ResultJSON), &result); err != nil {
				errs <- err
				return
			}
			ids <- result.RequestID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id string
	for got := range ids {
		if id != "" && got != id {
			t.Fatalf("concurrent calls duplicated: %q vs %q", id, got)
		}
		id = got
	}
	effects := f.writer.take()
	_, cmds := callEffects(t, effects)
	if len(cmds) != 1 || len(effects) != 2 {
		t.Fatalf("concurrent requests emitted duplicate commands: %+v", effects)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.job(jobAcknowledge, mustJSON(map[string]string{"request_id": id}), fmt.Sprintf("ack-%d", i)); err != nil {
				t.Errorf("ack: %v", err)
			}
		}(i)
	}
	wg.Wait()
	effects = append(effects, f.writer.take()...)
	_, cmds = callEffects(t, effects)
	if len(cmds) != 2 || cmds[0].ArgsJSON != `{"mask":255}` || cmds[1].ArgsJSON != `{"mask":0}` {
		t.Fatalf("wrong command order/count: %+v", cmds)
	}
	for i := 1; i < len(effects); i++ {
		if effects[i].Sequence <= effects[i-1].Sequence {
			t.Fatal("effect sequences reordered")
		}
	}
}

func TestServiceCallOverApplicationProtocol(t *testing.T) {
	for _, tc := range []struct{ name, request, ack, indicator string }{
		{"single-board", callKey, key2, ledEntity},
		{"cross-device", "station-a/key", "desk-b/confirm", "panel-c/led"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t, time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC))
			defer a.close()
			defer func() { _, _ = a.cli.Shutdown(a.ctx, &application.ShutdownRequest{}) }()
			if resp := a.configure(`{"mode":"service-call"}`, 1); !resp.Status.IsOK() {
				t.Fatal(resp.Status)
			}
			bindings := []application.Binding{{RequirementID: "button-input", EntityID: tc.request}, {RequirementID: "indicator", EntityID: tc.indicator}, {RequirementID: "acknowledge-input", EntityID: tc.ack}}
			if resp := a.validate(bindings); !resp.Valid {
				t.Fatal(resp.Issues)
			}
			a.openStream()
			a.send(1, &application.CapabilityEvent{RequirementID: "button-input", EntityID: tc.request, EventType: keyPressEvent})
			record, cmds := callEffects(t, a.waitEffects(2, 20*time.Millisecond))
			if record.Source != "button" || record.RequestEntity != tc.request || len(cmds) != 1 || cmds[0].EntityID != tc.indicator || record.IndicatorOn.State != "awaiting_result" {
				t.Fatalf("protocol request: %+v / %+v", record, cmds)
			}
			a.send(2, &application.RequestCompleted{RequestID: cmds[0].IdempotencyKey, EntityID: tc.indicator, Action: ledAction, State: application.CommandStateSucceeded})
			record, _ = callEffects(t, a.waitEffects(1, 20*time.Millisecond))
			if record.Status != "pending" || record.IndicatorOn.State != "succeeded" {
				t.Fatalf("protocol result: %+v", record)
			}
			a.send(3, &application.CapabilityEvent{RequirementID: "acknowledge-input", EntityID: tc.ack, EventType: keyPressEvent})
			record, cmds = callEffects(t, a.waitEffects(2, 20*time.Millisecond))
			if record.Status != "acknowledged" || record.AcknowledgeEntity != tc.ack || len(cmds) != 1 || cmds[0].ArgsJSON != `{"mask":0}` {
				t.Fatalf("protocol ack: %+v / %+v", record, cmds)
			}
			a.send(4, &application.RequestCompleted{RequestID: cmds[0].IdempotencyKey, EntityID: tc.indicator, Action: ledAction, State: application.CommandStateFailed, ErrorCode: "DEVICE_FAILED"})
			record, _ = callEffects(t, a.waitEffects(1, 20*time.Millisecond))
			if record.Status != "acknowledged" || record.IndicatorOff.State != "failed" {
				t.Fatalf("protocol off failure: %+v", record)
			}
			resp, err := a.cli.RunJob(a.ctx, &application.RunJobRequest{PluginInstanceID: testInstance, JobID: jobRequest, ArgsJSON: `{"confirm":true}`, IdempotencyKey: "manual"})
			if err != nil || !resp.Status.IsOK() {
				t.Fatalf("protocol RunJob: %+v / %v", resp, err)
			}
			a.waitEffects(2, 20*time.Millisecond)
		})
	}
}

func TestCallTimeoutRecordFlushesAfterStreamReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
		f.svc.now = time.Now
		id := f.request("create")
		f.writer.take()
		f.svc.mu.Lock()
		delete(f.svc.writers, testInstance)
		f.svc.mu.Unlock()
		time.Sleep(callResultWait + time.Millisecond)
		synctest.Wait()
		if got := readCallSummary(t, f.svc); got.PendingID != id || got.Indicator.State != "result_timeout" {
			t.Fatalf("lost local timeout while stream was down: %+v", got)
		}
		requireNoCallEffects(t, f)
		f.svc.mu.Lock()
		f.svc.writers[testInstance] = f.writer
		f.svc.mu.Unlock()
		f.event(&application.InstanceLifecycle{State: "running"})
		record, commands := callEffects(t, f.writer.take())
		if record == nil || record.RequestID != id || record.IndicatorOn.State != "result_timeout" || len(commands) != 0 {
			t.Fatalf("reconnect should only persist the honest result: %+v / %+v", record, commands)
		}
	})
}

// Omitting pattern from the console schema must not relax runtime validation.
func TestAcknowledgeTextInputValidation(t *testing.T) {
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)
	requestID := f.request("text-input-request")
	f.writer.take()
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"space", " "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"leading-space", " " + requestID},
		{"trailing-space", requestID + " "},
		{"embedded-space", "call- bad"},
		{"embedded-tab", "call-\tbad"},
		{"trailing-newline", requestID + "\n"},
		{"carriage-return", requestID + "\r"},
		{"form-feed", requestID + "\f"},
		{"vertical-tab", requestID + "\v"},
		{"non-breaking-space", requestID + "\u00a0"},
		{"ideographic-space", requestID + "\u3000"},
		{"too-long", strings.Repeat("x", 129)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.job(jobAcknowledge, mustJSON(map[string]string{"request_id": tc.value}), "invalid-"+tc.name)
			var st *status.Status
			if !errors.As(err, &st) || st.Code != status.CodeInvalidArgument {
				t.Fatalf("expected INVALID_ARGUMENT, got %v", err)
			}
			requireNoCallEffects(t, f)
		})
	}
	if got := f.acknowledge(requestID, "text-input-ack"); got != requestID {
		t.Fatalf("acknowledged %q, want generated request ID %q", got, requestID)
	}
}

// 普通用户不该手抄 request_id：这个零参数动作直接解除当前待处理呼叫。
func TestAcknowledgePendingClearsCurrentCallWithoutID(t *testing.T) {
	f := newCallFixture(t, `{"mode":"service-call"}`, callBindings)

	// 没有待处理呼叫时必须 fail-closed，而不是静默成功。
	if _, err := f.job(jobAcknowledgePending, `{}`, "nothing-pending"); err == nil {
		t.Fatal("acknowledge-pending accepted with no pending call")
	}
	// 这个动作不接受任何参数，多余字段必须被拒绝。
	if _, err := f.job(jobAcknowledgePending, `{"request_id":"x"}`, "extra-args"); err == nil {
		t.Fatal("acknowledge-pending accepted unexpected arguments")
	}

	id := f.request("create-pending")
	if got := f.mustJob(jobAcknowledgePending, `{}`, "clear-pending"); got != id {
		t.Fatalf("acknowledge-pending cleared %q, want %q", got, id)
	}
	if _, err := f.job(jobAcknowledgePending, `{}`, "clear-again"); err == nil {
		t.Fatal("acknowledge-pending accepted after the call was already cleared")
	}
}
