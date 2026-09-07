package environmentguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

type captureWriter struct {
	mu      sync.Mutex
	effects []*application.ApplicationEffect
	fail    func(*application.ApplicationEffect) error
}

func (w *captureWriter) Send(ctx context.Context, effect *application.ApplicationEffect) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		if err := w.fail(effect); err != nil {
			return err
		}
	}
	w.effects = append(w.effects, effect)
	return nil
}
func (w *captureWriter) all() []*application.ApplicationEffect {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*application.ApplicationEffect{}, w.effects...)
}
func (w *captureWriter) records(kind string) []*application.UpsertDomainRecord {
	var records []*application.UpsertDomainRecord
	for _, effect := range w.all() {
		if r, ok := effect.Union.(*application.UpsertDomainRecord); ok && r.RecordType == kind {
			records = append(records, r)
		}
	}
	return records
}
func (w *captureWriter) schedules() []*application.ScheduleTask {
	var tasks []*application.ScheduleTask
	for _, effect := range w.all() {
		if task, ok := effect.Union.(*application.ScheduleTask); ok {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

type eventRequest struct {
	ev  *application.ApplicationEvent
	ack chan struct{}
}
type eventPipe struct {
	in       chan eventRequest
	previous chan struct{}
}

func (p *eventPipe) Recv(ctx context.Context) (*application.ApplicationEvent, error) {
	if p.previous != nil {
		close(p.previous)
		p.previous = nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case req, ok := <-p.in:
		if !ok {
			return nil, io.EOF
		}
		p.previous = req.ack
		return req.ev, nil
	}
}

type harness struct {
	t              *testing.T
	svc            *Service
	clock          *testClock
	id             string
	writer         *captureWriter
	pipe           *eventPipe
	done           chan error
	cancel         context.CancelFunc
	sequence       uint64
	observationSeq map[string]int64
	stopped        bool
}

func newTestService(t *testing.T) (*Service, *testClock) {
	t.Helper()
	clock := &testClock{at: time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)}
	svc := New()
	svc.now = clock.now
	if _, err := svc.Initialize(context.Background(), &application.InitializeRequest{ProtocolVersion: application.ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	return svc, clock
}

func configure(t *testing.T, svc *Service, id, config string, rev uint32) {
	t.Helper()
	resp, err := svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: id, Config: []byte(config), ConfigRevision: rev})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Status.IsOK() || resp.AppliedRevision != rev {
		t.Fatalf("configure = %+v", resp)
	}
}

func bind(t *testing.T, svc *Service, id string) {
	t.Helper()
	resp, err := svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: id, Bindings: []application.Binding{
		{RequirementID: TemperatureRequirement, EntityID: "sensor-temperature"},
		{RequirementID: IlluminanceRequirement, EntityID: "sensor-illuminance"},
	}})
	if err != nil || !resp.Valid {
		t.Fatalf("binding = %+v, %v", resp, err)
	}
}

func openHarness(t *testing.T, svc *Service, clock *testClock, id string) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, svc: svc, clock: clock, id: id, writer: &captureWriter{}, pipe: &eventPipe{in: make(chan eventRequest)}, done: make(chan error, 1), cancel: cancel, observationSeq: map[string]int64{}}
	go func() { h.done <- svc.HandleEvents(ctx, h.pipe, h.writer) }()
	t.Cleanup(func() { h.stop() })
	h.send(&application.InstanceLifecycle{State: "running"})
	return h
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	svc, clock := newTestService(t)
	configure(t, svc, "desk", "{}", 1)
	bind(t, svc, "desk")
	return openHarness(t, svc, clock, "desk")
}

