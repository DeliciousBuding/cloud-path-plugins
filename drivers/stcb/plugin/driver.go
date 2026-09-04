package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/status"
)

// 稳定身份（一经发布即为机器契约，破坏性语义变化升 @2，不得原地改 @1）。
const (
	pluginID      = "io.github.deliciousbuding.cloud-path-driver-stcb"
	pluginVersion = "0.1.0"

	// driverID 是 Describe 上报的稳定 driver 标识，与 plugin.yaml contributes.drivers[0].id 一致。
	driverID = "stcb"
)

// STC-B 使用的 Capability 引用（cloudpath.dev 命名空间）。
const (
	capClock   = "cloudpath.dev/capability/clock@1"
	capAlarm   = "cloudpath.dev/capability/alarm@1"
	capContact = "cloudpath.dev/capability/contact@1"

	capTemp    = "cloudpath.dev/capability/temperature@1"
	capIllum   = "cloudpath.dev/capability/illuminance@1"
	capHall    = "cloudpath.dev/capability/hall@1"
	capVib     = "cloudpath.dev/capability/vibration@1"
	capKey     = "cloudpath.dev/capability/key@1"
	capBuzzer  = "cloudpath.dev/capability/buzzer@1"
	capLED     = "cloudpath.dev/capability/led@1"
	capDisplay = "cloudpath.dev/capability/display-text@1"
	capMotor   = "cloudpath.dev/capability/motor@1"
)

// entityDef 描述一台 STC-B 的静态 Entity。
type entityDef struct {
	ID   string
	Name string
	Cat  driver.EntityCategory
	Cap  string
}

// entities 是 STC-B 的全部 14 个 Entity（静态契约，与 in-process 参考适配器一致）。
var entities = []entityDef{
	{ID: "clock", Name: "时钟", Cat: driver.EntityCategorySensor, Cap: capClock},
	{ID: "alarm", Name: "提醒", Cat: driver.EntityCategorySensor, Cap: capAlarm},
	{ID: "compartment-1", Name: "分格 1", Cat: driver.EntityCategorySensor, Cap: capContact},
	{ID: "compartment-2", Name: "分格 2", Cat: driver.EntityCategorySensor, Cap: capContact},
	{ID: "compartment-3", Name: "分格 3", Cat: driver.EntityCategorySensor, Cap: capContact},
	{ID: "temperature", Name: "温度", Cat: driver.EntityCategorySensor, Cap: capTemp},
	{ID: "illuminance", Name: "光照", Cat: driver.EntityCategorySensor, Cap: capIllum},
	{ID: "hall", Name: "霍尔", Cat: driver.EntityCategorySensor, Cap: capHall},
	{ID: "vibration", Name: "振动", Cat: driver.EntityCategorySensor, Cap: capVib},
	{ID: "key", Name: "按键 K1", Cat: driver.EntityCategorySensor, Cap: capKey},
	{ID: "buzzer", Name: "蜂鸣器", Cat: driver.EntityCategoryActuator, Cap: capBuzzer},
	{ID: "led", Name: "LED", Cat: driver.EntityCategoryActuator, Cap: capLED},
	{ID: "display", Name: "数码管", Cat: driver.EntityCategoryActuator, Cap: capDisplay},
	{ID: "motor", Name: "电机", Cat: driver.EntityCategoryActuator, Cap: capMotor},
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

var ledActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"pattern": map[string]any{"type": "integer", "minimum": 0, "maximum": 9, "title": "LED 档（0=灭 9=全亮）"},
	},
	"required": []any{"pattern"},
}

var displayActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"digits": map[string]any{
			"type": "array", "items": map[string]any{"type": "integer", "minimum": 0, "maximum": 9},
			"minItems": 8, "maxItems": 8, "title": "8 位数字（0-9）",
		},
	},
	"required": []any{"digits"},
}

var motorActionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"steps": map[string]any{"type": "integer", "minimum": 0, "maximum": 4, "title": "步数档（0=停）"},
	},
	"required": []any{"steps"},
}

// capabilityDescriptors 返回 Describe 的 12 个 Capability 描述（与 plugin.yaml 冻结清单一致）。
func capabilityDescriptors() []driver.CapabilityDescriptor {
	return []driver.CapabilityDescriptor{
		{ID: capClock, Title: "Clock", Properties: []driver.PropertyDescriptor{{Name: "time", Type: "string", Access: "read"}}},
		{ID: capAlarm, Title: "Alarm", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "string", Access: "read"}}},
		{ID: capContact, Title: "Contact", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "string", Access: "read"}}},
		{ID: capTemp, Title: "Temperature", Properties: []driver.PropertyDescriptor{{Name: "value", Type: "number", Unit: "Cel", Access: "read", Quality: []string{"good"}}}},
		{ID: capIllum, Title: "Illuminance", Properties: []driver.PropertyDescriptor{{Name: "value", Type: "number", Access: "read", Quality: []string{"good"}}}},
		{ID: capHall, Title: "Hall", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "integer", Access: "read"}}},
		{ID: capVib, Title: "Vibration", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "integer", Access: "read"}}},
		{ID: capKey, Title: "Key", Properties: []driver.PropertyDescriptor{{Name: "state", Type: "integer", Access: "read"}}},
		{ID: capBuzzer, Title: "Buzzer", Actions: []driver.ActionDescriptor{{Name: actionBuzzer, InputSchemaJSON: mustJSON(buzzerActionSchema)}}},
		{ID: capLED, Title: "LED", Actions: []driver.ActionDescriptor{{Name: actionLED, InputSchemaJSON: mustJSON(ledActionSchema)}}},
		{ID: capDisplay, Title: "Display Text", Actions: []driver.ActionDescriptor{{Name: actionDisplay, InputSchemaJSON: mustJSON(displayActionSchema)}}},
		{ID: capMotor, Title: "Motor", Actions: []driver.ActionDescriptor{{Name: actionMotor, InputSchemaJSON: mustJSON(motorActionSchema)}}},
	}
}

