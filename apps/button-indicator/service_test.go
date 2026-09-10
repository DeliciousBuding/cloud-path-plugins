// SPDX-License-Identifier: Apache-2.0

package buttonindicator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/transport"
)

const (
	testInstance = "bi-1"
	key1         = "dev/key-1"
	key2         = "dev/key-2"
	ledEntity    = "dev/led-bank"
	buzzerEntity = "dev/buzzer"
)

var validConfigJSON = `{
  "timezone": "Asia/Shanghai",
  "heartbeat_cron": "*/5 * * * *"
}`

var validBindings = []application.Binding{
	{RequirementID: "button-input", EntityID: key1},
	{RequirementID: "button-input", EntityID: key2},
	{RequirementID: "indicator", EntityID: ledEntity},
}

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
		PluginID:                  pluginIDValue,
		PluginVersion:             pluginVersion,
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

func (a *testApp) configure(cfgJSON string, rev uint32) *application.ConfigureInstanceResponse {
	a.t.Helper()
	resp, err := a.cli.ConfigureInstance(a.ctx, &application.ConfigureInstanceRequest{
		PluginInstanceID: testInstance,
		Config:           []byte(cfgJSON),
		ConfigRevision:   rev,
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
	// 模拟 Core v0.2.9+ 平台行为：流开启即派发初始 lifecycle 事件
	// （Sequence 0 不占用应用的去重序号），应用由此预注册 effect writer。
	if err := st.Send(a.ctx, &application.ApplicationEvent{
		PluginInstanceID: testInstance,
		Sequence:         0,
		SchemaVersion:    "1",
		Union:            &application.InstanceLifecycle{State: "running"},
	}); err != nil {
		a.t.Fatalf("send initial lifecycle: %v", err)
	}
	// 等待服务端消费该事件完成 writer 注册：RunJob 是独立 RPC，可能赶在
	// 事件消费前到达（无此等待，bootstrap 测试会稳定复现该竞态）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.svc.mu.Lock()
		ready := a.svc.writers[testInstance] != nil
		a.svc.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatal("service never registered the effect writer from the initial lifecycle event")
		}
		time.Sleep(2 * time.Millisecond)
	}
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

// waitEffects waits for at least `least` effects (30s cap), then drains the
// same batch until idle. See the scheduled-compartment harness for the timing
// rationale (a fixed idle window alone loses effects on loaded CI machines).
func (a *testApp) waitEffects(least int, idle time.Duration) []*application.ApplicationEffect {
	a.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var out []*application.ApplicationEffect
	for len(out) < least {
		remain := time.Until(deadline)
		if remain <= 0 {
			a.t.Fatalf("waitEffects: timeout with %d/%d effects", len(out), least)
		}
		rctx, cancel := context.WithTimeout(a.ctx, remain)
		eff, err := a.stream.Recv(rctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				a.t.Fatalf("waitEffects: timeout with %d/%d effects", len(out), least)
			}
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, transport.ErrClosed) {
				a.t.Fatalf("waitEffects: stream closed with %d/%d effects", len(out), least)
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
			break
		}
		out = append(out, eff)
	}
	return out
}

// drainEffects collects whatever effects arrive within the idle window,
// expecting none. Used for negative assertions: waitEffects(1, ...) would
// block forever when the expected count is zero.
func (a *testApp) drainEffects(idle time.Duration) []*application.ApplicationEffect {
	a.t.Helper()
	var out []*application.ApplicationEffect
	deadline := time.Now().Add(idle)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return out
		}
		rctx, cancel := context.WithTimeout(a.ctx, remain)
		eff, err := a.stream.Recv(rctx)
		cancel()
		if err != nil {
			return out
		}
		out = append(out, eff)
	}
}

func (a *testApp) runJob(jobID, key string) *application.RunJobResponse {
	a.t.Helper()
	resp, err := a.cli.RunJob(a.ctx, &application.RunJobRequest{
		PluginInstanceID: testInstance,
		JobID:            jobID,
		IdempotencyKey:   key,
	})
	if err != nil {
		a.t.Fatalf("RunJob(%s): %v", jobID, err)
	}
	return resp
}

func (a *testApp) close() {
	a.t.Helper()
	a.cancel()
	a.clientEnd.Close()
	a.serverEnd.Close()
	<-a.serveDone
}

func mustConfigureAndBind(t *testing.T, now time.Time) *testApp {
	t.Helper()
	a := newTestApp(t, now)
	if resp := a.configure(validConfigJSON, 1); !resp.Status.IsOK() {
		t.Fatalf("configure: %v", resp.Status)
	}
	if resp := a.validate(validBindings); !resp.Valid {
		t.Fatalf("validate: %+v", resp.Issues)
	}
	return a
}

// --- config ---

func TestConfigValidation(t *testing.T) {
	if _, err := UnmarshalConfig([]byte(`{"timezone":"Mars/Olympus"}`)); err == nil {
		t.Fatal("bad timezone accepted")
	}
	if _, err := UnmarshalConfig([]byte(`{"heartbeat_cron":"* * * *"}`)); err == nil {
		t.Fatal("4-field cron accepted")
	}
	cfg, err := UnmarshalConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if cfg.ResolvedHeartbeatCron() != "*/5 * * * *" {
		t.Fatalf("default cron = %q", cfg.ResolvedHeartbeatCron())
	}
}

