package plugin

import (
	"testing"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
)

func TestOpenConfigForPrefersConnectionHints(t *testing.T) {
	d := New()
	d.cfg = instanceConfig{DeviceID: "def", Name: "default", Port: "COM1", Baud: 9600}
	id, cfg, err := d.openConfigFor(&driver.OpenDeviceRequest{
		DeviceID:        "board-2",
		ConnectionHints: map[string]string{"port": "COM4", "baud": "115200", "name": "second"},
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
	d.cfg = instanceConfig{DeviceID: "def", Name: "default", Port: "COM1", Baud: 9600}
	id, cfg, err := d.openConfigFor(&driver.OpenDeviceRequest{DeviceID: "def"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "def" || cfg.Port != "COM1" || cfg.Baud != 9600 {
		t.Fatalf("id=%q cfg=%+v", id, cfg)
	}
}

func TestDeviceIDsForListsOpenDevices(t *testing.T) {
	d := New()
	d.devices["b"] = &device{cfg: deviceConfig{ID: "b"}}
	d.devices["a"] = &device{cfg: deviceConfig{ID: "a"}}
	ids := d.deviceIDsFor(nil)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestDeviceIDsForPrefersRequest(t *testing.T) {
	d := New()
	d.cfg = instanceConfig{DeviceID: "def"}
	ids := d.deviceIDsFor([]string{"x", "y"})
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "y" {
		t.Fatalf("ids=%v", ids)
	}
}
