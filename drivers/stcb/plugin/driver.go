package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

// 稳定身份（一经发布即为机器契约，破坏性语义变化升 @2，不得原地改 @1）。
const (
	pluginID      = "io.github.deliciousbuding.cloud-path-driver-stcb"
	pluginVersion = "0.2.6"

	// driverID 是 Describe 上报的稳定 driver 标识，与 plugin.yaml contributes.drivers[0].id 一致。
	driverID = "stcb"
)

// STC-B 使用的 Capability 引用（cloudpath.dev 命名空间）。
const (
	capClock = "cloudpath.dev/capability/clock@1"

	capTemp    = "cloudpath.dev/capability/temperature@1"
	capIllum   = "cloudpath.dev/capability/illuminance@1"
	capHall    = "cloudpath.dev/capability/hall@1"
	capVib     = "cloudpath.dev/capability/vibration@1"
	capKey     = "cloudpath.dev/capability/key@1"
	capBuzzer  = "cloudpath.dev/capability/buzzer@1"
	capLED     = "cloudpath.dev/capability/led@1"
	capDisplay = "cloudpath.dev/capability/display-text@1"
	capMotor   = "cloudpath.dev/capability/motor@1"
	capAnalog  = "cloudpath.dev/capability/analog-input@1"
	capNav     = "cloudpath.dev/capability/navigation@1"
	// capDiag 用**发布者命名空间**而不是 cloudpath.dev：它暴露的是 STC-B 板级原始端口
	// 电平（P1/P2/P3 + hall_pin/vib_pin），是本 Driver 专有的诊断面，不是平台标准词汇。
	// 实测教训：平台的参考 demo 适配器也声明 cloudpath.dev/capability/diagnostics@1，
	// 但 action 集是 dump/noop/ping；Server catalog 按 ID 去重、进程内优先，于是本板的
	// diag 按钮被 demo 的动词顶掉（真机验证时 /api/capabilities 只返回 dump/noop/ping）。
	// capability-model.md 的命名空间规则正是为此存在：第三方能力用发布者命名空间。
	capDiag = "io.github.deliciousbuding/capability/board-diagnostics@1"

	// syncInterval 是 Watch 周期对时（T+HHMM）的间隔。真实板掉电后小时/相位会漂移，
	// 因此外部 Driver 自己在 Watch 循环内周期对时，不依赖 Core 注入生命周期命令。
	syncInterval = 10 * time.Minute
)

// entityDef 描述一台 STC-B 的静态 Entity。
type entityDef struct {
	ID   string
	Name string
	Cat  driver.EntityCategory
	Cap  string
}

// entities 是 STC-B 板载硬件的能力投影；业务实体由 Application Plugin 组合。
var entities = []entityDef{
	{ID: "clock", Name: "实时时钟", Cat: driver.EntityCategorySensor, Cap: capClock},
	{ID: "temperature", Name: "温度", Cat: driver.EntityCategorySensor, Cap: capTemp},
	{ID: "illuminance", Name: "光照", Cat: driver.EntityCategorySensor, Cap: capIllum},
	{ID: "navigation", Name: "KN 导航摇杆", Cat: driver.EntityCategorySensor, Cap: capNav},
	{ID: "ext0", Name: "EXT ADC0", Cat: driver.EntityCategorySensor, Cap: capAnalog},
	{ID: "ext1", Name: "EXT ADC1", Cat: driver.EntityCategorySensor, Cap: capAnalog},
	{ID: "hall", Name: "霍尔", Cat: driver.EntityCategorySensor, Cap: capHall},
	{ID: "vibration", Name: "振动", Cat: driver.EntityCategorySensor, Cap: capVib},
	{ID: "key1", Name: "按键 K1", Cat: driver.EntityCategorySensor, Cap: capKey},
	{ID: "key2", Name: "按键 K2", Cat: driver.EntityCategorySensor, Cap: capKey},
	{ID: "key3", Name: "按键 K3", Cat: driver.EntityCategorySensor, Cap: capKey},
	{ID: "buzzer", Name: "蜂鸣器", Cat: driver.EntityCategoryActuator, Cap: capBuzzer},
	{ID: "led-bank", Name: "LED L0-L7", Cat: driver.EntityCategoryActuator, Cap: capLED},
	{ID: "display", Name: "8 位数码管", Cat: driver.EntityCategoryActuator, Cap: capDisplay},
	{ID: "motor", Name: "步进电机接口", Cat: driver.EntityCategoryActuator, Cap: capMotor},
	{ID: "diagnostics", Name: "板级诊断", Cat: driver.EntityCategoryDiagnostic, Cap: capDiag},
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

var buzzerActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"freq":     map[string]any{"type": "integer", "minimum": 0, "maximum": 9, "title": "频率档"},
		"duration": map[string]any{"type": "integer", "minimum": 0, "maximum": 9, "title": "时长档"},
	},
	"required": []any{"freq", "duration"},
}

var toneActionSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"frequency_hz": map[string]any{"type": "integer", "minimum": 1, "maximum": 4000, "title": "频率 (Hz)"},
		"duration_ms":  map[string]any{"type": "integer", "minimum": 10, "maximum": 1200, "multipleOf": 10, "title": "时长 (ms，10 的倍数)"},
	},
	"required": []any{"frequency_hz", "duration_ms"},
}

var toneSequenceActionSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"notes": map[string]any{
			"type":     "array",
			"minItems": 1,
			"maxItems": maxToneSequenceNotes,
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"frequency_hz": map[string]any{"type": "integer", "minimum": 1, "maximum": 4000},
					"duration_ms":  map[string]any{"type": "integer", "minimum": 10, "maximum": 1200, "multipleOf": 10},
				},
				"required": []any{"frequency_hz", "duration_ms"},
			},
		},
		"gap_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": maxSequenceGapMS, "multipleOf": 10, "default": 0, "title": "音符间隔 (ms)"},
	},
	"required": []any{"notes"},
}

var ledActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"mask":    map[string]any{"type": "integer", "minimum": 0, "maximum": 255, "title": "L0-L7 位掩码"},
		"pattern": map[string]any{"type": "integer", "minimum": 0, "maximum": 9, "title": "兼容档（0=灭、1-8=单灯、9=全亮）"},
	},
	"oneOf": []any{map[string]any{"required": []any{"mask"}}, map[string]any{"required": []any{"pattern"}}},
}

var displayActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"digits": map[string]any{
			"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 9},
			"minItems": 8, "maxItems": 8, "title": "8 位数字（0-9）",
		},
		"codes": map[string]any{
			"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 25},
			"minItems": 8, "maxItems": 8, "title": "8 位字形码（0-25：数字/空白/横线/H/L/小数点数字）",
		},
		"mode": map[string]any{"type": "string", "enum": []any{"clock"}, "title": "恢复 HH-MM-SS 实时时钟"},
	},
	"oneOf": []any{
		map[string]any{"required": []any{"digits"}},
		map[string]any{"required": []any{"codes"}},
		map[string]any{"required": []any{"mode"}},
	},
}

var motorActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"steps": map[string]any{"type": "integer", "minimum": 0, "maximum": 4, "title": "步数档（0=停）"},
	},
	"required": []any{"steps"},
}

