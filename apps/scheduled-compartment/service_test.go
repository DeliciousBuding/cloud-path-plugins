package scheduledcompartment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/transport"
)

const (
	testInstance  = "app-1"
	alarmEntityID = "dev/alarm"
	c1            = "dev/compartment-1"
	c2            = "dev/compartment-2"
	c3            = "dev/compartment-3"
)

var validConfigJSON = `{
  "timezone": "Asia/Shanghai",
  "compartments": [
    {"id":"c1","name":"Compartment 1"},
    {"id":"c2","name":"Compartment 2"},
    {"id":"c3","name":"Compartment 3"}
  ],
  "schedule": [
    {"id":"w-morning","compartment":"c1","start":"08:00","end":"08:30"}
  ]
}`

var validBindings = []application.Binding{
	{RequirementID: "reminder-output", EntityID: alarmEntityID},
	{RequirementID: "compartments", EntityID: c1},
	{RequirementID: "compartments", EntityID: c2},
	{RequirementID: "compartments", EntityID: c3},
}

var windowTickJSON = `{"id":"win-1","compartment":"c1","start":"2026-09-03T08:00:00+08:00","end":"2026-09-03T08:30:00+08:00"}`

// --- in-package harness over the real Application Protocol wire ---

type testApp struct {
	t         *testing.T
	svc       *Service
	cli       application.ApplicationClient
	serverEnd transport.Transport
	clientEnd transport.Transport
	serveDone chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	stream    application.ApplicationEventStream
	now       time.Time
}

func newTestApp(t *testing.T, now time.Time) *testApp {
	t.Helper()
	svc := New()
	a := &testApp{
		t:   t,
		svc: svc,
		now: now,
	}
	svc.now = func() time.Time { return a.now }

	serverEnd, clientEnd := transport.Pipe(256)
	rpcServer := application.NewRPCServer(serverEnd, svc)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rpcServer.Serve(context.Background())
	}()
	cli := application.NewClient(clientEnd)
	ctx, cancel := context.WithCancel(context.Background())
	a.serverEnd = serverEnd
	a.clientEnd = clientEnd
	a.serveDone = done
	a.cli = cli
	a.ctx = ctx
	a.cancel = cancel

	if _, err := cli.Initialize(ctx, &application.InitializeRequest{
		PluginID:                  "mock-app",
		PluginVersion:             "0.1.0",
		LaunchID:                  "launch-1",
		HandshakeCookie:           "cookie-1",
		ProtocolVersion:           application.ProtocolVersion,
		SupportedProtocolVersions: []uint32{1},
		NodeID:                    "node-1",
		RuntimeType:               "process",
		HostInfo:                  map[string]string{"os": "windows"},
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return a
}

func (a *testApp) configure(cfgJSON string) *application.ConfigureInstanceResponse {
	a.t.Helper()
	resp, err := a.cli.ConfigureInstance(a.ctx, &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance,
		Config:           []byte(cfgJSON),
		ConfigRevision:   1,
	})
	if err != nil {
		a.t.Fatalf("ConfigureInstance: %v", err)
	}
	return resp
}

func (a *testApp) validate(bindings []application.Binding) *application.ValidateBindingResponse {
	a.t.Helper()
	resp, err := a.cli.ValidateBinding(a.ctx, &application.ValidateBindingRequest{
		PluginInstanceID: testInstance,
		Bindings:         bindings,
	})
	if err != nil {
		a.t.Fatalf("ValidateBinding: %v", err)
	}
	return resp
}

func (a *testApp) openStream() {
	a.t.Helper()
	st, err := a.cli.HandleEvents(a.ctx)
	if err != nil {
		a.t.Fatalf("HandleEvents: %v", err)
	}
	a.stream = st
}

func (a *testApp) send(seq uint64, union application.ApplicationEventUnion) {
	a.t.Helper()
	if err := a.stream.Send(a.ctx, &application.ApplicationEvent{
		PluginInstanceID: testInstance,
		Sequence:         seq,
		SchemaVersion:    "1",
		Union:            union,
	}); err != nil {
		a.t.Fatalf("send event: %v", err)
	}
}

