// SPDX-License-Identifier: Apache-2.0

package environmentguard

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/transport"
)

// Exercise the public codec/RPC path without opening a network listener, files,
// hardware, or a Core application host. This proves protocol interoperability,
// not Core fan-in, database execution or a real device's calibration.
func TestPublicSDKRPCObservationRecordAndManualJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := transport.Pipe(32)
	defer clientTransport.Close()
	defer serverTransport.Close()
	svc, clock := newTestService(t)
	server := application.NewRPCServer(serverTransport, svc)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("RPC server did not stop")
		}
	}()
	client := application.NewClient(clientTransport)
	initialized, err := client.Initialize(ctx, &application.InitializeRequest{PluginID: ApplicationID(), PluginVersion: Version(), ProtocolVersion: application.ProtocolVersion})
	if err != nil || !initialized.Status.IsOK() {
		t.Fatalf("RPC initialize=%+v %v", initialized, err)
	}
	descriptor, err := client.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !descriptor.Jobs[1].ManualOnly {
		t.Fatal("manual-only flag did not survive public RPC")
	}
	configured, err := client.ConfigureInstance(ctx, &application.ConfigureInstanceRequest{PluginInstanceID: "rpc-desk", Config: []byte("{}"), ConfigRevision: 1})
	if err != nil || !configured.Status.IsOK() {
		t.Fatalf("RPC configure=%+v %v", configured, err)
	}
	bound, err := client.ValidateBinding(ctx, &application.ValidateBindingRequest{PluginInstanceID: "rpc-desk", Bindings: []application.Binding{{RequirementID: TemperatureRequirement, EntityID: "env-temperature"}, {RequirementID: IlluminanceRequirement, EntityID: "env-illuminance"}}})
	if err != nil || !bound.Valid {
		t.Fatalf("RPC binding=%+v %v", bound, err)
	}
	stream, err := client.HandleEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel(context.Background())
	if err := stream.Send(ctx, &application.ApplicationEvent{PluginInstanceID: "rpc-desk", Sequence: 1, SchemaVersion: application.SchemaVersion, Union: &application.InstanceLifecycle{State: "running"}}); err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schedule, ok := first.Union.(*application.ScheduleTask)
	if !ok || schedule.ScheduleID != jobFreshness || schedule.Cron != "* * * * *" {
		t.Fatalf("RPC schedule=%+v", first)
	}
	second, err := stream.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if record, ok := second.Union.(*application.UpsertDomainRecord); !ok || record.RecordType != "environment" {
		t.Fatalf("RPC first snapshot=%+v", second)
	}
	for i, role := range roles {
		value := 24.0
		if role == IlluminanceRequirement {
			value = 512
		}
		payload := jsonText(map[string]any{"capability": capability(role), "property": "value", "value": value, "sequence": 1, "quality": "good", "observed_at": clock.now().Format(time.RFC3339Nano)})
		if err := stream.Send(ctx, &application.ApplicationEvent{PluginInstanceID: "rpc-desk", Sequence: uint64(i + 2), SchemaVersion: application.SchemaVersion, Union: &application.CapabilityEvent{RequirementID: role, EntityID: "env-" + role, EventType: PropertyObservedEvent, PayloadJSON: payload}}); err != nil {
			t.Fatal(err)
		}
		effect, err := stream.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		record, ok := effect.Union.(*application.UpsertDomainRecord)
		if !ok || record.RecordID != "current" || effect.PluginInstanceID != "rpc-desk" {
			t.Fatalf("RPC record=%+v", effect)
		}
	}
	result, err := client.RunJob(ctx, &application.RunJobRequest{PluginInstanceID: "rpc-desk", JobID: jobRefresh, ArgsJSON: "{}", IdempotencyKey: "operator-1"})
	if err != nil || !result.Status.IsOK() {
		t.Fatalf("RPC manual job=%+v %v", result, err)
	}
	var body struct {
		Environment EnvironmentRecord `json:"environment"`
		Sampled     bool              `json:"sampled"`
	}
	if err := json.Unmarshal([]byte(result.ResultJSON), &body); err != nil {
		t.Fatal(err)
	}
	if body.Sampled || body.Environment.Status != "within_thresholds" || body.Environment.Illuminance.ThresholdEnabled {
		t.Fatalf("RPC result=%+v", body)
	}
	assertValue(t, body.Environment.Illuminance, 512)
	clock.advance(120 * time.Second)
	if _, err := client.RunJob(ctx, &application.RunJobRequest{PluginInstanceID: "rpc-desk", JobID: jobFreshness, ArgsJSON: "{}", IdempotencyKey: "minute-2"}); err != nil {
		t.Fatal(err)
	}
	stale, err := stream.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	update, ok := stale.Union.(*application.UpsertDomainRecord)
	if !ok {
		t.Fatalf("RPC stale=%+v", stale)
	}
	var state EnvironmentRecord
	if err := json.Unmarshal([]byte(update.DataJSON), &state); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, state, "stale")
	if shutdown, err := client.Shutdown(ctx, &application.ShutdownRequest{Reason: "test"}); err != nil || !shutdown.Status.IsOK() {
		t.Fatalf("RPC shutdown=%+v %v", shutdown, err)
	}
}