// capabilityDescriptors 返回板载硬件的稳定 Capability 描述。
func capabilityDescriptors() []driver.CapabilityDescriptor {
	return []driver.CapabilityDescriptor{
		{ID: capClock, Title: "实时时钟", Properties: []driver.PropertyDescriptor{{Name: "time", Type: "string", Access: "read"}}},
		{ID: capTemp, Title: "温度", Properties: []driver.PropertyDescriptor{{Name: "value", Type: "number", Unit: "Cel", Access: "read", Quality: []string{"good"}}}},
		{ID: capIllum, Title: "光照", Properties: []driver.PropertyDescriptor{{Name: "value", Type: "number", Access: "read", Quality: []string{"good"}}}},
		{ID: capAnalog, Title: "模拟输入", Properties: []driver.PropertyDescriptor{{Name: "raw", Type: "integer", Access: "read"}}},
		{ID: capNav, Title: "导航摇杆", Properties: []driver.PropertyDescriptor{{Name: "raw", Type: "integer", Access: "read"}, {Name: "direction", Type: "integer", Access: "read"}}, Events: []driver.EventDescriptor{{Name: "direction", PayloadSchemaJSON: "{}"}}},
		{ID: capHall, Title: "磁场检测", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "integer", Access: "read"}}, Events: []driver.EventDescriptor{{Name: "changed", PayloadSchemaJSON: "{}"}}},
		{ID: capVib, Title: "振动检测", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "integer", Access: "read"}}, Events: []driver.EventDescriptor{{Name: "quake", PayloadSchemaJSON: "{}"}}},
		{ID: capKey, Title: "按键", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "integer", Access: "read"}}, Events: []driver.EventDescriptor{{Name: "pressed", PayloadSchemaJSON: "{}"}, {Name: "released", PayloadSchemaJSON: "{}"}}},
		{ID: capBuzzer, Title: "蜂鸣器", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "string", Access: "read"}}, Actions: []driver.ActionDescriptor{{Name: actionBuzzer, Title: "播放提示音", Description: "按频率档和时长档播放，完成后返回设备回执。", InputSchemaJSON: mustJSON(buzzerActionSchema)}, {Name: actionTone, Title: "播放原始音调", Description: "按 1-4000 Hz 频率和 10-1200 ms 时长播放，完成后返回设备回执。", InputSchemaJSON: mustJSON(toneActionSchema)}, {Name: actionToneSequence, Title: "播放音序", Description: "一次提交 1-64 个音符；Driver 本地按序执行，内置曲目走固件原生音序器。", InputSchemaJSON: mustJSON(toneSequenceActionSchema)}}},
		{ID: capLED, Title: "LED 灯组", Properties: []driver.PropertyDescriptor{{Name: "mask", Type: "integer", Access: "read"}}, Actions: []driver.ActionDescriptor{{Name: actionLED, Title: "设置指示灯", Description: "mask 与 pattern 二选一；mask 的每一位对应 L0–L7。", InputSchemaJSON: mustJSON(ledActionSchema)}}},
		{ID: capDisplay, Title: "数码管", Properties: []driver.PropertyDescriptor{{Name: "mode", Type: "string", Access: "read"}}, Actions: []driver.ActionDescriptor{{Name: actionDisplay, Title: "设置数码管", Description: "digits、codes、mode 三选一；mode 为 clock 时恢复时钟。", InputSchemaJSON: mustJSON(displayActionSchema)}}},
		{ID: capMotor, Title: "步进电机接口", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "string", Access: "read"}}, Actions: []driver.ActionDescriptor{{Name: actionMotor, Title: "控制步进电机", Description: "steps 为步数档；0 停止，1–4 对应 50–200 步。", InputSchemaJSON: mustJSON(motorActionSchema)}}},
		{ID: capDiag, Title: "板级诊断", Actions: []driver.ActionDescriptor{{Name: "diag", Title: "读取板级诊断", Description: "读取原始端口与输入状态，不驱动执行器。", InputSchemaJSON: "{}"}}},
	}
}

// instanceConfig 是插件实例配置（来自 ConfigureInstance 的 Config 字节）。
//
// 线上有**两个**平台生产者，编码不同，本结构必须同时接受：
//
//   - Server desired state（cloud-path internal/pluginhost.configureInstance）：
//     Instance.Config 是 map[string]string，json.Marshal 之后所有值都是 JSON 字符串，
//     数值字段在线上是 "115200" 这样的字符串。这是插件实例配置的权威契约。
//   - Edge 设备桥（cloud-path internal/edge/external_driver.go）：按 edge.yaml 的原生
//     类型编码，数值字段是 JSON 数字，并额外带 extra 映射。
//
// 把数值字段声明成 int 只接受第二种。Core v0.2.14 起既有实例的重配置会真正下发第一种，
// 于是整份配置被 InvalidArgument 拒绝、实例无法收敛——真机上这等于设备链路起不来。
// 所以数值字段一律用 configScalar，两种形状都收，取值校验一律不放。
type instanceConfig struct {
	DeviceID      string            `json:"device_id"`
	Name          string            `json:"name"`
	Port          string            `json:"port"`
	Baud          configScalar      `json:"baud"`
	PollIntervalS configScalar      `json:"poll_interval_s"`
	Extra         map[string]string `json:"extra"`
}

// configScalar 是一个整数配置值，接受 JSON 数字或等价的 JSON 字符串。
//
// 宽松只针对**编码形状**，不针对取值：空字符串与 null 等同「未设置」，保留零值走既有
// 默认；无法解析成整数的值仍然报错，由 ConfigureInstance 诚实拒绝，绝不静默取零值去开
// 串口。口径与 openConfigFor 里 connection_hints.baud 的既有 strconv 处理一致。
type configScalar int

func (s *configScalar) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] != '"' {
		var n int
		if err := json.Unmarshal(trimmed, &n); err != nil {
			return fmt.Errorf("须为整数或整数字符串: %w", err)
		}
		*s = configScalar(n)
		return nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err != nil {
		return fmt.Errorf("须为整数或整数字符串: %w", err)
	}
	if text = strings.TrimSpace(text); text == "" {
		return nil
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("须为整数或整数字符串: %w", err)
	}
	*s = configScalar(n)
	return nil
}

