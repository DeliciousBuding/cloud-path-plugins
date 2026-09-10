// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
)

func TestDescribeActionPresentationSurvivesJSON(t *testing.T) {
	described, err := New().Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(described)
	if err != nil {
		t.Fatal(err)
	}
	var restored driver.DriverDescriptor
	if err := json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{actionBuzzer: true, actionTone: true, actionToneSequence: true, actionLED: true, actionDisplay: true, actionMotor: true, actionDiag: true}
	for _, capability := range restored.Capabilities {
		if strings.TrimSpace(capability.Title) == "" {
			t.Fatalf("capability %s lost its display title", capability.ID)
		}
		for _, action := range capability.Actions {
			if !want[action.Name] {
				t.Fatalf("unexpected or duplicate action exposed: %s", action.Name)
			}
			delete(want, action.Name)
			if strings.TrimSpace(action.Title) == "" || strings.TrimSpace(action.Description) == "" {
				t.Fatalf("action %s lost presentation metadata through Describe JSON", action.Name)
			}
			if !json.Valid([]byte(action.InputSchemaJSON)) {
				t.Fatalf("action %s lost its schema", action.Name)
			}
			if action.Destructive || action.Confirmation != "" {
				t.Fatalf("ordinary action %s gained undeclared danger semantics", action.Name)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing declared actions: %v", want)
	}
}
