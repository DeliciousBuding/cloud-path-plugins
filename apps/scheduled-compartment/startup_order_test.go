// SPDX-License-Identifier: Apache-2.0

package scheduledcompartment

import (
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

// Core v0.2.15 validates the proposed bindings before ConfigureInstance.
// Exercise that actual RPC order, not just the configure-first unit harness.
func TestCoreBindingBeforeConfiguration(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(mustJSON(count), func(t *testing.T) {
			a := newTestApp(t, time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC))
			defer a.close()
			binding, err := a.cli.ValidateBinding(a.ctx, &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: practicalBindings(count)})
			if err != nil || !binding.Valid {
				t.Fatalf("pre-config binding: %+v, %v", binding, err)
			}
			configured := a.configure(mustJSON(practicalConfig(count)))
			if !configured.Status.IsOK() {
				t.Fatalf("configure rejected matching bindings: %+v", configured.Status)
			}
		})
	}
}

func TestPreconfiguredBindingsStillRequireExactCompartmentCount(t *testing.T) {
	a := newTestApp(t, time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC))
	defer a.close()
	binding, err := a.cli.ValidateBinding(a.ctx, &application.ValidateBindingRequest{PluginInstanceID: testInstance, Bindings: practicalBindings(1)})
	if err != nil || !binding.Valid {
		t.Fatalf("pre-config binding: %+v, %v", binding, err)
	}
	rejected := a.configure(mustJSON(practicalConfig(3)))
	if rejected.Status.IsOK() {
		t.Fatal("three compartments with one input must be rejected at configuration")
	}
	if a.svc.instances[testInstance].config != nil {
		t.Fatal("failed configuration must not activate partial settings")
	}
	accepted := a.configure(mustJSON(practicalConfig(1)))
	if !accepted.Status.IsOK() {
		t.Fatalf("matching recovery configuration rejected: %+v", accepted.Status)
	}
}
