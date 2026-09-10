package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

// 本文件锁定 ConfigureInstance 的**线上形状**与**实例归属**契约。
//
// 同一个 Driver 进程会从两个平台生产者收到实例配置，而两者编码不同：
//
//   - Server desired state（cloud-path internal/pluginhost.configureInstance）：
//     Instance.Config 是 map[string]string，json.Marshal 之后**所有值都是 JSON 字符串**，
//     所以 `baud` 在线上是 "115200" 而不是 115200。
//   - Edge 设备桥（cloud-path internal/edge/external_driver.go）：按 edge.yaml 的原生类型
//     编码，`baud` 是 JSON 数字，并额外带 `extra` 映射（protocol 等）。
//
// 0.2.2 把 Baud 声明成 int，只接受第二种形状。Core v0.2.14 起既有实例的重配置会真正下发
// 第一种形状，于是整份配置被 InvalidArgument 拒绝、实例无法收敛（真机链路上这是致命的）。
// 两种形状都必须接受，而真正非法的值仍须诚实拒绝——不能为了兼容把校验一起放掉。
//
// 归属：SDK 的每个请求都带 PluginInstanceID，配置是**每实例**的，不是进程级的。两个生产者
// 用不同实例 ID 配置同一进程时不得互相覆盖，否则后写入的一方会抹掉前一方的 extra/protocol。
const (
	platformInstance = "default/stcb-driver"
	bridgeInstance   = pluginID + "/stcb-real-1"
)

// platformShape 是 Server desired state 的逐字节形状：值全为字符串，无 extra。
const platformShape = `{"baud":"115200","device_id":"stcb-real-1","name":"STC-B real board (COM3)","port":"COM3"}`

// bridgeShape 是 Edge 设备桥的形状：数字标量 + extra 映射。
const bridgeShape = `{"baud":115200,"device_id":"stcb-real-1","extra":{"protocol":"v1"},"name":"STC-B real board (COM3)","port":"COM3"}`

type discoverySink struct{ events []*driver.DiscoveryEvent }

func (s *discoverySink) Send(_ context.Context, m *driver.DiscoveryEvent) error {
	s.events = append(s.events, m)
	return nil
}

func configure(t *testing.T, d *Driver, instanceID, config string, revision uint32) {
	t.Helper()
	resp, err := d.ConfigureInstance(context.Background(), &driver.ConfigureInstanceRequest{
		PluginInstanceID: instanceID,
		Config:           []byte(config),
		ConfigRevision:   revision,
	})
	if err != nil {
		t.Fatalf("ConfigureInstance(%s) transport: %v", instanceID, err)
	}
	if resp == nil || resp.Status == nil {
		t.Fatalf("ConfigureInstance(%s) returned no status", instanceID)
	}
	if !resp.Status.IsOK() {
		t.Fatalf("ConfigureInstance(%s) status = %v, want ok", instanceID, resp.Status)
	}
	if resp.AppliedRevision != revision {
		t.Fatalf("ConfigureInstance(%s) applied revision = %d, want %d", instanceID, resp.AppliedRevision, revision)
	}
}

func configureWantRejected(t *testing.T, d *Driver, instanceID, config string) {
	t.Helper()
	resp, err := d.ConfigureInstance(context.Background(), &driver.ConfigureInstanceRequest{
		PluginInstanceID: instanceID, Config: []byte(config), ConfigRevision: 3,
	})
	if err != nil {
		t.Fatalf("ConfigureInstance(%s) transport: %v", instanceID, err)
	}
	if resp == nil || resp.Status == nil || resp.Status.IsOK() {
		t.Fatalf("ConfigureInstance(%s) accepted %s, want invalid-argument", instanceID, config)
	}
}

// discoverFound 走公开 Discover 流读回该实例当前生效的设备身份，不触碰串口。
func discoverFound(t *testing.T, d *Driver, instanceID string) *driver.DiscoveryFoundDevice {
	t.Helper()
	sink := &discoverySink{}
	if err := d.Discover(context.Background(), &driver.DiscoverRequest{PluginInstanceID: instanceID}, sink); err != nil {
		t.Fatalf("Discover(%s): %v", instanceID, err)
	}
	for _, e := range sink.events {
		if found, ok := e.Union.(*driver.DiscoveryFoundDevice); ok {
			return found
		}
	}
	t.Fatalf("Discover(%s) reported no device", instanceID)
	return nil
}

