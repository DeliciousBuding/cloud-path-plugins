package environmentguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

func TestScheduleSendFailureRetriesSameJobWithoutFalseSuccess(t *testing.T) {
	h := newHarness(t)
	sentinel := errors.New("schedule send failed")
	h.writer.mu.Lock()
	h.writer.fail = func(effect *application.ApplicationEffect) error {
		if _, ok := effect.Union.(*application.ScheduleTask); ok {
			return sentinel
		}
		return nil
	}
	h.writer.mu.Unlock()
	_, err := h.svc.ConfigureInstance(context.Background(), &application.ConfigureInstanceRequest{PluginInstanceID: h.id, Config: []byte(`{"timezone":"Asia/Shanghai"}`), ConfigRevision: 2})
	if !errors.Is(err, sentinel) {
		t.Fatalf("configure send failure=%v", err)
	}
	st, _ := h.svc.lookup(h.id, false)
	st.mu.Lock()
	declared := st.scheduleDeclared
	st.mu.Unlock()
	if declared {
		t.Fatal("failed schedule was marked declared")
	}
	request := &application.RunJobRequest{PluginInstanceID: h.id, JobID: jobBootstrap, ArgsJSON: "{}", IdempotencyKey: "bootstrap-retry"}
	if _, err := h.svc.RunJob(context.Background(), request); !errors.Is(err, sentinel) {
		t.Fatalf("bootstrap failure=%v", err)
	}
	h.writer.mu.Lock()
	h.writer.fail = nil
	h.writer.mu.Unlock()
	if result, err := h.svc.RunJob(context.Background(), request); err != nil || !result.Status.IsOK() {
		t.Fatalf("retry=%+v %v", result, err)
	}
	if len(h.writer.schedules()) != 2 {
		t.Fatal("schedule retry was skipped/cached or duplicated")
	}
	h.job(jobBootstrap, "bootstrap-retry")
	if len(h.writer.schedules()) != 2 {
		t.Fatal("duplicate bootstrap redeclared schedule")
	}
}

func TestSequenceCannotSilentlyDisappearAfterOrderedStream(t *testing.T) {
	h := newHarness(t)
	h.good()
	h.raw(TemperatureRequirement, `{"capability":"cloudpath.dev/capability/temperature@1","property":"value","value":24}`)
	r := h.snapshot()
	if r.Status != "unknown" || r.Temperature.Reason != "missing_sequence" {
		t.Fatalf("sequence downgrade=%+v", r)
	}
	h.observe(TemperatureRequirement, 24)
	assertStatus(t, h.snapshot(), "within_thresholds")
}

func TestOtherInstanceProgressesWhileOneWriterBlocks(t *testing.T) {
	svc, clock := newTestService(t)
	for _, id := range []string{"desk-a", "desk-b"} {
		configure(t, svc, id, "{}", 1)
		bind(t, svc, id)
	}
	a, b := openHarness(t, svc, clock, "desk-a"), openHarness(t, svc, clock, "desk-b")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	a.writer.mu.Lock()
	a.writer.fail = func(effect *application.ApplicationEffect) error {
		if _, ok := effect.Union.(*application.UpsertDomainRecord); ok {
			close(entered)
			<-release
		}
		return nil
	}
	a.writer.mu.Unlock()
	finished := make(chan error, 1)
	a.sequence++
	event := &application.ApplicationEvent{PluginInstanceID: a.id, Sequence: a.sequence, Union: &application.CapabilityEvent{RequirementID: TemperatureRequirement, EntityID: "sensor-temperature", EventType: PropertyObservedEvent, PayloadJSON: jsonText(a.observation(TemperatureRequirement, 24))}}
	go func() { finished <- a.sendError(event) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not block")
	}
	b.good()
	assertStatus(t, b.snapshot(), "within_thresholds")
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("instance A did not resume")
	}
}