func (c instanceConfig) pollInterval() time.Duration {
	if c.PollIntervalS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.PollIntervalS) * time.Second
}

var _ driver.DriverServer = (*Driver)(nil)

// Driver 是 STC-B 的 Driver Protocol v1 实现。一个进程管理多台设备。
type Driver struct {
	mu          sync.Mutex
	initialized bool
	shutdown    bool
	negotiated  uint32
	runtimeID   string
	instanceID  string

	cfg instanceConfig

	devMu   sync.Mutex
	devices map[string]*device

	// subscribers fan out per-device events/progress to every matching Watch.
	subsMu   sync.Mutex
	subsNext uint64
	subs     map[uint64]watchSubscriber

	seqMu sync.Mutex
	seq   uint64
}

type watchMessage struct {
	deviceID string
	union    driver.DriverMessageUnion
}

type watchSubscriber struct {
	deviceIDs map[string]struct{}
	ch        chan watchMessage
}

// New 返回一个新的 Driver。
func New() *Driver {
	return &Driver{
		devices: map[string]*device{},
		subs:    map[uint64]watchSubscriber{},
	}
}

func (d *Driver) nextSeq() uint64 {
	d.seqMu.Lock()
	defer d.seqMu.Unlock()
	d.seq++
	return d.seq
}

func (d *Driver) publish(deviceID string, union driver.DriverMessageUnion) {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for _, sub := range d.subs {
		if _, ok := sub.deviceIDs[deviceID]; !ok {
			continue
		}
		select {
		case sub.ch <- watchMessage{deviceID: deviceID, union: union}:
		default:
		}
	}
}

func (d *Driver) pushEvent(deviceID, entityID, eventType string) {
	d.publish(deviceID, &driver.Event{DeviceID: deviceID, EntityID: entityID, EventType: eventType, OccurredAt: time.Now().UTC().Format(time.RFC3339)})
}

func (d *Driver) pushProgress(deviceID string, p *driver.CommandProgress) {
	d.publish(deviceID, p)
}

func (d *Driver) subscribe(deviceIDs []string) (uint64, <-chan watchMessage) {
	set := make(map[string]struct{}, len(deviceIDs))
	for _, id := range deviceIDs {
		set[id] = struct{}{}
	}
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	d.subsNext++
	id := d.subsNext
	ch := make(chan watchMessage, 64)
	d.subs[id] = watchSubscriber{deviceIDs: set, ch: ch}
	return id, ch
}

func (d *Driver) unsubscribe(id uint64) {
	d.subsMu.Lock()
	delete(d.subs, id)
	d.subsMu.Unlock()
}

func unavailable() *status.Status {
	return status.Errorf(status.CodeUnavailable, "stcb driver shut down")
}

// Initialize 协商协议版本。
func (d *Driver) Initialize(_ context.Context, req *driver.InitializeRequest) (*driver.InitializeResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return nil, unavailable()
	}
	v, ok := driver.NegotiateProtocolVersion(req.SupportedProtocolVersions, req.ProtocolVersion, 1, driver.ProtocolVersion)
	if !ok {
		return &driver.InitializeResponse{Status: status.Errorf(status.CodeFailedPrecondition, "no common protocol version")}, nil
	}
	d.initialized = true
	d.negotiated = v
	if d.runtimeID == "" {
		d.runtimeID = "stcb-driver-" + req.LaunchID
	}
	return &driver.InitializeResponse{NegotiatedProtocolVersion: v, Status: status.New(), RuntimeID: d.runtimeID}, nil
}

// Describe 返回稳定的驱动描述。
func (d *Driver) Describe(context.Context) (*driver.DriverDescriptor, error) {
	return &driver.DriverDescriptor{
		DriverID:       driverID,
		Version:        pluginVersion,
		SchemaVersions: []string{driver.SchemaVersion},
		Capabilities:   capabilityDescriptors(),
	}, nil
}

// ConfigureInstance 保存插件实例配置（端口/设备身份）。
func (d *Driver) ConfigureInstance(_ context.Context, req *driver.ConfigureInstanceRequest) (*driver.ConfigureInstanceResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return nil, unavailable()
	}
	var cfg instanceConfig
	if len(req.Config) > 0 {
		if err := json.Unmarshal(req.Config, &cfg); err != nil {
			return &driver.ConfigureInstanceResponse{
				PluginInstanceID: req.PluginInstanceID,
				Status:           status.Errorf(status.CodeInvalidArgument, "config 须为 JSON: %v", err),
			}, nil
		}
	}
	d.instanceID = req.PluginInstanceID
	d.cfg = cfg
	return &driver.ConfigureInstanceResponse{
		PluginInstanceID: req.PluginInstanceID,
		AppliedRevision:  req.ConfigRevision,
		Status:           status.New(),
	}, nil
}