func TestDiscoverReportsOnlyTerminalCountForUsableConfig(t *testing.T) {
	d := New()
	configure(t, d, platformInstance, platformShape, 7)

	sink := &discoverySink{}
	if err := d.Discover(context.Background(), &driver.DiscoverRequest{PluginInstanceID: platformInstance}, sink); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	found := 0
	var finished *driver.DiscoveryFinished
	for _, event := range sink.events {
		switch body := event.Union.(type) {
		case *driver.DiscoveryFoundDevice:
			found++
		case *driver.DiscoveryFinished:
			finished = body
		case *driver.DiscoveryFailed:
			t.Fatalf("usable config failed discovery: %v", body.Status)
		}
	}
	if found != 1 || finished == nil || finished.FoundCount != 1 {
		t.Fatalf("found=%d finished=%+v, want exactly one terminal device", found, finished)
	}
}

func TestDiscoverFailsClosedForIncompleteConfig(t *testing.T) {
	cases := []struct {
		name        string
		config      string
		wantMessage string
	}{
		{"missing-config", "", "device_id"},
		{"missing-device-id", `{"port":"COM3"}`, "device_id"},
		{"missing-port", `{"device_id":"board-1"}`, "port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := New()
			if tc.config != "" {
				configure(t, d, platformInstance, tc.config, 1)
			}
			sink := &discoverySink{}
			if err := d.Discover(context.Background(), &driver.DiscoverRequest{PluginInstanceID: platformInstance}, sink); err != nil {
				t.Fatalf("Discover transport: %v", err)
			}

			found := 0
			failed := 0
			for _, event := range sink.events {
				switch body := event.Union.(type) {
				case *driver.DiscoveryFoundDevice:
					found++
				case *driver.DiscoveryFinished:
					t.Fatalf("incomplete config fabricated Finished(%d)", body.FoundCount)
				case *driver.DiscoveryFailed:
					failed++
					if body.Status == nil || body.Status.Code != status.CodeFailedPrecondition {
						t.Fatalf("failed status = %+v, want FAILED_PRECONDITION", body.Status)
					}
					if !strings.Contains(body.Status.Message, tc.wantMessage) {
						t.Fatalf("failed message = %q, want substring %q", body.Status.Message, tc.wantMessage)
					}
				}
			}
			if found != 0 || failed != 1 {
				t.Fatalf("found=%d failed=%d, want found=0 failed=1", found, failed)
			}
		})
	}
}

func TestConfigureInstanceAcceptsPlatformStringScalars(t *testing.T) {
	d := New()
	configure(t, d, platformInstance, platformShape, 7)

	found := discoverFound(t, d, platformInstance)
	if found.DeviceID != "stcb-real-1" {
		t.Fatalf("device_id = %q, want stcb-real-1", found.DeviceID)
	}
	if found.ExternalID != "COM3" {
		t.Fatalf("external_id = %q, want COM3", found.ExternalID)
	}
}

func TestConfigureInstanceAcceptsEdgeBridgeTypedScalars(t *testing.T) {
	d := New()
	configure(t, d, bridgeInstance, bridgeShape, 4)

	found := discoverFound(t, d, bridgeInstance)
	if found.DeviceID != "stcb-real-1" || found.ExternalID != "COM3" {
		t.Fatalf("found = %+v, want stcb-real-1 / COM3", found)
	}
}

// 兼容不是放弃校验：无法解析成整数的波特率仍须被拒绝，而不是静默取零值去开串口。
func TestConfigureInstanceRejectsNonNumericBaud(t *testing.T) {
	d := New()
	configureWantRejected(t, d, platformInstance, `{"baud":"one-hundred","device_id":"stcb-real-1","port":"COM3"}`)
	configureWantRejected(t, d, platformInstance, `{"baud":115200.5,"device_id":"stcb-real-1","port":"COM3"}`)
	configureWantRejected(t, d, platformInstance, `{"baud":true,"device_id":"stcb-real-1","port":"COM3"}`)
}

// 空字符串等同「未设置」，保留既有默认值语义（baud 由 connection_hints 或默认补齐），
// 不能因为宽松解析就把它当成 0 波特率。
func TestConfigureInstanceTreatsEmptyScalarAsUnset(t *testing.T) {
	d := New()
	configure(t, d, platformInstance, `{"baud":"","device_id":"stcb-real-1","port":"COM3"}`, 5)
	if found := discoverFound(t, d, platformInstance); found.ExternalID != "COM3" {
		t.Fatalf("external_id = %q, want COM3", found.ExternalID)
	}
}