func (h *harness) stop() {
	h.cancel()
	if !h.stopped {
		select {
		case err := <-h.done:
			if err != nil {
				h.t.Errorf("stream: %v", err)
			}
		case <-time.After(3 * time.Second):
			h.t.Error("stream did not close")
		}
		h.stopped = true
	}
}
func (h *harness) sendError(ev *application.ApplicationEvent) error {
	ack := make(chan struct{})
	select {
	case h.pipe.in <- eventRequest{ev: ev, ack: ack}:
	case err := <-h.done:
		h.stopped = true
		return err
	case <-time.After(3 * time.Second):
		return fmt.Errorf("stream send timeout")
	}
	select {
	case <-ack:
		return nil
	case err := <-h.done:
		h.stopped = true
		return err
	case <-time.After(3 * time.Second):
		return fmt.Errorf("event processing timeout")
	}
}
func (h *harness) send(union application.ApplicationEventUnion) {
	h.t.Helper()
	h.sequence++
	if err := h.sendError(&application.ApplicationEvent{PluginInstanceID: h.id, Sequence: h.sequence, SchemaVersion: application.SchemaVersion, Union: union}); err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) observation(role string, value float64) map[string]any {
	h.observationSeq[role]++
	return map[string]any{"capability": capability(role), "property": "value", "value": value, "quality": "good", "observed_at": h.clock.now().Format(time.RFC3339Nano), "received_at": h.clock.now().Format(time.RFC3339Nano), "sequence": h.observationSeq[role]}
}
func (h *harness) raw(role, payload string) {
	h.t.Helper()
	h.send(&application.CapabilityEvent{RequirementID: role, EntityID: "sensor-" + role, EventType: PropertyObservedEvent, PayloadJSON: payload})
}
func (h *harness) observe(role string, value float64) {
	h.t.Helper()
	h.raw(role, jsonText(h.observation(role, value)))
}
func (h *harness) good() {
	h.observe(TemperatureRequirement, 24)
	h.observe(IlluminanceRequirement, 60)
}
func (h *harness) job(job, key string) *application.RunJobResponse {
	h.t.Helper()
	result, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: h.id, JobID: job, ArgsJSON: "{}", IdempotencyKey: key})
	if err != nil || !result.Status.IsOK() {
		h.t.Fatalf("job %s = %+v, %v", job, result, err)
	}
	return result
}
func (h *harness) snapshot() EnvironmentRecord {
	h.t.Helper()
	response, err := h.svc.HandleRequest(context.Background(), &application.PluginHTTPRequest{PluginInstanceID: h.id, Context: application.RequestContext{InstanceID: h.id}, Method: "GET", Path: "/status"})
	if err != nil {
		h.t.Fatal(err)
	}
	var body struct {
		Environment EnvironmentRecord `json:"environment"`
		Sampled     bool              `json:"sampled"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		h.t.Fatal(err)
	}
	if response.StatusCode != 200 || body.Sampled {
		h.t.Fatalf("status response = %s", response.Body)
	}
	return body.Environment
}
func assertValue(t *testing.T, v SensorReading, want float64) {
	t.Helper()
	if v.Value == nil || *v.Value != want {
		t.Fatalf("value = %+v, want %v", v, want)
	}
}
func assertStatus(t *testing.T, r EnvironmentRecord, want string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status=%s want=%s; %+v", r.Status, want, r)
	}
}

func TestWarmAndLightThresholdHysteresisAndBoundedAlertSlots(t *testing.T) {
	h := newHarness(t)
	configure(t, h.svc, h.id, `{"light_threshold":30,"light_alert_when":"below"}`, 2)
	h.observe(TemperatureRequirement, 30)
	h.observe(IlluminanceRequirement, 20)
	r := h.snapshot()
	assertStatus(t, r, "attention")
	if r.Temperature.Status != "warm" || r.Illuminance.Status != "below_threshold" {
		t.Fatalf("states=%+v", r)
	}
	if len(h.writer.records("alert")) != 2 {
		t.Fatal("temperature and numeric light condition must each enter once")
	}
	for i, temp := range []float64{29, 28.1, 28, 27.5, 27.01} {
		h.clock.advance(5 * time.Second)
		h.observe(TemperatureRequirement, temp)
		h.observe(IlluminanceRequirement, []float64{29, 30, 31, 34, 34.99}[i])
	}
	if len(h.writer.records("alert")) != 2 {
		t.Fatal("threshold jitter generated duplicate history")
	}
	h.clock.advance(5 * time.Second)
	h.observe(TemperatureRequirement, 27)
	h.observe(IlluminanceRequirement, 35)
	assertStatus(t, h.snapshot(), "within_thresholds")
	if len(h.writer.records("alert")) != 4 {
		t.Fatal("inclusive hysteresis boundaries must recover both conditions")
	}
	h.observe(TemperatureRequirement, 17)
	h.observe(TemperatureRequirement, 19)
	if len(h.writer.records("alert")) != 6 {
		t.Fatal("cold condition did not enter/recover")
	}
	for range 50 {
		h.observe(TemperatureRequirement, 30)
		h.observe(TemperatureRequirement, 27)
		h.observe(IlluminanceRequirement, 20)
		h.observe(IlluminanceRequirement, 35)
	}
	ids := map[string]bool{}
	for _, record := range h.writer.records("alert") {
		ids[record.RecordID] = true
		var alert AlertRecord
		if err := json.Unmarshal([]byte(record.DataJSON), &alert); err != nil {
			t.Fatal(err)
		}
		if alert.Title == "" || alert.Summary == "" || alert.ObservedAt == nil || alert.Value == nil {
			t.Fatalf("not a usable alert: %+v", alert)
		}
	}
	if len(ids) != 6 {
		t.Fatalf("durable alert cardinality=%d, want six fixed latest-transition slots", len(ids))
	}
	for _, effect := range h.writer.all() {
		switch effect.Union.(type) {
		case *application.ScheduleTask, *application.UpsertDomainRecord:
		default:
			t.Fatalf("forbidden side effect: %T", effect.Union)
		}
	}
}

func TestCurrentSnapshotThrottleAndImmediateStateChanges(t *testing.T) {
	h := newHarness(t)
	if len(h.writer.records("environment")) != 1 {
		t.Fatal("initial unknown must be written immediately")
	}
	h.good()
	if len(h.writer.records("environment")) != 3 {
		t.Fatal("each first observation must publish immediately")
	}
	for i := 1; i <= 5; i++ {
		h.clock.advance(5 * time.Second)
		h.observe(TemperatureRequirement, 24+float64(i)/10)
	}
	if got := len(h.writer.records("environment")); got != 3 {
		t.Fatalf("numeric updates bypassed 30s limit: %d", got)
	}
	h.clock.advance(5 * time.Second)
	h.observe(TemperatureRequirement, 25)
	if len(h.writer.records("environment")) != 4 {
		t.Fatal("latest reading should publish at 30s")
	}
	h.clock.advance(time.Second)
	h.observe(TemperatureRequirement, 29)
	if len(h.writer.records("environment")) != 5 {
		t.Fatal("threshold status change must bypass throttle")
	}
	for i := 0; i < 12; i++ {
		h.clock.advance(5 * time.Second)
		h.observe(TemperatureRequirement, 29.1+float64(i)/100)
	}
	if len(h.writer.records("alert")) != 1 {
		t.Fatal("a sustained warm reading is not a new alert every sample")
	}
	for _, r := range h.writer.records("environment") {
		if r.RecordID != "current" {
			t.Fatalf("snapshot created historical row %q", r.RecordID)
		}
	}
}

func TestBadNullNonNumericOldAndFutureDataAreNeverComfortable(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		raw    string
		state  string
	}{
		{name: "bad", change: func(m map[string]any) { m["quality"] = "bad" }, state: "unknown"},
		{name: "unavailable", change: func(m map[string]any) { m["quality"] = "unavailable" }, state: "unknown"},
		{name: "uncertain", change: func(m map[string]any) { m["quality"] = "uncertain" }, state: "unknown"},
		{name: "unknown quality", change: func(m map[string]any) { m["quality"] = "perfect" }, state: "unknown"},
		{name: "null", change: func(m map[string]any) { m["value"] = nil }, state: "unknown"},
		{name: "missing", change: func(m map[string]any) { delete(m, "value") }, state: "unknown"},
		{name: "string NaN", change: func(m map[string]any) { m["value"] = "NaN" }, state: "unknown"},
		{name: "boolean", change: func(m map[string]any) { m["value"] = true }, state: "unknown"},
		{name: "array", change: func(m map[string]any) { m["value"] = []int{24} }, state: "unknown"},
		{name: "quality null", change: func(m map[string]any) { m["quality"] = nil }, state: "unknown"},
		{name: "negative sequence", change: func(m map[string]any) { m["sequence"] = -1 }, state: "unknown"},
		{name: "timestamp", change: func(m map[string]any) { m["observed_at"] = "not-a-date" }, state: "unknown"},
		{name: "future", change: func(m map[string]any) { m["observed_at"] = "2027-01-01T00:00:00Z" }, state: "unknown"},
		{name: "number overflow", raw: `{"capability":"cloudpath.dev/capability/temperature@1","property":"value","value":1e999}`, state: "unknown"},
		{name: "bare NaN", raw: `{"capability":"cloudpath.dev/capability/temperature@1","property":"value","value":NaN}`, state: "unknown"},
		{name: "root null", raw: "null", state: "unknown"},
		{name: "duplicate value", raw: `{"capability":"cloudpath.dev/capability/temperature@1","property":"value","value":20,"value":40}`, state: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.good()
			h.clock.advance(time.Second)
			payload := h.observation(TemperatureRequirement, 24)
			if tc.change != nil {
				tc.change(payload)
			}
			raw := tc.raw
			if raw == "" {
				raw = jsonText(payload)
			}
			h.raw(TemperatureRequirement, raw)
			r := h.snapshot()
			assertStatus(t, r, tc.state)
			if r.Temperature.Usable {
				t.Fatalf("bad observation usable: %+v", r.Temperature)
			}
		})
	}
	for _, field := range []string{"observed_at", "received_at"} {
		t.Run("stale "+field, func(t *testing.T) {
			h := newHarness(t)
			h.observe(IlluminanceRequirement, 60)
			data := h.observation(TemperatureRequirement, 24)
			data[field] = h.clock.now().Add(-121 * time.Second).Format(time.RFC3339Nano)
			h.raw(TemperatureRequirement, jsonText(data))
			assertStatus(t, h.snapshot(), "stale")
			if h.snapshot().Temperature.Usable {
				t.Fatal("old measurement laundered by a recent receipt")
			}
		})
	}
}

func TestWrongBindingCapabilityPropertyAndOutOfOrderAreIsolated(t *testing.T) {
	h := newHarness(t)
	h.good()
	h.clock.advance(5 * time.Second)
	before := len(h.writer.all())
	goodPayload := map[string]any{"capability": TemperatureCapability, "property": "value", "value": 50, "quality": "good", "observed_at": h.clock.now().Format(time.RFC3339Nano), "sequence": int64(2)}
	for _, mutate := range []func(*application.CapabilityEvent, map[string]any){
		func(e *application.CapabilityEvent, _ map[string]any) { e.EntityID = "another-entity" },
		func(e *application.CapabilityEvent, _ map[string]any) { e.RequirementID = "another-role" },
		func(e *application.CapabilityEvent, _ map[string]any) { e.EventType = PropertyObservedEvent + "/extra" },
		func(_ *application.CapabilityEvent, m map[string]any) { m["capability"] = IlluminanceCapability },
		func(_ *application.CapabilityEvent, m map[string]any) { m["property"] = "raw" },
		func(_ *application.CapabilityEvent, m map[string]any) { m["entity_id"] = "another-entity" },
		func(_ *application.CapabilityEvent, m map[string]any) { m["sequence"] = 1 },
		func(_ *application.CapabilityEvent, m map[string]any) {
			m["observed_at"] = h.clock.now().Add(-10 * time.Second).Format(time.RFC3339Nano)
		},
	} {
		copyPayload := map[string]any{}
		for k, v := range goodPayload {
			copyPayload[k] = v
		}
		event := &application.CapabilityEvent{RequirementID: TemperatureRequirement, EntityID: "sensor-temperature", EventType: PropertyObservedEvent}
		mutate(event, copyPayload)
		event.PayloadJSON = jsonText(copyPayload)
		h.send(event)
		assertValue(t, h.snapshot().Temperature, 24)
	}
	if len(h.writer.all()) != before {
		t.Fatal("ignored observations produced effects")
	}
	// Wrong observations did not poison either data or sequence watermarks.
	h.observe(TemperatureRequirement, 26)
	assertValue(t, h.snapshot().Temperature, 26)
	r := h.snapshot()
	assertStatus(t, r, "within_thresholds")
	if err := h.sendError(&application.ApplicationEvent{PluginInstanceID: h.id, Sequence: 1, Union: &application.CapabilityEvent{RequirementID: TemperatureRequirement, EntityID: "sensor-temperature", EventType: PropertyObservedEvent, PayloadJSON: jsonText(goodPayload)}}); err != nil {
		t.Fatal(err)
	}
	assertValue(t, h.snapshot().Temperature, 26)
}

func TestMissingTimestampsAndRelativeUnitsAreHonest(t *testing.T) {
	h := newHarness(t)
	h.raw(TemperatureRequirement, `{"capability":"cloudpath.dev/capability/temperature@1","property":"value","value":24}`)
	h.raw(IlluminanceRequirement, `{"capability":"cloudpath.dev/capability/illuminance@1","property":"value","value":73}`)
	r := h.snapshot()
	assertStatus(t, r, "within_thresholds")
	if r.ObservedAt != nil || r.Temperature.ObservedAt != nil || r.Illuminance.ObservedAt != nil {
		t.Fatal("invented measurement timestamp")
	}
	if r.Temperature.ReceivedAt != nil || r.Temperature.ApplicationReceivedAt == nil {
		t.Fatal("application arrival was misrepresented as a Core timestamp")
	}
	if r.Quality != "unspecified" || r.Illuminance.Unit != "" || r.Illuminance.UnitLabel != "原始/相对读数" {
		t.Fatalf("invented calibration/quality: %+v", r)
	}
	h.clock.advance(time.Minute)
	h.job(jobRefresh, "read-only")
	after := h.snapshot()
	if after.ObservedAt != nil || !after.Illuminance.ApplicationReceivedAt.Equal(*r.Illuminance.ApplicationReceivedAt) {
		t.Fatal("refresh moved a sampling timestamp")
	}
	h.clock.advance(time.Minute)
	h.job(jobFreshness, "expired")
	assertStatus(t, h.snapshot(), "stale")
}

func TestDurableHeartbeatOnlyAndManualRefreshNeverSamples(t *testing.T) {
	h := newHarness(t)
	tasks := h.writer.schedules()
	if len(tasks) != 1 || tasks[0].ScheduleID != jobFreshness || tasks[0].Cron != "* * * * *" || tasks[0].PayloadJSON != "{}" {
		t.Fatalf("schedule=%+v", tasks)
	}
	unknown := h.job(jobRefresh, "before-observation")
	var initial struct {
		Environment EnvironmentRecord `json:"environment"`
		Sampled     bool              `json:"sampled"`
	}
	if err := json.Unmarshal([]byte(unknown.ResultJSON), &initial); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, initial.Environment, "unknown")
	if initial.Sampled || initial.Environment.Temperature.Value != nil {
		t.Fatal("manual job invented a sample")
	}
	h.good()
	original := h.snapshot()
	written := len(h.writer.records("environment"))
	h.clock.advance(120 * time.Second)
	h.job(jobBootstrap, "declaration-only")
	if len(h.writer.schedules()) != 1 || len(h.writer.records("environment")) != written {
		t.Fatal("bootstrap performed duplicate freshness work")
	}
	// Read-only status calculates staleness without writing anything.
	assertStatus(t, h.snapshot(), "stale")
	h.job(jobFreshness, "minute-2")
	if len(h.writer.records("environment")) != written+1 {
		t.Fatal("durable tick failed to publish stale status")
	}
	if h.snapshot().ObservedAt == nil || !h.snapshot().ObservedAt.Equal(*original.ObservedAt) {
		t.Fatal("freshness tick fabricated observation time")
	}
	count := len(h.writer.all())
	first := h.job(jobRefresh, "same-key")
	h.clock.advance(time.Minute)
	second := h.job(jobRefresh, "same-key")
	if first.ResultJSON != second.ResultJSON || len(h.writer.all()) != count {
		t.Fatal("job replay emitted effects or recomputed its saved result")
	}
	h.job(jobFreshness, "minute-3")
	if len(h.writer.records("alert")) != 0 || len(h.writer.records("environment")) != written+1 {
		t.Fatal("silence must not create a history row every minute")
	}
	h.good()
	assertStatus(t, h.snapshot(), "within_thresholds")
}

func TestBadAndStaleNeverRecoverAnActiveThreshold(t *testing.T) {
	h := newHarness(t)
	h.observe(TemperatureRequirement, 30)
	h.observe(IlluminanceRequirement, 60)
	h.clock.advance(time.Second)
	bad := h.observation(TemperatureRequirement, 24)
	bad["quality"] = "bad"
	h.raw(TemperatureRequirement, jsonText(bad))
	h.clock.advance(120 * time.Second)
	h.job(jobFreshness, "stale")
	if len(h.writer.records("alert")) != 1 {
		t.Fatal("invalid/stale data falsely recovered a threshold")
	}
	h.observe(TemperatureRequirement, 27.5)
	h.observe(IlluminanceRequirement, 60)
	if h.snapshot().Temperature.Status != "warm" || len(h.writer.records("alert")) != 1 {
		t.Fatal("invalid period cleared hysteresis latch")
	}
	h.observe(TemperatureRequirement, 27)
	if len(h.writer.records("alert")) != 2 {
		t.Fatal("fresh recovery missing")
	}
}

func TestUnitsMustNotBeInventedConvertedOrSilentlyChanged(t *testing.T) {
	h := newHarness(t)
	configure(t, h.svc, h.id, `{"temperature_unit":"degC","light_unit":"relative","light_threshold":300}`, 2)
	h.observe(TemperatureRequirement, 24)
	h.observe(IlluminanceRequirement, 500)
	r := h.snapshot()
	assertStatus(t, r, "within_thresholds")
	if r.Illuminance.Unit != "relative" || r.Temperature.Unit != "degC" {
		t.Fatal("configured fallback units missing")
	}
	// No global 0..100 assumption: a generic relative source can use any scale.
	assertValue(t, r.Illuminance, 500)
	data := h.observation(TemperatureRequirement, 75)
	data["unit"] = "degF"
	h.raw(TemperatureRequirement, jsonText(data))
	r = h.snapshot()
	assertStatus(t, r, "unknown")
	if r.Temperature.Reason != "unit_mismatch" {
		t.Fatalf("unit mismatch=%+v", r.Temperature)
	}
	configure(t, h.svc, h.id, `{"temperature_unit":"degF","temperature_min":64,"temperature_max":82}`, 3)
	if h.snapshot().Temperature.Value != nil {
		t.Fatal("unit config relabelled an old sample")
	}
	h.raw(TemperatureRequirement, jsonText(h.observation(TemperatureRequirement, 75)))
	h.observe(IlluminanceRequirement, 60)
	assertStatus(t, h.snapshot(), "within_thresholds")
	h2 := newHarness(t)
	a := h2.observation(TemperatureRequirement, 24)
	a["unit"] = "degC"
	h2.raw(TemperatureRequirement, jsonText(a))
	h2.observe(IlluminanceRequirement, 500)
	a = h2.observation(TemperatureRequirement, 24)
	a["unit"] = "degF"
	h2.raw(TemperatureRequirement, jsonText(a))
	if h2.snapshot().Temperature.Reason != "unit_changed" {
		t.Fatal("unconfigured metadata change was silently accepted")
	}
}

func TestTwoInstancesRouteEffectsAndJobsIndependently(t *testing.T) {
	svc, clock := newTestService(t)
	for _, id := range []string{"desk-a", "desk-b"} {
		configure(t, svc, id, "{}", 1)
		bind(t, svc, id)
	}
	configure(t, svc, "desk-a", `{"light_threshold":30}`, 2)
	a, b := openHarness(t, svc, clock, "desk-a"), openHarness(t, svc, clock, "desk-b")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.observe(TemperatureRequirement, 30); a.observe(IlluminanceRequirement, 20) }()
	go func() { defer wg.Done(); b.good() }()
	wg.Wait()
	assertStatus(t, a.snapshot(), "attention")
	assertStatus(t, b.snapshot(), "within_thresholds")
	before := len(b.writer.all())
	clock.advance(120 * time.Second)
	a.job(jobFreshness, "same-job-key")
	if len(b.writer.all()) != before {
		t.Fatal("instance A job wrote into instance B stream")
	}
	b.job(jobFreshness, "same-job-key")
	for _, h := range []*harness{a, b} {
		for _, e := range h.writer.all() {
			if e.PluginInstanceID != h.id {
				t.Fatalf("cross-instance effect %q -> %q", e.PluginInstanceID, h.id)
			}
		}
	}
	if len(a.writer.records("alert")) != 2 || len(b.writer.records("alert")) != 0 {
		t.Fatal("threshold latches crossed instances")
	}
}

func TestOldStreamCleanupCannotRemoveReplacementRoute(t *testing.T) {
	old := newHarness(t)
	old.good()
	replacement := openHarness(t, old.svc, old.clock, old.id)
	old.stop()
	before := len(old.writer.all())
	old.clock.advance(120 * time.Second)
	replacement.job(jobFreshness, "new-stream")
	if len(old.writer.all()) != before || len(replacement.writer.records("environment")) != 1 {
		t.Fatal("replacement writer was lost during old stream cleanup")
	}
	if len(replacement.writer.schedules()) != 1 {
		t.Fatal("new stream did not re-declare the stable schedule id")
	}
}

func TestJobAndBindingValidationAndBoundedIdempotency(t *testing.T) {
	h := newHarness(t)
	h.good()
	for _, args := range []string{"null", "[]", `{"value":20}`, `{"x":1,"x":2}`, "{} {}"} {
		if _, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: h.id, JobID: jobRefresh, ArgsJSON: args}); err == nil {
			t.Fatalf("accepted args %q", args)
		}
	}
	for _, req := range []*application.RunJobRequest{nil, {PluginInstanceID: h.id, JobID: "sample-now"}, {PluginInstanceID: "", JobID: jobRefresh}, {PluginInstanceID: "missing", JobID: jobRefresh}, {PluginInstanceID: h.id, JobID: jobRefresh, Deadline: "broken"}, {PluginInstanceID: h.id, JobID: jobRefresh, Deadline: h.clock.now().Format(time.RFC3339)}} {
		if _, err := h.svc.RunJob(context.Background(), req); err == nil {
			t.Fatalf("accepted bad job %+v", req)
		}
	}
	for i := 0; i < maxJobResults+40; i++ {
		h.job(jobRefresh, fmt.Sprint(i))
	}
	st, _ := h.svc.lookup(h.id, false)
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.jobResults) != maxJobResults || len(st.jobOrder) != maxJobResults {
		t.Fatal("job idempotency cache is unbounded")
	}
}

func TestRebindingClearsOldReadingsAndDoesNotAcceptInvalidProposal(t *testing.T) {
	h := newHarness(t)
	h.good()
	old := []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "sensor-temperature"}, {RequirementID: IlluminanceRequirement, EntityID: "sensor-illuminance"}}
	for _, bindings := range [][]application.Binding{nil, old[:1], append(append([]application.Binding{}, old...), old[0]), {{RequirementID: "output", EntityID: "e"}}, {{RequirementID: TemperatureRequirement, EntityID: " "}, old[1]}} {
		resp, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: h.id, Bindings: bindings})
		if err != nil || resp.Valid || len(resp.Issues) == 0 {
			t.Fatalf("invalid proposal = %+v, %v", resp, err)
		}
		assertStatus(t, h.snapshot(), "within_thresholds")
	}
	updated := []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "replacement"}, old[1]}
	if resp, err := h.svc.ValidateBinding(context.Background(), &application.ValidateBindingRequest{PluginInstanceID: h.id, Bindings: updated}); err != nil || !resp.Valid {
		t.Fatalf("rebind=%+v %v", resp, err)
	}
	h.observe(TemperatureRequirement, 25)
	r := h.snapshot()
	assertStatus(t, r, "unknown")
	if r.Temperature.Value != nil {
		t.Fatal("old entity observation crossed binding change")
	}
	h.send(&application.CapabilityEvent{RequirementID: TemperatureRequirement, EntityID: "replacement", EventType: PropertyObservedEvent, PayloadJSON: jsonText(h.observation(TemperatureRequirement, 24))})
	assertStatus(t, h.snapshot(), "within_thresholds")
	same := []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "compound-sensor"}, {RequirementID: IlluminanceRequirement, EntityID: "compound-sensor"}}
	if _, issues := validateBindings(same); len(issues) != 0 {
		t.Fatal("compound entity supporting both capabilities must be allowed")
	}
}

func TestConfigDefaultsAndStrictValidation(t *testing.T) {
	for _, raw := range []string{"", "{}"} {
		cfg, err := UnmarshalConfig([]byte(raw))
		if err != nil || cfg != DefaultConfig() {
			t.Fatalf("defaults: %+v %v", cfg, err)
		}
	}
	cfg, err := UnmarshalConfig([]byte(`{"timezone":"Asia/Shanghai","hysteresis":{"temperature":0.5}}`))
	if err != nil || cfg.Hysteresis.Light != 5 || cfg.Timezone != "Asia/Shanghai" {
		t.Fatalf("partial config=%+v %v", cfg, err)
	}
	cases := []string{"null", "[]", "{} {}", `{"timezone":"Local"}`, `{"timezone":"No/Such_Zone"}`, `{"timezone":null}`, `{"temperture_min":10}`, `{"temperature_min":28}`, `{"temperature_max":"28"}`, `{"temperature_max":null}`, `{"light_threshold":"low"}`, `{"stale_after_s":59}`, `{"stale_after_s":86401}`, `{"stale_after_s":120.5}`, `{"hysteresis":null}`, `{"hysteresis":{"temperature":5}}`, `{"hysteresis":{"light":-1}}`, `{"hysteresis":{"temperature":null}}`, `{"hysteresis":{"typo":1}}`, `{"timezone":"UTC","timezone":"UTC"}`, `{"hysteresis":{"temperature":1,"temperature":2}}`, `{"temperature_unit":" degC"}`, `{"light_unit":"x\ny"}`}
	for _, raw := range cases {
		if _, err := UnmarshalConfig([]byte(raw)); err == nil {
			t.Errorf("accepted invalid config %s", raw)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := DefaultConfig()
		c.TemperatureMax = value
		if c.Validate() == nil {
			t.Errorf("non-finite config accepted: %v", value)
		}
	}
}

func TestConfigRevisionTimezoneAndStatusFacts(t *testing.T) {
	h := newHarness(t)
	h.good()
	configure(t, h.svc, h.id, `{"timezone":"Asia/Shanghai"}`, 2)
	record := h.snapshot()
	if record.Timezone != "Asia/Shanghai" || record.EvaluatedAt.Hour() != 17 || record.ConfigRevision != 2 || record.Title == "" || record.Summary == "" || record.Quality != "good" {
		t.Fatalf("status facts=%+v", record)
	}
	resp, err := h.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: h.id, Config: []byte("{}"), ConfigRevision: 1})
	if err != nil || resp.Status.IsOK() || h.snapshot().ConfigRevision != 2 {
		t.Fatal("stale config revision applied")
	}
	resp, err = h.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: h.id, Config: []byte("{}"), ConfigRevision: 2})
	if err != nil || resp.Status.IsOK() {
		t.Fatal("different config reused a revision")
	}
	for _, req := range []*application.PluginHTTPRequest{nil, {Method: "GET", Context: application.RequestContext{}}, {Method: "GET", PluginInstanceID: "other", Context: application.RequestContext{InstanceID: h.id}}} {
		if _, err := h.svc.HandleRequest(context.Background(), req); err == nil {
			t.Fatalf("accepted HTTP context %+v", req)
		}
	}
	for _, test := range []struct {
		method, path string
		code         uint32
	}{{"POST", "/status", 405}, {"GET", "/absent", 404}} {
		resp, err := h.svc.HandleRequest(context.Background(), &application.PluginHTTPRequest{Method: test.method, Path: test.path, Context: application.RequestContext{InstanceID: h.id}})
		if err != nil || resp.StatusCode != test.code {
			t.Fatalf("HTTP=%+v %v", resp, err)
		}
	}
}

type sliceReader struct {
	events []*application.ApplicationEvent
	err    error
}

func (r *sliceReader) Recv(context.Context) (*application.ApplicationEvent, error) {
	if len(r.events) == 0 {
		if r.err != nil {
			return nil, r.err
		}
		return nil, io.EOF
	}
	ev := r.events[0]
	r.events = r.events[1:]
	return ev, nil
}

type nonComparableWriter []int

func (nonComparableWriter) Send(context.Context, *application.ApplicationEffect) error { return nil }

func assertCode(t *testing.T, err error, code status.Code) {
	t.Helper()
	var rpcStatus *status.Status
	if !errors.As(err, &rpcStatus) || rpcStatus.Code != code {
		t.Fatalf("status error = %v, want %v", err, code)
	}
}

func TestNilStreamsLifecycleAndReadFailures(t *testing.T) {
	svc := New()
	ctx := context.Background()
	if health, _ := svc.Health(ctx); health.State != application.HealthStateNotServing {
		t.Fatal("uninitialized service claimed serving")
	}
	_, initErr := svc.Initialize(ctx, nil)
	assertCode(t, initErr, status.CodeInvalidArgument)
	if _, err := svc.Initialize(ctx, &application.InitializeRequest{ProtocolVersion: 99}); err == nil {
		t.Fatal("unsupported protocol")
	}
	if _, err := svc.Initialize(ctx, &application.InitializeRequest{ProtocolVersion: 99, SupportedProtocolVersions: []uint32{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfigureInstance(ctx, nil); err == nil {
		t.Fatal("nil configure")
	}
	if _, err := svc.ValidateBinding(ctx, nil); err == nil {
		t.Fatal("nil binding")
	}
	var reader *sliceReader
	var writer *captureWriter
	for _, pair := range []struct {
		r application.ApplicationEventReader
		w application.ApplicationEffectWriter
	}{{nil, &captureWriter{}}, {&sliceReader{}, nil}, {reader, &captureWriter{}}, {&sliceReader{}, writer}} {
		if svc.HandleEvents(ctx, pair.r, pair.w) == nil {
			t.Fatal("nil stream accepted")
		}
	}
	for _, err := range []error{io.EOF, context.Canceled, context.DeadlineExceeded} {
		if got := svc.HandleEvents(ctx, &sliceReader{err: fmt.Errorf("wrapped: %w", err)}, &captureWriter{}); got != nil {
			t.Fatal(got)
		}
	}
	sentinel := errors.New("reader failed")
	if got := svc.HandleEvents(ctx, &sliceReader{err: sentinel}, &captureWriter{}); !errors.Is(got, sentinel) {
		t.Fatalf("read error=%v", got)
	}
	configure(t, svc, "test", "{}", 1)
	bind(t, svc, "test")
	if err := svc.HandleEvents(ctx, &sliceReader{events: []*application.ApplicationEvent{nil, {PluginInstanceID: "test"}, {PluginInstanceID: "test", Union: (*application.CapabilityEvent)(nil)}}}, nonComparableWriter{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Shutdown(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if health, _ := svc.Health(ctx); health.State != application.HealthStateNotServing {
		t.Fatal("closed service claimed serving")
	}
	if _, err := svc.RunJob(ctx, &application.RunJobRequest{PluginInstanceID: "test", JobID: jobRefresh}); err == nil {
		t.Fatal("job accepted after shutdown")
	}
	if _, err := svc.Initialize(ctx, &application.InitializeRequest{ProtocolVersion: 1}); err == nil {
		t.Fatal("reinitialized closed service")
	}
}

func TestSendFailuresKeepPendingWorkAndDoNotCacheFalseSuccess(t *testing.T) {
	h := newHarness(t)
	h.good()
	sentinel := errors.New("effect write failed")
	h.writer.mu.Lock()
	h.writer.fail = func(effect *application.ApplicationEffect) error {
		if r, ok := effect.Union.(*application.UpsertDomainRecord); ok && r.RecordType == "alert" {
			return sentinel
		}
		return nil
	}
	h.writer.mu.Unlock()
	h.sequence++
	ev := &application.ApplicationEvent{PluginInstanceID: h.id, Sequence: h.sequence, Union: &application.CapabilityEvent{RequirementID: TemperatureRequirement, EntityID: "sensor-temperature", EventType: PropertyObservedEvent, PayloadJSON: jsonText(h.observation(TemperatureRequirement, 30))}}
	if err := h.sendError(ev); !errors.Is(err, sentinel) {
		t.Fatalf("send failure=%v", err)
	}
	st, _ := h.svc.lookup(h.id, false)
	st.mu.Lock()
	pending := len(st.pendingAlerts)
	declared := st.scheduleDeclared
	st.mu.Unlock()
	if pending != 1 || declared {
		t.Fatal("lost pending alert or advertised dead stream schedule")
	}
	if _, err := h.svc.RunJob(context.Background(), &application.RunJobRequest{PluginInstanceID: h.id, JobID: jobRefresh, IdempotencyKey: "retry"}); err == nil {
		t.Fatal("job succeeded without an effect writer")
	}
	replacement := openHarness(t, h.svc, h.clock, h.id)
	replacement.job(jobRefresh, "retry")
	if len(replacement.writer.records("alert")) != 1 {
		t.Fatal("pending threshold transition was lost or duplicated")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.pendingAlerts) != 0 || !st.scheduleDeclared {
		t.Fatal("pending work not reconciled on new stream")
	}
}