// recvEffects drains effects until the stream goes idle (a short timeout) or
// EOF. It models the fact that an event may produce a variable number of
// effects on a bidi stream.
func (a *testApp) recvEffects(idle time.Duration) []*application.ApplicationEffect {
	a.t.Helper()
	var out []*application.ApplicationEffect
	for {
		rctx, cancel := context.WithTimeout(a.ctx, idle)
		eff, err := a.stream.Recv(rctx)
		cancel()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, context.Canceled) || errors.Is(err, transport.ErrClosed) {
				break
			}
			a.t.Fatalf("recv effect: %v", err)
		}
		out = append(out, eff)
	}
	return out
}

// waitEffects 先条件等待至 least 个效果到达（30s 上限），再按 idle 语义把同批
// 余量收干。慢机/满载下首个效果的到达可远晚于固定 idle 窗（Windows CI 实测
// 60ms 被击穿收到空集）；正断言与阶段间 drain 必须用它。同一事件的效果由服务
// 端连续产出，首个到达后其余紧随其后，idle 收干即可保证不泄漏进下一阶段。
func (a *testApp) waitEffects(least int, idle time.Duration) []*application.ApplicationEffect {
	a.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var out []*application.ApplicationEffect
	for len(out) < least {
		remain := time.Until(deadline)
		if remain <= 0 {
			a.t.Fatalf("waitEffects: 超时只收到 %d/%d 个效果", len(out), least)
		}
		rctx, cancel := context.WithTimeout(a.ctx, remain)
		eff, err := a.stream.Recv(rctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				a.t.Fatalf("waitEffects: 超时只收到 %d/%d 个效果", len(out), least)
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, transport.ErrClosed) {
				a.t.Fatalf("waitEffects: 流已关闭，收到 %d/%d 个效果: %v", len(out), least, err)
			}
			a.t.Fatalf("recv effect: %v", err)
		}
		out = append(out, eff)
	}
	for {
		rctx, cancel := context.WithTimeout(a.ctx, idle)
		eff, err := a.stream.Recv(rctx)
		cancel()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, context.Canceled) || errors.Is(err, transport.ErrClosed) {
				break
			}
			a.t.Fatalf("recv effect: %v", err)
		}
		out = append(out, eff)
	}
	return out
}

func (a *testApp) runJob(jobID, idem string) *application.RunJobResponse {
	a.t.Helper()
	resp, err := a.cli.RunJob(a.ctx, &application.RunJobRequest{
		PluginInstanceID: testInstance,
		JobID:            jobID,
		ArgsJSON:         `{"window_id":"win-1"}`,
		IdempotencyKey:   idem,
	})
	if err != nil {
		a.t.Fatalf("RunJob: %v", err)
	}
	return resp
}

func (a *testApp) close() {
	a.t.Helper()
	a.cancel()
	_ = a.serverEnd.Close()
	_ = a.clientEnd.Close()
	select {
	case <-a.serveDone:
	case <-time.After(5 * time.Second):
		a.t.Error("server did not stop after close")
	}
}

func mustConfigureAndBind(t *testing.T, now time.Time) *testApp {
	t.Helper()
	a := newTestApp(t, now)
	if resp := a.configure(validConfigJSON); !resp.Status.IsOK() {
		t.Fatalf("configure rejected: %s", resp.Status)
	}
	if resp := a.validate(validBindings); !resp.Valid {
		t.Fatalf("bindings rejected: %+v", resp.Issues)
	}
	return a
}

// --- tests ---

