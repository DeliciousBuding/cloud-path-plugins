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
	// 'D' 在板上是 ISP 下载模式：legacy 编码器绝不能把诊断命令编成 'D'。
	if _, err := encodeCommand("diag", ""); err == nil {
		t.Fatal("expected legacy diag to be rejected (ISP footgun)")
	}
	// 药盒业务动词不属于硬件 Driver。
	for _, business := range []string{"dump", "trigger", "open"} {
		if _, err := encodeCommand(business, ""); err == nil {
			t.Fatalf("expected business action %q to be rejected", business)
		}
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
	// 与 pluginVersion 常量对齐而不是写死字面量：版本号只在 pluginVersion 与
	// plugin.yaml 两处存在，二者的一致性由 TestManifestConsistency 交叉锁定。
	if desc.Version != pluginVersion {
		t.Fatalf("Version = %q, want pluginVersion %q", desc.Version, pluginVersion)
	}
	if len(desc.Capabilities) != 13 {
		t.Fatalf("capabilities = %d, want 13", len(desc.Capabilities))
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
		IdempotencyKey: "k1", Action: "sensor",
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
		"version: " + pluginVersion,
		"id: io.github.deliciousbuding.cloud-path-driver-stcb",
		"entrypoint: cloudpath-driver-stcb",
		"    - id: stcb",
		"      ui:",
		"        apiVersion: 1",
		"        device:",
		"          sections:",
		"            - type: status",
		"              source: device",
		"            - type: diagnostics",
		"              source: diagnostics",
		"            - type: actions",
		"              source: device-actions",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Fatalf("plugin.yaml missing %q", c)
		}
	}
	if strings.Contains(s, "navigation:") {
		t.Fatal("Driver UI must not register main navigation")
	}
	for _, cap := range []string{capClock, capTemp, capIllum, capAnalog, capNav, capHall, capVib, capKey, capBuzzer, capLED, capDisplay, capMotor, capDiag} {
		if !strings.Contains(s, cap) {
			t.Fatalf("plugin.yaml missing capability %q", cap)
		}
	}
}

// TestBoardDiagnosticsUsesPublisherNamespace 锁定命名空间规则：Driver 专有能力不得占用
// 平台 cloudpath.dev 词汇。真实事故——Core 参考 demo 适配器也声明
// cloudpath.dev/capability/diagnostics@1（action 是 dump/noop/ping），Server catalog 按 ID
// 去重且进程内优先，结果真机设备页丢掉 diag 按钮、反而出现 demo 的动词。
func TestBoardDiagnosticsUsesPublisherNamespace(t *testing.T) {
	if !strings.HasPrefix(capDiag, "io.github.deliciousbuding/capability/") {
		t.Fatalf("板级诊断能力 = %q, 必须用发布者命名空间（不得占用 cloudpath.dev 平台词汇）", capDiag)
	}
	for _, c := range capabilityDescriptors() {
		if c.ID == "cloudpath.dev/capability/diagnostics@1" {
			t.Fatal("不得再声明 cloudpath.dev/capability/diagnostics@1：会与 Core 参考 demo 适配器冲突")
		}
	}
}