// Discover 手工发现：上报配置中指向的那台 STC-B（无自动扫描）。
func (d *Driver) Discover(_ context.Context, req *driver.DiscoverRequest, stream driver.DiscoveryWriter) error {
	did := req.DiscoveryID
	if did == "" {
		did = req.PluginInstanceID
	}
	send := func(seq uint64, union driver.DiscoveryEventUnion) error {
		return stream.Send(context.Background(), &driver.DiscoveryEvent{
			PluginInstanceID: req.PluginInstanceID,
			Sequence:         seq,
			SchemaVersion:    driver.SchemaVersion,
			DiscoveryID:      did,
			Union:            union,
		})
	}
	if err := send(d.nextSeq(), &driver.DiscoveryStarted{DiscoveryID: did}); err != nil {
		return err
	}
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()
	if cfg.DeviceID != "" {
		devID := cfg.DeviceID
		ext := cfg.Port
		if err := send(d.nextSeq(), &driver.DiscoveryFoundDevice{DeviceID: devID, ExternalID: ext, Manufacturer: "STC-B", Model: "IAP15F2K61S2"}); err != nil {
			return err
		}
	}
	return send(d.nextSeq(), &driver.DiscoveryFinished{FoundCount: 1})
}

// openConfigFor 解析一台设备的打开参数：优先 OpenDevice 的 ConnectionHints，
// 回落实例级默认配置。ConnectionHints 由 Edge 从 edge.yaml 注入（port/baud/name）。
func (d *Driver) openConfigFor(req *driver.OpenDeviceRequest) (string, deviceConfig, error) {
	d.mu.Lock()
	def := d.cfg
	d.mu.Unlock()

	deviceID := req.DeviceID
	if deviceID == "" {
		deviceID = def.DeviceID
	}
	if deviceID == "" {
		return "", deviceConfig{}, fmt.Errorf("device id 未配置")
	}
	protocol := ""
	if def.Extra != nil {
		protocol = def.Extra["protocol"]
	}
	cfg := deviceConfig{ID: deviceID, Name: def.Name, Port: def.Port, Baud: int(def.Baud), Protocol: protocol}
	if req.ConnectionHints != nil {
		if v := req.ConnectionHints["port"]; v != "" {
			cfg.Port = v
		}
		if v := req.ConnectionHints["baud"]; v != "" {
			b, aerr := strconv.Atoi(v)
			if aerr != nil {
				return "", deviceConfig{}, fmt.Errorf("connection_hints.baud 非法: %w", aerr)
			}
			cfg.Baud = b
		}
		if v := req.ConnectionHints["name"]; v != "" {
			cfg.Name = v
		}
		if v := req.ConnectionHints["protocol"]; v != "" {
			cfg.Protocol = v
		}
	}
	if cfg.Port == "" {
		return "", deviceConfig{}, fmt.Errorf("端口未配置（config.port 或 connection_hints.port 必填）")
	}
	return deviceID, cfg, nil
}

// OpenDevice 打开串口并启动 RX 循环。
func (d *Driver) OpenDevice(ctx context.Context, req *driver.OpenDeviceRequest) (*driver.OpenDeviceResponse, error) {
	d.mu.Lock()
	shutdown := d.shutdown
	d.mu.Unlock()
	if shutdown {
		return nil, unavailable()
	}
	deviceID, cfg, err := d.openConfigFor(req)
	if err != nil {
		return &driver.OpenDeviceResponse{
			PluginInstanceID: req.PluginInstanceID,
			DeviceID:         req.DeviceID,
			Status:           status.Errorf(status.CodeFailedPrecondition, "%v", err),
		}, nil
	}

	d.devMu.Lock()
	defer d.devMu.Unlock()
	if _, ok := d.devices[deviceID]; ok {
		return &driver.OpenDeviceResponse{PluginInstanceID: req.PluginInstanceID, DeviceID: deviceID, Status: status.New()}, nil
	}
	dev, err := openDevice(ctx, cfg, func(entityID, eventType string) { d.pushEvent(deviceID, entityID, eventType) })
	if err != nil {
		return &driver.OpenDeviceResponse{
			PluginInstanceID: req.PluginInstanceID,
			DeviceID:         deviceID,
			Status:           status.Errorf(status.CodeUnavailable, "打开串口失败: %v", err),
		}, nil
	}
	d.devices[deviceID] = dev
	return &driver.OpenDeviceResponse{PluginInstanceID: req.PluginInstanceID, DeviceID: deviceID, Status: status.New()}, nil
}

