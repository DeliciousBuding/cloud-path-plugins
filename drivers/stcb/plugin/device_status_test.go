// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
)

func TestSendSnapshotUsesFreshnessNotOpenPort(t *testing.T) {
	d := New()
	var statuses []driver.DeviceStatus
	send := func(_ string, union driver.DriverMessageUnion) error {
		if upsert, ok := union.(*driver.DeviceUpsert); ok {
			statuses = append(statuses, upsert.Device.Status)
		}
		return nil
	}
	dev := &device{cfg: deviceConfig{Name: "board"}, portName: "COM3"}

	if err := d.sendSnapshot(send, "board", dev); err != nil {
		t.Fatal(err)
	}
	dev.full = &FullState{}
	dev.lastFull = time.Now()
	if err := d.sendSnapshot(send, "board", dev); err != nil {
		t.Fatal(err)
	}
	dev.lastFull = time.Now().Add(-31 * time.Second)
	if err := d.sendSnapshot(send, "board", dev); err != nil {
		t.Fatal(err)
	}

	want := []driver.DeviceStatus{
		driver.DeviceStatusUnavailable,
		driver.DeviceStatusOnline,
		driver.DeviceStatusUnavailable,
	}
	if len(statuses) != len(want) {
		t.Fatalf("statuses=%v want=%v", statuses, want)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("statuses[%d]=%v want=%v (all=%v)", i, statuses[i], want[i], statuses)
		}
	}
}