func TestDescriptorRequirements(t *testing.T) {
	a := newTestApp(t, time.Now())
	defer a.close()

	desc, err := a.cli.Describe(a.ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ApplicationID != "io.github.deliciousbuding.cloud-path-app-scheduled-compartment" {
		t.Fatalf("application id = %q", desc.ApplicationID)
	}
	if desc.Version != "0.1.0" {
		t.Fatalf("version = %q", desc.Version)
	}
	if desc.DeclarativeOnly {
		t.Fatal("expected process-based application (declarative_only=false)")
	}
	if len(desc.Requirements) != 3 {
		t.Fatalf("requirements = %d, want 3", len(desc.Requirements))
	}
	want := map[string]struct {
		cap  string
		card string
		min  uint32
	}{
		"reminder-output": {"cloudpath.dev/capability/alarm@1", "one", 0},
		"compartments":    {"cloudpath.dev/capability/contact@1", "one-or-more", 3},
		"local-display":   {"cloudpath.dev/capability/display-text@1", "zero-or-one", 0},
	}
	for _, r := range desc.Requirements {
		w, ok := want[r.ID]
		if !ok {
			t.Fatalf("unexpected requirement %q", r.ID)
		}
		if r.Capability != w.cap || r.Cardinality != w.card || r.MinItems != w.min {
			t.Fatalf("requirement %q = (%s,%s,%d), want (%s,%s,%d)",
				r.ID, r.Capability, r.Cardinality, r.MinItems, w.cap, w.card, w.min)
		}
		delete(want, r.ID)
	}
	if len(want) != 0 {
		t.Fatalf("missing requirements: %v", want)
	}
	if len(desc.Jobs) != 1 || desc.Jobs[0].ID != "window-check" {
		t.Fatalf("jobs = %+v, want single window-check job", desc.Jobs)
	}
}

func TestConfigureAndValidateBinding(t *testing.T) {
	a := newTestApp(t, time.Now())
	defer a.close()

	// valid configure
	cfgResp := a.configure(validConfigJSON)
	if !cfgResp.Status.IsOK() {
		t.Fatalf("valid config rejected: %s", cfgResp.Status)
	}
	if cfgResp.AppliedRevision != 1 {
		t.Fatalf("applied revision = %d, want 1", cfgResp.AppliedRevision)
	}

	// valid bindings
	vResp := a.validate(validBindings)
	if !vResp.Valid {
		t.Fatalf("valid bindings rejected: %+v", vResp.Issues)
	}
	if len(vResp.Issues) != 0 {
		t.Fatalf("expected no issues, got %+v", vResp.Issues)
	}

	// missing reminder-output -> invalid
	bad := []application.Binding{
		{RequirementID: "compartments", EntityID: c1},
		{RequirementID: "compartments", EntityID: c2},
		{RequirementID: "compartments", EntityID: c3},
	}
	if r := a.validate(bad); r.Valid {
		t.Fatal("expected missing reminder-output to be invalid")
	}

	// fewer than 3 compartments -> invalid
	bad2 := []application.Binding{
		{RequirementID: "reminder-output", EntityID: alarmEntityID},
		{RequirementID: "compartments", EntityID: c1},
		{RequirementID: "compartments", EntityID: c2},
	}
	if r := a.validate(bad2); r.Valid {
		t.Fatal("expected fewer than 3 compartments to be invalid")
	}

	// unknown requirement -> invalid (structural driver-coupling rejection)
	bad3 := []application.Binding{
		{RequirementID: "reminder-output", EntityID: alarmEntityID},
		{RequirementID: "compartments", EntityID: c1},
		{RequirementID: "compartments", EntityID: c2},
		{RequirementID: "compartments", EntityID: c3},
		{RequirementID: "driver:vendor-device", EntityID: "dev/thing"},
	}
	if r := a.validate(bad3); r.Valid {
		t.Fatal("expected unknown requirement to be invalid")
	} else if len(r.Issues) == 0 {
		t.Fatal("expected at least one issue")
	}

	// empty entity -> invalid
	bad4 := []application.Binding{
		{RequirementID: "reminder-output", EntityID: ""},
		{RequirementID: "compartments", EntityID: c1},
		{RequirementID: "compartments", EntityID: c2},
		{RequirementID: "compartments", EntityID: c3},
	}
	if r := a.validate(bad4); r.Valid {
		t.Fatal("expected empty entity_id to be invalid")
	}
}

func TestWindowReminderEffect(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream()
	a.send(1, &application.ScheduleTick{ScheduleID: "s-1", OccurredAt: "2026-09-03T08:00:00+08:00", WindowJSON: windowTickJSON})
	effects := a.waitEffects(3, 60*time.Millisecond)

	var gotRequest *application.RequestCommand
	var gotUpsert bool
	for _, e := range effects {
		if u, ok := e.Union.(*application.RequestCommand); ok {
			gotRequest = u
		}
		if u, ok := e.Union.(*application.UpsertDomainRecord); ok && u.RecordType == "window" {
			gotUpsert = true
		}
	}
	if gotRequest == nil {
		t.Fatal("expected a RequestCommand(alarm) effect")
	}
	if gotRequest.EntityID != alarmEntityID {
		t.Fatalf("request entity = %q, want %q", gotRequest.EntityID, alarmEntityID)
	}
	if gotRequest.Action != "trigger" {
		t.Fatalf("action = %q, want trigger", gotRequest.Action)
	}
	if gotRequest.IdempotencyKey != "reminder-win-1" {
		t.Fatalf("idempotency = %q, want reminder-win-1", gotRequest.IdempotencyKey)
	}
	if !gotUpsert {
		t.Fatal("expected a window UpsertDomainRecord effect")
	}
	if got := windowStateOf(effects, "win-1"); got != windowOpened {
		t.Fatalf("window state = %q, want %q", got, windowOpened)
	}
}

func TestContactCompletesWindow(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream()
	a.send(1, &application.ScheduleTick{ScheduleID: "s-1", OccurredAt: "2026-09-03T08:00:00+08:00", WindowJSON: windowTickJSON})
	_ = a.waitEffects(3, 60*time.Millisecond)

	a.send(2, &application.CapabilityEvent{
		RequirementID: "compartments",
		EntityID:      c1,
		EventType:     "cloudpath.dev/capability/contact@1/opened",
		OccurredAt:    "2026-09-03T08:05:00+08:00",
	})
	effects := a.waitEffects(2, 60*time.Millisecond)
	if got := windowStateOf(effects, "win-1"); got != windowCompleted {
		t.Fatalf("window state = %q, want %q", got, windowCompleted)
	}
	if !hasCancelTask(effects, "window-check-win-1") {
		t.Fatal("expected a CancelScheduledTask for the completed window")
	}
}

func TestMissedWindowRecord(t *testing.T) {
	start := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	a := mustConfigureAndBind(t, start)
	defer a.close()
	a.openStream()
	a.send(1, &application.ScheduleTick{ScheduleID: "s-1", OccurredAt: "2026-09-03T08:00:00+08:00", WindowJSON: windowTickJSON})
	_ = a.waitEffects(3, 60*time.Millisecond)

	// advance the clock past the window end and run the window-check job
	a.now = time.Date(2026, 9, 3, 8, 31, 0, 0, time.UTC)
	resp := a.runJob("window-check", "job-missed-1")
	if !resp.Status.IsOK() {
		t.Fatalf("RunJob status: %s", resp.Status)
	}
	if !strings.Contains(resp.ResultJSON, "win-1") {
		t.Fatalf("result %s does not mention win-1", resp.ResultJSON)
	}
	effects := a.waitEffects(3, 60*time.Millisecond)
	if got := windowStateOf(effects, "win-1"); got != windowMissed {
		t.Fatalf("window state = %q, want %q", got, windowMissed)
	}
	if !hasCancelTask(effects, "window-check-win-1") {
		t.Fatal("expected CancelScheduledTask for missed window")
	}
	if !hasNotification(effects) {
		t.Fatal("expected a SendNotification for the missed window")
	}

	// idempotency: a second RunJob with the same key must not re-emit effects
	resp2 := a.runJob("window-check", "job-missed-1")
	if resp2.ResultJSON != resp.ResultJSON {
		t.Fatalf("idempotent result mismatch: %s vs %s", resp2.ResultJSON, resp.ResultJSON)
	}
	if dup := a.recvEffects(40 * time.Millisecond); len(dup) != 0 {
		t.Fatalf("duplicate RunJob emitted %d effects", len(dup))
	}
}

func TestDuplicateEventIdempotent(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream()

	a.send(1, &application.ScheduleTick{ScheduleID: "s-1", OccurredAt: "2026-09-03T08:00:00+08:00", WindowJSON: windowTickJSON})
	start := a.waitEffects(3, 60*time.Millisecond)
	if n := countRequestCommand(start); n != 1 {
		t.Fatalf("window start emitted %d RequestCommand, want 1", n)
	}

	// duplicate same sequence, then re-delivery with a new sequence but the same
	// window id. Neither should emit anything.
	a.send(1, &application.ScheduleTick{ScheduleID: "s-1", OccurredAt: "2026-09-03T08:00:00+08:00", WindowJSON: windowTickJSON})
	a.send(2, &application.ScheduleTick{ScheduleID: "s-1", OccurredAt: "2026-09-03T08:00:00+08:00", WindowJSON: windowTickJSON})

	a.send(3, &application.CapabilityEvent{
		RequirementID: "compartments", EntityID: c1, EventType: "cloudpath.dev/capability/contact@1/opened",
		OccurredAt: "2026-09-03T08:05:00+08:00",
	})
	after := a.waitEffects(2, 60*time.Millisecond)
	if n := countRequestCommand(after); n != 0 {
		t.Fatalf("duplicate events emitted %d additional RequestCommand, want 0", n)
	}
	if got := windowStateOf(after, "win-1"); got != windowCompleted {
		t.Fatalf("window state after complete = %q, want %q", got, windowCompleted)
	}

	// a duplicate contact event after completion must not re-complete
	a.send(4, &application.CapabilityEvent{
		RequirementID: "compartments", EntityID: c1, EventType: "cloudpath.dev/capability/contact@1/opened",
		OccurredAt: "2026-09-03T08:06:00+08:00",
	})
	if dup := a.recvEffects(40 * time.Millisecond); len(dup) != 0 {
		t.Fatalf("duplicate contact after completion emitted %d effects", len(dup))
	}
}

func TestRejectDriverCoupling(t *testing.T) {
	a := newTestApp(t, time.Now())
	defer a.close()

	desc, err := a.cli.Describe(a.ctx)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	// A device-agnostic application only consumes public capability
	// requirements. Asserting the closed allowed set makes it structurally
	// impossible to couple to a Driver requirement.
	for _, r := range desc.Requirements {
		switch r.Capability {
		case alarmCap, contactCap, displayCap:
		default:
			t.Fatalf("requirement %s uses undeclared capability %s", r.ID, r.Capability)
		}
	}

	// Bindings must be for declared capability requirements only; a driver
	// requirement id is rejected, so the application can never couple to one.
	resp := a.validate([]application.Binding{
		{RequirementID: "reminder-output", EntityID: alarmEntityID},
		{RequirementID: "compartments", EntityID: c1},
		{RequirementID: "compartments", EntityID: c2},
		{RequirementID: "compartments", EntityID: c3},
		{RequirementID: "driver:vendor-device", EntityID: "dev/thing"},
	})
	if resp.Valid {
		t.Fatal("expected driver requirement to be rejected")
	}
	found := false
	for _, issue := range resp.Issues {
		if issue.RequirementID == "driver:vendor-device" && strings.Contains(issue.Message, "not declared") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 'not declared' issue for the driver requirement, got %+v", resp.Issues)
	}
}

func TestInvalidScheduleConfig(t *testing.T) {
	a := newTestApp(t, time.Now())
	defer a.close()

	invalid := []string{
		// missing timezone
		`{"compartments":[{"id":"c1"},{"id":"c2"},{"id":"c3"}],"schedule":[{"id":"w1","compartment":"c1","start":"08:00","end":"08:30"}]}`,
		// invalid timezone name
		`{"timezone":"Not/AZone","compartments":[{"id":"c1"},{"id":"c2"},{"id":"c3"}],"schedule":[{"id":"w1","compartment":"c1","start":"08:00","end":"08:30"}]}`,
		// missing compartments
		`{"timezone":"Asia/Shanghai","schedule":[{"id":"w1","compartment":"c1","start":"08:00","end":"08:30"}]}`,
		// duplicate compartment id
		`{"timezone":"Asia/Shanghai","compartments":[{"id":"c1"},{"id":"c1"},{"id":"c2"}],"schedule":[{"id":"w1","compartment":"c1","start":"08:00","end":"08:30"}]}`,
		// missing schedule
		`{"timezone":"Asia/Shanghai","compartments":[{"id":"c1"},{"id":"c2"},{"id":"c3"}]}`,
		// window references unknown compartment
		`{"timezone":"Asia/Shanghai","compartments":[{"id":"c1"},{"id":"c2"},{"id":"c3"}],"schedule":[{"id":"w1","compartment":"nope","start":"08:00","end":"08:30"}]}`,
		// invalid HH:MM
		`{"timezone":"Asia/Shanghai","compartments":[{"id":"c1"},{"id":"c2"},{"id":"c3"}],"schedule":[{"id":"w1","compartment":"c1","start":"25:00","end":"08:30"}]}`,
		// end before start
		`{"timezone":"Asia/Shanghai","compartments":[{"id":"c1"},{"id":"c2"},{"id":"c3"}],"schedule":[{"id":"w1","compartment":"c1","start":"08:30","end":"08:00"}]}`,
		// malformed JSON
		`{`,
	}
	for i, cfg := range invalid {
		resp := a.configure(cfg)
		if resp.Status.IsOK() {
			t.Fatalf("case %d: invalid config accepted", i)
		}
		if resp.AppliedRevision != 0 {
			t.Fatalf("case %d: applied revision = %d, want 0", i, resp.AppliedRevision)
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	a := mustConfigureAndBind(t, time.Now())
	defer a.close()

	sh, err := a.cli.Shutdown(a.ctx, &application.ShutdownRequest{Reason: "host close", GraceSeconds: 1})
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !sh.Status.IsOK() {
		t.Fatalf("shutdown status: %s", sh.Status)
	}

	health, err := a.cli.Health(a.ctx)
	if err != nil {
		t.Fatalf("Health after shutdown: %v", err)
	}
	if health.State != application.HealthStateNotServing {
		t.Fatalf("health state after shutdown = %v, want NotServing", health.State)
	}
}

// --- helpers ---

func windowStateOf(effects []*application.ApplicationEffect, id string) string {
	for _, e := range effects {
		u, ok := e.Union.(*application.UpsertDomainRecord)
		if !ok || u.RecordType != "window" || u.RecordID != id {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(u.DataJSON), &m) != nil {
			continue
		}
		if s, ok := m["state"].(string); ok {
			return s
		}
	}
	return ""
}

func countRequestCommand(effects []*application.ApplicationEffect) int {
	n := 0
	for _, e := range effects {
		if _, ok := e.Union.(*application.RequestCommand); ok {
			n++
		}
	}
	return n
}

func hasCancelTask(effects []*application.ApplicationEffect, scheduleID string) bool {
	for _, e := range effects {
		if u, ok := e.Union.(*application.CancelScheduledTask); ok && u.ScheduleID == scheduleID {
			return true
		}
	}
	return false
}

func hasNotification(effects []*application.ApplicationEffect) bool {
	for _, e := range effects {
		if _, ok := e.Union.(*application.SendNotification); ok {
			return true
		}
	}
	return false
}