// CloseDevice 关闭串口。
func (d *Driver) CloseDevice(_ context.Context, req *driver.CloseDeviceRequest) (*driver.CloseDeviceResponse, error) {
	d.devMu.Lock()
	dev := d.devices[req.DeviceID]
	delete(d.devices, req.DeviceID)
	d.devMu.Unlock()
	if dev != nil {
		_ = dev.Close()
	}
	return &driver.CloseDeviceResponse{PluginInstanceID: req.PluginInstanceID, DeviceID: req.DeviceID, Status: status.New()}, nil
}

// deviceIDsFor 决定 Watch 应上报的设备集合：优先请求显式列表，再回落实例默认设备，
// 最后回落当前已打开的全体设备。
func (d *Driver) deviceIDsFor(ids []string) []string {
	if len(ids) > 0 {
		return ids
	}
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()
	if cfg.DeviceID != "" {
		return []string{cfg.DeviceID}
	}
	d.devMu.Lock()
	defer d.devMu.Unlock()
	if len(d.devices) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.devices))
	for id := range d.devices {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// device 返回指定设备（未打开返回 nil）。
func (d *Driver) device(deviceID string) *device {
	d.devMu.Lock()
	defer d.devMu.Unlock()
	return d.devices[deviceID]
}

// sendSnapshot 发送一台设备的 DeviceUpsert + 14 个 EntityUpsert。
func (d *Driver) sendSnapshot(send func(deviceID string, union driver.DriverMessageUnion) error, deviceID string, dev *device) error {
	statusNow := driver.DeviceStatusUnavailable
	external := ""
	name := ""
	if dev != nil {
		statusNow = driver.DeviceStatusOnline
		external = dev.portName
		name = dev.cfg.Name
	}
	if err := send(deviceID, &driver.DeviceUpsert{Device: driver.Device{
		DeviceID: deviceID, ExternalID: external, Manufacturer: "STC-B",
		Model: "IAP15F2K61S2", Status: statusNow, DisplayName: name,
	}}); err != nil {
		return err
	}
	for _, e := range entities {
		if err := send(deviceID, &driver.EntityUpsert{Entity: driver.Entity{
			EntityID: e.ID, DeviceID: deviceID, UniqueKey: e.ID, Name: e.Name,
			Category: e.Cat, Capabilities: []string{e.Cap},
		}}); err != nil {
			return err
		}
	}
	return nil
}

// Watch 打开观测流：先发 Device/Entity 快照，再周期取帧上报观测与事件。
func (d *Driver) Watch(ctx context.Context, req *driver.WatchRequest, stream driver.DriverMessageWriter) error {
	d.mu.Lock()
	if d.shutdown {
		d.mu.Unlock()
		return unavailable()
	}
	if !d.initialized {
		d.mu.Unlock()
		return status.Errorf(status.CodeFailedPrecondition, "Initialize required before Watch")
	}
	poll := d.cfg.pollInterval()
	d.mu.Unlock()

	deviceIDs := d.deviceIDsFor(req.DeviceIDs)
	if len(deviceIDs) == 0 {
		return status.Errorf(status.CodeInvalidArgument, "device id 未配置")
	}
	for _, deviceID := range deviceIDs {
		if dev := d.device(deviceID); dev != nil && dev.isProtocolV1() && poll > time.Second {
			poll = time.Second
		}
	}

	send := func(deviceID string, union driver.DriverMessageUnion) error {
		return stream.Send(ctx, &driver.DriverMessage{
			PluginInstanceID: req.PluginInstanceID,
			Sequence:         d.nextSeq(),
			SchemaVersion:    driver.SchemaVersion,
			DeviceID:         deviceID,
			Union:            union,
		})
	}

	for _, deviceID := range deviceIDs {
		if err := d.sendSnapshot(send, deviceID, d.device(deviceID)); err != nil {
			return err
		}
	}
	watchID, messages := d.subscribe(deviceIDs)
	defer d.unsubscribe(watchID)

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	syncTicker := time.NewTicker(syncInterval)
	defer syncTicker.Stop()
	var deviceDone <-chan struct{}
	if len(deviceIDs) == 1 {
		if dev := d.device(deviceIDs[0]); dev != nil {
			deviceDone = dev.Done()
		}
	}

	d.syncAll(deviceIDs)

	reqFrame := func(deviceID string) {
		dev := d.device(deviceID)
		if dev == nil || dev.isProtocolV1() {
			return
		}
		_ = dev.write([]byte("S"))
		_ = dev.write([]byte("V"))
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deviceDone:
			return nil
		case <-ticker.C:
			for _, deviceID := range deviceIDs {
				reqFrame(deviceID)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			for _, deviceID := range deviceIDs {
				if dev := d.device(deviceID); dev != nil {
					d.emitObservations(send, deviceID, dev)
					d.emitDiagnostic(send, deviceID, dev)
				}
			}
		case <-syncTicker.C:
			d.syncAll(deviceIDs)
		case msg := <-messages:
			_ = send(msg.deviceID, msg.union)
		}
	}
}

// syncAll 对当前 Watch 的每台设备下发 T+HHMM 对时帧（尽力而为，慢发防丢字节）。
func (d *Driver) syncAll(deviceIDs []string) {
	for _, deviceID := range deviceIDs {
		dev := d.device(deviceID)
		if dev == nil {
			continue
		}
		sctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		_, _ = dev.sendCommand(sctx, "sync-watch", actionSync, "")
		cancel()
	}
}

// emitObservations 把当前转储/V 帧快照转成 Observation 消息。
func (d *Driver) emitObservations(send func(deviceID string, union driver.DriverMessageUnion) error, deviceID string, dev *device) {
	if dev == nil {
		return
	}
	sensor, lastSensor, _ := dev.snapshot()
	full, lastFull, _ := dev.fullSnapshot()

	obs := func(entityID, capability, property string, value driver.Value, at time.Time) {
		_ = send(deviceID, &driver.Observation{
			EntityID: entityID, Capability: capability, Property: property, Value: value,
			ObservedAt: at.UTC().Format(time.RFC3339), Quality: "good",
		})
	}
	str := func(s string) driver.Value { return driver.Value{Kind: driver.ValueString, StringValue: s} }
	num := func(f float64) driver.Value { return driver.Value{Kind: driver.ValueNumber, NumberValue: f} }
	integer := func(i int) driver.Value { return driver.Value{Kind: driver.ValueInt, IntValue: int64(i)} }

	if full != nil {
		at := lastFull
		if at.IsZero() {
			at = time.Now()
		}
		obs("clock", capClock, "time", str(full.Clock), at)
		obs("temperature", capTemp, "value", num(TempC(full.Temp)), at)
		obs("illuminance", capIllum, "value", integer(full.Light), at)
		obs("navigation", capNav, "raw", integer(full.Nav), at)
		obs("navigation", capNav, "direction", integer(full.NavKey), at)
		obs("ext0", capAnalog, "raw", integer(full.Ext0), at)
		obs("ext1", capAnalog, "raw", integer(full.Ext1), at)
		obs("hall", capHall, "state", integer(full.Hall), at)
		obs("vibration", capVib, "state", integer(full.Vib), at)
		obs("key1", capKey, "state", integer(full.Key1), at)
		obs("key2", capKey, "state", integer(full.Key2), at)
		obs("key3", capKey, "state", integer(full.Key3), at)
		obs("buzzer", capBuzzer, "state", str(full.Beep), at)
		obs("led-bank", capLED, "mask", integer(full.LED), at)
		obs("display", capDisplay, "mode", str(full.Display), at)
		obs("motor", capMotor, "state", str(full.Motor), at)
		return
	}
	if sensor != nil {
		at := lastSensor
		if at.IsZero() {
			at = time.Now()
		}
		obs("clock", capClock, "time", str(fmt.Sprintf("%02d:%02d:%02d", sensor.Hour, sensor.Min, sensor.Sec)), at)
		obs("temperature", capTemp, "value", num(TempC(sensor.Rt)), at)
		obs("illuminance", capIllum, "value", integer(sensor.Rop), at)
		obs("hall", capHall, "state", integer(sensor.Hall), at)
		obs("vibration", capVib, "state", integer(sensor.Vib), at)
		obs("navigation", capNav, "raw", integer(sensor.Nav), at)
		obs("ext0", capAnalog, "raw", integer(sensor.Ext0), at)
		obs("ext1", capAnalog, "raw", integer(sensor.Ext1), at)
		obs("key1", capKey, "state", integer(sensor.Key), at)
	}
}

// emitDiagnostic publishes the latest board diagnostic snapshot.
func (d *Driver) emitDiagnostic(send func(deviceID string, union driver.DriverMessageUnion) error, deviceID string, dev *device) {
	fields, at := dev.diagnosticSnapshot()
	if len(fields) == 0 {
		return
	}
	_ = send(deviceID, &driver.Diagnostic{Level: "info", Message: "STC-B board diagnostic", Fields: fields, ObservedAt: at.UTC().Format(time.RFC3339)})
}

// Execute 执行一条命令，返回真实 ACK/ERROR 状态并推送 CommandProgress。
func (d *Driver) Execute(ctx context.Context, req *driver.ExecuteRequest) (*driver.ExecuteResponse, error) {
	d.mu.Lock()
	if d.shutdown {
		d.mu.Unlock()
		return nil, unavailable()
	}
	d.mu.Unlock()

	ok := false
	for _, a := range supportedActions {
		if a == req.Action {
			ok = true
			break
		}
	}
	if !ok {
		return &driver.ExecuteResponse{
			CommandID:      req.IdempotencyKey,
			IdempotencyKey: req.IdempotencyKey,
			Status:         status.Errorf(status.CodeInvalidArgument, "unknown action %q", req.Action),
			State:          driver.CommandStateFailed,
		}, nil
	}
	if req.IdempotencyKey == "" {
		return &driver.ExecuteResponse{
			Status: status.Errorf(status.CodeInvalidArgument, "idempotency_key is required"),
			State:  driver.CommandStateFailed,
		}, nil
	}

	d.devMu.Lock()
	deviceID := req.DeviceID
	if deviceID == "" && len(d.devices) == 1 {
		for id := range d.devices {
			deviceID = id
		}
	}
	dev := d.devices[deviceID]
	d.devMu.Unlock()
	if dev == nil {
		return &driver.ExecuteResponse{
			CommandID:      req.IdempotencyKey,
			IdempotencyKey: req.IdempotencyKey,
			Status:         status.Errorf(status.CodeFailedPrecondition, "device not open"),
			State:          driver.CommandStateFailed,
		}, nil
	}

	detail, err := dev.sendCommand(ctx, req.IdempotencyKey, req.Action, req.ArgsJSON)
	cmdID := "cmd-" + req.IdempotencyKey
	if err != nil {
		d.pushProgress(deviceID, &driver.CommandProgress{
			CommandID: cmdID, IdempotencyKey: req.IdempotencyKey, EntityID: req.EntityID,
			Action: req.Action, State: driver.CommandStateFailed, Detail: err.Error(),
		})
		return &driver.ExecuteResponse{
			CommandID:      cmdID,
			IdempotencyKey: req.IdempotencyKey,
			Status:         status.Errorf(status.CodeInternal, "%v", err),
			State:          driver.CommandStateFailed,
		}, nil
	}

	d.pushProgress(deviceID, &driver.CommandProgress{
		CommandID: cmdID, IdempotencyKey: req.IdempotencyKey, EntityID: req.EntityID,
		Action: req.Action, State: driver.CommandStateSucceeded, Progress: 1, Detail: detail,
	})
	acceptedFor := 15 * time.Second
	if req.Action == actionToneSequence {
		acceptedFor = 60 * time.Second
	}
	return &driver.ExecuteResponse{
		CommandID:        cmdID,
		IdempotencyKey:   req.IdempotencyKey,
		Status:           status.New(),
		State:            driver.CommandStateSucceeded,
		AcceptedDeadline: time.Now().Add(acceptedFor).UTC().Format(time.RFC3339),
	}, nil
}

// Health 返回服务状态。
func (d *Driver) Health(context.Context) (*driver.HealthResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return &driver.HealthResponse{State: driver.HealthStateNotServing}, nil
	}
	return &driver.HealthResponse{State: driver.HealthStateServing}, nil
}

// Shutdown 标记停止并关闭设备。
func (d *Driver) Shutdown(_ context.Context, _ *driver.ShutdownRequest) (*driver.ShutdownResponse, error) {
	d.mu.Lock()
	d.shutdown = true
	d.mu.Unlock()
	d.devMu.Lock()
	devices := d.devices
	d.devices = map[string]*device{}
	d.devMu.Unlock()
	for _, dev := range devices {
		_ = dev.Close()
	}
	return &driver.ShutdownResponse{Status: status.New()}, nil
}
