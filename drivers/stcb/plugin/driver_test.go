package plugin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
)

func TestEncodeCommand(t *testing.T) {
	cases := []struct {
		action string
		args   string
		want   string
	}{
		{"dump", "", "S"},
		{"trigger", "", "R"},
		{"open", "", "O"},
		{"isp", "", "D"},
		{"sensor", "", "V"},
		{"buzzer", `{"freq":4,"duration":3}`, "B43"},
		{"led", `{"pattern":9}`, "L90"},
		{"display", `{"digits":[1,2,3,4,5,6,7,8]}`, "N12345678"},
		{"motor", `{"steps":2}`, "M2"},
		{"raw", `{"args":"XYZ"}`, "XYZ"},
	}
	for _, c := range cases {
		got, err := encodeCommand(c.action, c.args)
		if err != nil {
			t.Fatalf("encodeCommand(%s) err=%v", c.action, err)
		}
		if string(got) != c.want {
			t.Fatalf("encodeCommand(%s)=%q want %q", c.action, got, c.want)
		}
	}
}

func TestEncodeCommandInvalid(t *testing.T) {
	if _, err := encodeCommand("buzzer", `{"freq":10,"duration":0}`); err == nil {
		t.Fatal("expected reject freq 10")
	}
	if _, err := encodeCommand("display", `{"digits":[1,2]}`); err == nil {
		t.Fatal("expected reject short digits")
	}
	if _, err := encodeCommand("nosuch", ""); err == nil {
		t.Fatal("expected reject unknown action")
	}
	if _, err := encodeCommand("sync", "bad"); err == nil {
		t.Fatal("expected reject malformed sync args")
	}
}

func TestSyncEncodesCurrentTime(t *testing.T) {
	b, err := encodeCommand("sync", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 5 || b[0] != 'T' || !validHHMM(string(b[1:])) {
		t.Fatalf("sync frame = %q", b)
	}
}

func TestDescribeStable(t *testing.T) {
	d := New()
	desc, err := d.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.DriverID != "stcb" {
		t.Fatalf("DriverID = %q", desc.DriverID)
	}
	if desc.Version != "0.1.0" {
		t.Fatalf("Version = %q", desc.Version)
	}
	if len(desc.Capabilities) != 12 {
		t.Fatalf("capabilities = %d, want 12", len(desc.Capabilities))
	}
}

func TestInitializeNegotiatesV1(t *testing.T) {
	d := New()
	resp, err := d.Initialize(context.Background(), &driver.InitializeRequest{
		ProtocolVersion: 1, SupportedProtocolVersions: []uint32{1}, LaunchID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.NegotiatedProtocolVersion != 1 || !resp.Status.IsOK() {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestExecuteRejectsUnknownAction(t *testing.T) {
	d := New()
	resp, err := d.Execute(context.Background(), &driver.ExecuteRequest{
		IdempotencyKey: "k1", Action: "nope",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != driver.CommandStateFailed {
		t.Fatalf("state = %v, want failed", resp.State)
	}
}

func TestExecuteRequiresOpenDevice(t *testing.T) {
	d := New()
	resp, err := d.Execute(context.Background(), &driver.ExecuteRequest{
		IdempotencyKey: "k1", Action: "dump",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.State != driver.CommandStateFailed {
		t.Fatalf("state = %v, want failed (device not open)", resp.State)
	}
}

func TestConfigureInstanceRejectsBadJSON(t *testing.T) {
	d := New()
	resp, err := d.ConfigureInstance(context.Background(), &driver.ConfigureInstanceRequest{
		PluginInstanceID: "i1", Config: []byte("not json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status.IsOK() {
		t.Fatal("expected invalid-argument status")
	}
}

// TestManifestConsistency 交叉锁定 plugin.yaml 与代码常量：
// 贡献 id 必须是 stcb，capability 清单必须与代码一致。
func TestManifestConsistency(t *testing.T) {
	b, err := os.ReadFile("../plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	checks := []string{
		"kind: Driver",
		"id: io.github.deliciousbuding.cloud-path-driver-stcb",
		"entrypoint: cloudpath-driver-stcb",
		"    - id: stcb",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Fatalf("plugin.yaml missing %q", c)
		}
	}
	for _, cap := range []string{capClock, capAlarm, capContact, capTemp, capIllum, capHall, capVib, capKey, capBuzzer, capLED, capDisplay, capMotor} {
		if !strings.Contains(s, cap) {
			t.Fatalf("plugin.yaml missing capability %q", cap)
		}
	}
}
