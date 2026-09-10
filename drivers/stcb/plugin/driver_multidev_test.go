package plugin

import (
	"context"
	"testing"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
	"go.bug.st/serial"
)

func TestOpenConfigForPrefersConnectionHints(t *testing.T) {
	d := New()
	d.cfgs["inst"] = instanceConfig{DeviceID: "def", Name: "default", Port: "COM1", Baud: 9600}
	id, cfg, err := d.openConfigFor(&driver.OpenDeviceRequest{
		PluginInstanceID: "inst",
		DeviceID:         "board-2",
		ConnectionHints:  map[string]string{"port": "COM4", "baud": "115200", "name": "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "board-2" || cfg.Port != "COM4" || cfg.Baud != 115200 || cfg.Name != "second" {
		t.Fatalf("id=%q cfg=%+v", id, cfg)
	}
}

func TestOpenConfigForFallsBackToInstanceConfig(t *testing.T) {
	d := New()
	d.cfgs["inst"] = instanceConfig{DeviceID: "def", Name: "default", Port: "COM1", Baud: 9600}
	id, cfg, err := d.openConfigFor(&driver.OpenDeviceRequest{PluginInstanceID: "inst", DeviceID: "def"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "def" || cfg.Port != "COM1" || cfg.Baud != 9600 {
		t.Fatalf("id=%q cfg=%+v", id, cfg)
	}
}

func TestDeviceIDsForListsOpenDevices(t *testing.T) {
	d := New()
	d.devices[instanceDeviceKey("inst", "b")] = &device{instanceID: "inst", cfg: deviceConfig{ID: "b"}}
	d.devices[instanceDeviceKey("inst", "a")] = &device{instanceID: "inst", cfg: deviceConfig{ID: "a"}}
	d.devices[instanceDeviceKey("other", "z")] = &device{instanceID: "other", cfg: deviceConfig{ID: "z"}}
	ids := d.deviceIDsFor("inst", nil)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestDeviceIDsForPrefersRequest(t *testing.T) {
	d := New()
	d.cfgs["inst"] = instanceConfig{DeviceID: "def"}
	ids := d.deviceIDsFor("inst", []string{"x", "y"})
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "y" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestInstancesWithSameDeviceIDAreIsolated(t *testing.T) {
	oldOpen := serialOpen
	defer func() { serialOpen = oldOpen }()
	ports := map[string]*fakePort{"COM3": {}, "COM4": {}}
	serialOpen = func(name string, _ *serial.Mode) (serial.Port, error) { return ports[name], nil }

	d := New()
	configure(t, d, "instance-a", `{"device_id":"board-1","port":"COM3","baud":115200}`, 1)
	configure(t, d, "instance-b", `{"device_id":"board-1","port":"COM4","baud":115200}`, 1)

	for _, instanceID := range []string{"instance-a", "instance-b"} {
		resp, err := d.OpenDevice(context.Background(), &driver.OpenDeviceRequest{PluginInstanceID: instanceID, DeviceID: "board-1"})
		if err != nil || !resp.Status.IsOK() {
			t.Fatalf("open %s: resp=%+v err=%v", instanceID, resp, err)
		}
	}
	if len(d.devices) != 2 {
		t.Fatalf("devices=%d, want 2 isolated entries", len(d.devices))
	}
	if d.device("instance-a", "board-1") == d.device("instance-b", "board-1") {
		t.Fatal("same device_id in different instances was shared")
	}

	resp, err := d.Execute(context.Background(), &driver.ExecuteRequest{PluginInstanceID: "instance-a", DeviceID: "board-1", IdempotencyKey: "a-led", Action: actionLED, ArgsJSON: `{"pattern":9}`})
	if err != nil || resp.State != driver.CommandStateSucceeded {
		t.Fatalf("execute A: resp=%+v err=%v", resp, err)
	}
	if got := ports["COM3"].joined(); got != "L90" {
		t.Fatalf("COM3 got %q", got)
	}
	if got := ports["COM4"].joined(); got != "" {
		t.Fatalf("instance B received A command: %q", got)
	}

	resp, err = d.Execute(context.Background(), &driver.ExecuteRequest{PluginInstanceID: "instance-b", DeviceID: "board-1", IdempotencyKey: "b-led", Action: actionLED, ArgsJSON: `{"pattern":9}`})
	if err != nil || resp.State != driver.CommandStateSucceeded {
		t.Fatalf("execute B: resp=%+v err=%v", resp, err)
	}
	if got := ports["COM4"].joined(); got != "L90" {
		t.Fatalf("COM4 got %q", got)
	}

	_, _ = d.CloseDevice(context.Background(), &driver.CloseDeviceRequest{PluginInstanceID: "instance-a", DeviceID: "board-1"})
	if !ports["COM3"].isClosed() {
		t.Fatal("instance A port was not closed")
	}
	if ports["COM4"].isClosed() {
		t.Fatal("instance B port was affected by instance A close")
	}
	if d.device("instance-b", "board-1") == nil {
		t.Fatal("instance B device disappeared")
	}
}