// instanceConfig 是插件实例配置（来自 Server desired state 的 Config 字节）。
type instanceConfig struct {
	DeviceID      string `json:"device_id"`
	Name          string `json:"name"`
	Port          string `json:"port"`
	Baud          int    `json:"baud"`
	PollIntervalS int    `json:"poll_interval_s"`
}

func (c instanceConfig) pollInterval() time.Duration {
	if c.PollIntervalS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.PollIntervalS) * time.Second
}

var _ driver.DriverServer = (*Driver)(nil)

// Driver 是 STC-B 的 Driver Protocol v1 实现。一个进程管理一台设备。
type Driver struct {
	mu          sync.Mutex
	initialized bool
	shutdown    bool
	negotiated  uint32
	runtimeID   string
	instanceID  string

	cfg instanceConfig

	devMu sync.Mutex
	dev   *device

	// events / progress 把设备自发事件与命令进度转发给活跃的 Watch 流。
	events   chan string
	progress chan *driver.CommandProgress

	seqMu sync.Mutex
	seq   uint64
}

// New 返回一个新的 Driver。
func New() *Driver {
	return &Driver{
		events:   make(chan string, 64),
		progress: make(chan *driver.CommandProgress, 64),
	}
}

func (d *Driver) nextSeq() uint64 {
	d.seqMu.Lock()
	defer d.seqMu.Unlock()
	d.seq++
	return d.seq
}

func (d *Driver) pushEvent(t string) {
	select {
	case d.events <- t:
	default:
	}
}

func (d *Driver) pushProgress(p *driver.CommandProgress) {
	select {
	case d.progress <- p:
	default:
	}
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

// OpenDevice 打开串口并启动 RX 循环。
func (d *Driver) OpenDevice(ctx context.Context, req *driver.OpenDeviceRequest) (*driver.OpenDeviceResponse, error) {
	d.mu.Lock()
	if d.shutdown {
		d.mu.Unlock()
		return nil, unavailable()
	}
	cfg := d.cfg
	d.mu.Unlock()

	if cfg.Port == "" {
		return &driver.OpenDeviceResponse{
			PluginInstanceID: req.PluginInstanceID,
			DeviceID:         req.DeviceID,
			Status:           status.Errorf(status.CodeFailedPrecondition, "端口未配置（config.port 必填）"),
		}, nil
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = req.DeviceID
	}
	d.devMu.Lock()
	defer d.devMu.Unlock()
	if d.dev != nil {
		return &driver.OpenDeviceResponse{
			PluginInstanceID: req.PluginInstanceID,
			DeviceID:         req.DeviceID,
			Status:           status.New(),
		}, nil
	}
	dev, err := openDevice(ctx, deviceConfig{ID: cfg.DeviceID, Name: cfg.Name, Port: cfg.Port, Baud: cfg.Baud}, d.pushEvent)
	if err != nil {
		return &driver.OpenDeviceResponse{
			PluginInstanceID: req.PluginInstanceID,
			DeviceID:         req.DeviceID,
			Status:           status.Errorf(status.CodeUnavailable, "打开串口失败: %v", err),
		}, nil
	}
	d.dev = dev
	return &driver.OpenDeviceResponse{PluginInstanceID: req.PluginInstanceID, DeviceID: req.DeviceID, Status: status.New()}, nil
}

// CloseDevice 关闭串口。
func (d *Driver) CloseDevice(_ context.Context, req *driver.CloseDeviceRequest) (*driver.CloseDeviceResponse, error) {
	d.devMu.Lock()
	dev := d.dev
	d.dev = nil
	d.devMu.Unlock()
	if dev != nil {
		_ = dev.Close()
	}
	return &driver.CloseDeviceResponse{PluginInstanceID: req.PluginInstanceID, DeviceID: req.DeviceID, Status: status.New()}, nil
}

// deviceIDsFor 决定 Watch 应上报的设备集合（单设备驱动：总是配置中的那一台）。
func (d *Driver) deviceIDsFor(ids []string) []string {
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()
	if cfg.DeviceID == "" {
		return ids
	}
	return []string{cfg.DeviceID}
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
	cfg := d.cfg
	d.mu.Unlock()

	deviceID := cfg.DeviceID
	if deviceID == "" && len(req.DeviceIDs) > 0 {
		deviceID = req.DeviceIDs[0]
	}
	if deviceID == "" {
		return status.Errorf(status.CodeInvalidArgument, "device id 未配置")
	}

	send := func(union driver.DriverMessageUnion) error {
		return stream.Send(ctx, &driver.DriverMessage{
			PluginInstanceID: req.PluginInstanceID,
			Sequence:         d.nextSeq(),
			SchemaVersion:    driver.SchemaVersion,
			DeviceID:         deviceID,
			Union:            union,
		})
	}

	d.devMu.Lock()
	dev := d.dev
	d.devMu.Unlock()

	statusNow := driver.DeviceStatusUnavailable
	if dev != nil {
		statusNow = driver.DeviceStatusOnline
	}
	if err := send(&driver.DeviceUpsert{Device: driver.Device{
		DeviceID: deviceID, ExternalID: cfg.Port, Manufacturer: "STC-B",
		Model: "IAP15F2K61S2", Status: statusNow, DisplayName: cfg.Name,
	}}); err != nil {
		return err
	}
	for _, e := range entities {
		if err := send(&driver.EntityUpsert{Entity: driver.Entity{
			EntityID: e.ID, DeviceID: deviceID, UniqueKey: e.ID, Name: e.Name,
			Category: e.Cat, Capabilities: []string{e.Cap},
		}}); err != nil {
			return err
		}
	}

	poll := time.NewTicker(cfg.pollInterval())
	defer poll.Stop()
	reqFrame := func() {
		if dev == nil {
			return
		}
		_ = dev.write([]byte("S"))
		_ = dev.write([]byte("V"))
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			reqFrame()
			// 等一小拍让回帧落进 RX 缓冲，再按当前快照发观测。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			d.emitObservations(send, dev, deviceID)
		case t := <-d.events:
			_ = send(&driver.Event{DeviceID: deviceID, EventType: t, OccurredAt: time.Now().UTC().Format(time.RFC3339)})
		case p := <-d.progress:
			_ = send(p)
		}
	}
}