// --- bindings ---

func TestBindingValidation(t *testing.T) {
	svc := New()
	// unknown requirement id is rejected (rules out Driver coupling)
	resp, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		Bindings: []application.Binding{
			{RequirementID: "button-input", EntityID: key1},
			{RequirementID: "indicator", EntityID: ledEntity},
			{RequirementID: "compartments", EntityID: "x"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Valid {
		t.Fatal("unknown requirement accepted")
	}
	// missing indicator is rejected
	resp, _ = svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		Bindings: []application.Binding{{RequirementID: "button-input", EntityID: key1}},
	})
	if resp.Valid {
		t.Fatal("missing indicator accepted")
	}
	// duplicate entity rejected
	resp, _ = svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{
		Bindings: []application.Binding{
			{RequirementID: "button-input", EntityID: key1},
			{RequirementID: "button-input", EntityID: key1},
			{RequirementID: "indicator", EntityID: ledEntity},
		},
	})
	if resp.Valid {
		t.Fatal("duplicate entity accepted")
	}
}

// --- key press: the walking light ---

func TestKeyPressWalkingLight(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream()

	press := func(seq uint64, entity string) {
		a.send(seq, &application.CapabilityEvent{
			RequirementID: "button-input", EntityID: entity, EventType: keyPressEvent,
		})
	}

	// 三次按键：LED 掩码走马灯 1 → 2 → 4，每按都写 press 记录
	for i, wantMask := range []int{1, 2, 4} {
		press(uint64(i+1), key1)
		effects := a.waitEffects(2, 60*time.Millisecond)
		if len(effects) != 2 {
			t.Fatalf("press %d: effects = %d, want 2 (record + led)", i+1, len(effects))
		}
		var record *application.UpsertDomainRecord
		var cmd *application.RequestCommand
		for _, e := range effects {
			switch u := e.Union.(type) {
			case *application.UpsertDomainRecord:
				record = u
			case *application.RequestCommand:
				cmd = u
			}
		}
		if record == nil || cmd == nil {
			t.Fatalf("press %d: missing record or command: %+v", i+1, effects)
		}
		if record.RecordType != "press" || record.RecordID != "last" {
			t.Fatalf("press %d: record = %s/%s", i+1, record.RecordType, record.RecordID)
		}
		var data struct {
			Count  int    `json:"count"`
			Mask   int    `json:"mask"`
			Entity string `json:"entity"`
		}
		if err := json.Unmarshal([]byte(record.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		if data.Count != i+1 || data.Mask != wantMask || data.Entity != key1 {
			t.Fatalf("press %d: record data = %+v, want count=%d mask=%d", i+1, data, i+1, wantMask)
		}
		if cmd.EntityID != ledEntity || cmd.Action != "led" {
			t.Fatalf("press %d: cmd = %s/%s", i+1, cmd.EntityID, cmd.Action)
		}
		var args struct {
			Mask int `json:"mask"`
		}
		if err := json.Unmarshal([]byte(cmd.ArgsJSON), &args); err != nil {
			t.Fatal(err)
		}
		if args.Mask != wantMask {
			t.Fatalf("press %d: led mask = %d, want %d", i+1, args.Mask, wantMask)
		}
	}

	// 第二个按键实体同样有效（count 继续走）
	a.send(4, &application.CapabilityEvent{RequirementID: "button-input", EntityID: key2, EventType: keyPressEvent})
	effects := a.waitEffects(2, 60*time.Millisecond)
	var record *application.UpsertDomainRecord
	for _, e := range effects {
		if u, ok := e.Union.(*application.UpsertDomainRecord); ok {
			record = u
		}
	}
	var data struct {
		Count  int    `json:"count"`
		Entity string `json:"entity"`
	}
	_ = json.Unmarshal([]byte(record.DataJSON), &data)
	if data.Count != 4 || data.Entity != key2 {
		t.Fatalf("key2 press: %+v", data)
	}

	// 未绑定实体的事件被忽略（无 effect）
	a.send(5, &application.CapabilityEvent{RequirementID: "button-input", EntityID: "dev/key-9", EventType: keyPressEvent})
	if effects := a.drainEffects(150 * time.Millisecond); len(effects) != 0 {
		t.Fatalf("unbound entity produced %d effects", len(effects))
	}
}

// --- bootstrap: declarative heartbeat registration ---

func TestBootstrapRegistersHeartbeatOncePerRevision(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream()

	// 第一次 bootstrap → 声明 ScheduleTask
	a.runJob(jobBootstrap, "bs-1")
	effects := a.waitEffects(1, 60*time.Millisecond)
	if len(effects) != 1 {
		t.Fatalf("bootstrap #1 effects = %d, want 1", len(effects))
	}
	task, ok := effects[0].Union.(*application.ScheduleTask)
	if !ok {
		t.Fatalf("bootstrap #1 union = %T", effects[0].Union)
	}
	if task.ScheduleID != heartbeatCaps || task.Cron != "*/5 * * * *" {
		t.Fatalf("schedule task = %+v", task)
	}

	// 同 revision 再跑 bootstrap（不同幂等键）→ 不重复声明
	a.runJob(jobBootstrap, "bs-2")
	if effects := a.drainEffects(150 * time.Millisecond); len(effects) != 0 {
		t.Fatalf("bootstrap #2 re-declared: %d effects", len(effects))
	}

	// 重配置（revision 变化）→ 重新声明
	a.configure(`{"timezone":"Asia/Shanghai","heartbeat_cron":"*/2 * * * *"}`, 2)
	a.runJob(jobBootstrap, "bs-3")
	effects = a.waitEffects(1, 60*time.Millisecond)
	if len(effects) != 1 {
		t.Fatalf("bootstrap after reconfigure effects = %d, want 1", len(effects))
	}
	task, ok = effects[0].Union.(*application.ScheduleTask)
	if !ok || task.Cron != "*/2 * * * *" {
		t.Fatalf("redeclared task = %+v", task)
	}
}

// --- heartbeat: the scheduled job writes a domain record ---

func TestHeartbeatJobWritesRecord(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream()

	// 先按两下，heartbeat 记录里应带上当前计数
	a.send(1, &application.CapabilityEvent{RequirementID: "button-input", EntityID: key1, EventType: keyPressEvent})
	a.waitEffects(2, 60*time.Millisecond)

	a.runJob(jobHeartbeat, "hb-1")
	effects := a.waitEffects(1, 60*time.Millisecond)
	if len(effects) != 1 {
		t.Fatalf("heartbeat effects = %d, want 1", len(effects))
	}
	rec, ok := effects[0].Union.(*application.UpsertDomainRecord)
	if !ok {
		t.Fatalf("heartbeat union = %T", effects[0].Union)
	}
	if rec.RecordType != "heartbeat" || rec.RecordID != "last" {
		t.Fatalf("heartbeat record = %s/%s", rec.RecordType, rec.RecordID)
	}
	var data struct {
		PressCount int    `json:"press_count"`
		At         string `json:"at"`
	}
	if err := json.Unmarshal([]byte(rec.DataJSON), &data); err != nil {
		t.Fatal(err)
	}
	if data.PressCount != 1 {
		t.Fatalf("heartbeat press_count = %d, want 1", data.PressCount)
	}

	// 同幂等键重放 → 不重复
	a.runJob(jobHeartbeat, "hb-1")
	if effects := a.drainEffects(150 * time.Millisecond); len(effects) != 0 {
		t.Fatalf("duplicate heartbeat produced %d effects", len(effects))
	}
}

// --- multi-instance isolation ---

func TestMultiInstanceEffectRouting(t *testing.T) {
	a := mustConfigureAndBind(t, time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	defer a.close()
	a.openStream() // bi-1 的流

	const inst2 = "bi-2"
	if _, err := a.cli.ConfigureInstance(a.ctx, &application.ConfigureInstanceRequest{
		PluginInstanceID: inst2, Config: []byte(validConfigJSON), ConfigRevision: 1,
	}); err != nil {
		t.Fatalf("configure bi-2: %v", err)
	}
	if _, err := a.cli.ValidateBinding(a.ctx, &application.ValidateBindingRequest{
		PluginInstanceID: inst2, Bindings: validBindings,
	}); err != nil {
		t.Fatalf("validate bi-2: %v", err)
	}
	stream2, err := a.cli.HandleEvents(a.ctx)
	if err != nil {
		t.Fatalf("open stream bi-2: %v", err)
	}

	// bi-2 的按键（先发，制造「后开的流」）
	if err := stream2.Send(a.ctx, &application.ApplicationEvent{
		PluginInstanceID: inst2, Sequence: 1, SchemaVersion: "1",
		Union: &application.CapabilityEvent{RequirementID: "button-input", EntityID: key1, EventType: keyPressEvent},
	}); err != nil {
		t.Fatalf("send bi-2 press: %v", err)
	}
	// bi-1 的按键
	a.send(1, &application.CapabilityEvent{RequirementID: "button-input", EntityID: key1, EventType: keyPressEvent})

	// bi-1 的 2 个 effect 全部带 PluginInstanceID=bi-1 且到 bi-1 的流
	for _, e := range a.waitEffects(2, 60*time.Millisecond) {
		if e.PluginInstanceID != testInstance {
			t.Fatalf("bi-1 stream received effect for %q", e.PluginInstanceID)
		}
	}
	// bi-2 的 2 个 effect 在 stream2 上
	var got2 int
	deadline := time.Now().Add(5 * time.Second)
	for got2 < 2 {
		rctx, cancel := context.WithTimeout(a.ctx, time.Until(deadline))
		eff, err := stream2.Recv(rctx)
		cancel()
		if err != nil {
			t.Fatalf("recv bi-2 effect: %v", err)
		}
		if eff.PluginInstanceID != inst2 {
			t.Fatalf("bi-2 stream received effect for %q", eff.PluginInstanceID)
		}
		got2++
	}
}