// emitObservations 把当前转储/V 帧快照转成 Observation 消息。
func (d *Driver) emitObservations(send func(driver.DriverMessageUnion) error, dev *device, deviceID string) {
	if dev == nil {
		return
	}
	dump, lastDump, sensor, lastSensor, _ := dev.snapshot()

	obs := func(entityID, capability, property string, value driver.Value, at time.Time) {
		_ = send(&driver.Observation{
			EntityID: entityID, Capability: capability, Property: property, Value: value,
			ObservedAt: at.UTC().Format(time.RFC3339), Quality: "good",
		})
	}
	str := func(s string) driver.Value { return driver.Value{Kind: driver.ValueString, StringValue: s} }
	num := func(f float64) driver.Value { return driver.Value{Kind: driver.ValueNumber, NumberValue: f} }
	integer := func(i int) driver.Value { return driver.Value{Kind: driver.ValueInt, IntValue: int64(i)} }

	if dump != nil {
		at := lastDump
		if at.IsZero() {
			at = time.Now()
		}
		obs("clock", capClock, "time", str(fmt.Sprintf("%02d:%02d", dump.Hour, dump.Min)), at)
		obs("alarm", capAlarm, "state", str(StateLabel(dump.State)), at)
		for i := range dump.Slots {
			obs(fmt.Sprintf("compartment-%d", i+1), capContact, "state", str(SlotLabel(dump.Slots[i])), at)
		}
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
		obs("key", capKey, "state", integer(sensor.Key), at)
	}
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
	dev := d.dev
	d.devMu.Unlock()
	if dev == nil {
		return &driver.ExecuteResponse{
			CommandID:      req.IdempotencyKey,
			IdempotencyKey: req.IdempotencyKey,
			Status:         status.Errorf(status.CodeFailedPrecondition, "device not open"),
			State:          driver.CommandStateFailed,
		}, nil
	}

	detail, err := dev.sendCommand(ctx, req.Action, req.ArgsJSON)
	cmdID := "cmd-" + req.IdempotencyKey
	if err != nil {
		d.pushProgress(&driver.CommandProgress{
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

	d.pushProgress(&driver.CommandProgress{
		CommandID: cmdID, IdempotencyKey: req.IdempotencyKey, EntityID: req.EntityID,
		Action: req.Action, State: driver.CommandStateSucceeded, Progress: 1, Detail: detail,
	})
	return &driver.ExecuteResponse{
		CommandID:        cmdID,
		IdempotencyKey:   req.IdempotencyKey,
		Status:           status.New(),
		State:            driver.CommandStateSucceeded,
		AcceptedDeadline: time.Now().Add(15 * time.Second).UTC().Format(time.RFC3339),
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
	dev := d.dev
	d.dev = nil
	d.devMu.Unlock()
	if dev != nil {
		_ = dev.Close()
	}
	return &driver.ShutdownResponse{Status: status.New()}, nil
}
