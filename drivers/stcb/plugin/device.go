package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// 命令后等待真实回帧的窗口。固件回包 ~100ms，窗口留足余量但不拖长 ack。
const (
	dumpResultWindow   = 800 * time.Millisecond
	syncResultWindow   = 1200 * time.Millisecond
	frameSettleDelay   = 300 * time.Millisecond
	frameRequestDelay  = 250 * time.Millisecond
	frameWaitPollDelay = 20 * time.Millisecond
)

// deviceConfig 是打开一台 STC-B 设备的参数（来自插件实例配置）。
type deviceConfig struct {
	ID   string
	Name string
	Port string
	Baud int
}

// device 是一台已打开串口的 STC-B 设备：RX 循环持续解析转储/传感器/事件。
type device struct {
	cfg      deviceConfig
	portName string
	port     serial.Port
	onEvent  func(eventType string)

	mu         sync.Mutex
	dump       *Dump
	lastDump   time.Time
	sensor     *Sensor
	lastSensor time.Time
	dead       bool

	done     chan struct{}
	doneOnce sync.Once
}

// openDevice 打开串口并启动 RX 循环。拔线/端口错误通过 done 通知上层。
func openDevice(ctx context.Context, cfg deviceConfig, onEvent func(string)) (*device, error) {
	baud := cfg.Baud
	if baud <= 0 {
		baud = 9600
	}
	port, err := serial.Open(cfg.Port, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("stcb: open %s: %w", cfg.Port, err)
	}
	if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("stcb: set timeout: %w", err)
	}
	d := &device{
		cfg:      cfg,
		portName: cfg.Port,
		port:     port,
		onEvent:  onEvent,
		done:     make(chan struct{}),
	}
	go d.rxLoop(ctx)
	return d, nil
}

// rxLoop 按行读串口：解析转储更新状态、解析事件回调 onEvent。
func (d *device) rxLoop(ctx context.Context) {
	defer d.markDead()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.done:
			return
		default:
		}
		n, err := d.port.Read(tmp)
		if err != nil {
			return // 拔线/端口死 → Done
		}
		if n == 0 {
			continue
		}
		buf = append(buf, tmp[:n]...)
		for {
			i := indexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := string(buf[:i])
			buf = append(buf[:0], buf[i+1:]...)
			d.handleLine(strings.TrimSpace(strings.ReplaceAll(line, "\x00", "")))
		}
		if len(buf) > 200 { // 无 EOL 垃圾防洪
			buf = buf[:0]
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func (d *device) handleLine(line string) {
	if line == "" {
		return
	}
	if dump, ok := ParseDump(line); ok {
		d.mu.Lock()
		d.dump = &dump
		d.lastDump = time.Now()
		d.mu.Unlock()
		return
	}
	if sensor, ok := ParseSensor(line); ok {
		d.mu.Lock()
		d.sensor = &sensor
		d.lastSensor = time.Now()
		d.mu.Unlock()
		return
	}
	if ev, ok := ParseEvent(line); ok {
		if d.onEvent != nil {
			d.onEvent(ev)
		}
	}
}

func (d *device) markDead() {
	d.mu.Lock()
	d.dead = true
	d.mu.Unlock()
	d.doneOnce.Do(func() { close(d.done) })
}

func (d *device) Done() <-chan struct{} { return d.done }

func (d *device) Close() error {
	d.doneOnce.Do(func() { close(d.done) })
	return d.port.Close()
}

// snapshot 返回当前解析状态（并发安全）。
func (d *device) snapshot() (dump *Dump, lastDump time.Time, sensor *Sensor, lastSensor time.Time, dead bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dump, d.lastDump, d.sensor, d.lastSensor, d.dead
}

func (d *device) online() bool {
	_, lastDump, _, lastSensor, dead := d.snapshot()
	if dead {
		return false
	}
	last := lastDump
	if lastSensor.After(last) {
		last = lastSensor
	}
	return !last.IsZero() && time.Since(last) < 30*time.Second
}

// write 单次写入命令字节。端口已死/写入失败时标记死亡。
func (d *device) write(b []byte) error {
	d.mu.Lock()
	dead := d.dead
	d.mu.Unlock()
	if dead {
		return fmt.Errorf("stcb: %s port dead", d.cfg.ID)
	}
	if _, err := d.port.Write(b); err != nil {
		go d.markDead()
		return fmt.Errorf("stcb: write: %w", err)
	}
	return nil
}

// writeSlow 逐字节慢发：固件 UART 命令缓冲仅 1 字节，快发会丢。
func (d *device) writeSlow(ctx context.Context, b []byte) error {
	for _, ch := range b {
		if err := d.write([]byte{ch}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.done:
			return fmt.Errorf("stcb: port dead during command")
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

// sendCommand 执行一条 action 并返回**真实执行结果**的一行非敏感摘要。
// 执行器命令（B/L/N/M）固件 1S 拍执行、无独立回帧，返回下发事实摘要；
// 传感器/转储/对时命令主动等待真实回帧，未回帧诚实报告（绝不伪造成功）。
func (d *device) sendCommand(ctx context.Context, action, argsJSON string) (string, error) {
	switch action {
	case actionBuzzer, actionLED, actionDisplay, actionMotor:
		frame, err := encodeCommand(action, argsJSON)
		if err != nil {
			return "", err
		}
		if err := d.writeSlow(ctx, frame); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s(%s) 已下发，args=%s（固件 1S 拍执行，无独立回帧）", action, wireByte(action), argsJSON), nil

	case actionSensor:
		_, _, _, before, _ := d.snapshot()
		if err := d.write([]byte("V")); err != nil {
			return "", err
		}
		if d.waitSensorAfter(before, dumpResultWindow, ctx) {
			return d.sensorSummary(action), nil
		}
		return fmt.Sprintf("%s(V) 已下发，%s内未收到回帧", action, dumpResultWindow.String()), nil
	}

	_, before, _, _, _ := d.snapshot()
	switch action {
	case actionDump:
		if err := d.write([]byte("S")); err != nil {
			return "", err
		}
		if d.waitDumpAfter(before, dumpResultWindow, ctx) {
			return d.frameSummary(action), nil
		}
		return fmt.Sprintf("%s 已下发，%s内未收到回帧", action, dumpResultWindow.String()), nil

	case actionSync:
		hhmm, err := encodeSync(argsJSON)
		if err != nil {
			return "", err
		}
		if err := d.writeSlow(ctx, hhmm); err != nil {
			return "", err
		}
		// 对时的真实结果只能由新回帧证明：主动取一帧，收到新帧才报同步后板钟与漂移。
		if d.requestFrame(ctx, before, frameSettleDelay, syncResultWindow) {
			if s, ok := d.syncSummary(); ok {
				return "synced " + s, nil
			}
		}
		raw := strings.TrimPrefix(string(hhmm), "T")
		return fmt.Sprintf("synced hhmm=%s（%s内未收到回帧）", raw, syncResultWindow.String()), nil

	case actionTrigger, actionOpen:
		frame, err := encodeCommand(action, argsJSON)
		if err != nil {
			return "", err
		}
		if err := d.write(frame); err != nil {
			return "", err
		}
		d.requestFrame(ctx, before, frameRequestDelay, dumpResultWindow)
		if d.hasFreshDump(before) {
			return d.frameSummary(action + "(" + wireByte(action) + ")"), nil
		}
		return fmt.Sprintf("%s(%s) 已下发，%s内未收到回帧", action, wireByte(action), dumpResultWindow.String()), nil

	case actionISP:
		if err := d.write([]byte("D")); err != nil {
			return "", err
		}
		return "isp(D) 已进入 ISP 下载模式，设备停止回应，需重新上电", nil

	case actionRaw:
		frame, err := encodeCommand(action, argsJSON)
		if err != nil {
			return "", err
		}
		if err := d.write(frame); err != nil {
			return "", err
		}
		d.requestFrame(ctx, before, frameRequestDelay, dumpResultWindow)
		if d.hasFreshDump(before) {
			return d.frameSummary("raw(" + argsJSON + ")"), nil
		}
		return fmt.Sprintf("raw(%s) 已下发，%s内未收到回帧", argsJSON, dumpResultWindow.String()), nil

	default:
		return "", fmt.Errorf("stcb: 不支持的命令 %q", action)
	}
}

// ---- 回帧等待与结果摘要（与 in-process 参考适配器同语义）----

func (d *device) hasFreshDump(before time.Time) bool {
	_, last, _, _, _ := d.snapshot()
	return !last.IsZero() && last.After(before)
}

func (d *device) waitDumpAfter(before time.Time, window time.Duration, ctx context.Context) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-d.done:
			return false
		default:
		}
		if d.hasFreshDump(before) {
			return true
		}
		time.Sleep(frameWaitPollDelay)
	}
	return d.hasFreshDump(before)
}

func (d *device) requestFrame(ctx context.Context, before time.Time, settle, window time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-d.done:
		return false
	case <-time.After(settle):
	}
	if err := d.write([]byte("S")); err != nil {
		return false
	}
	return d.waitDumpAfter(before, window, ctx)
}

func (d *device) frameSummary(prefix string) string {
	dump, _, _, _, _ := d.snapshot()
	if dump == nil {
		return prefix + " 已下发（暂无转储帧）"
	}
	slots := make([]string, 0, len(dump.Slots))
	for _, s := range dump.Slots {
		slots = append(slots, SlotLabel(s))
	}
	return fmt.Sprintf("%s dump_raw=%s clock=%02d:%02d state=%s slots=%s drift_min=%g",
		prefix, dump.Raw, dump.Hour, dump.Min, StateLabel(dump.State),
		strings.Join(slots, "/"), DriftMin(dump.Hour, dump.Min, BeijingNow()))
}

func (d *device) syncSummary() (string, bool) {
	dump, _, _, _, _ := d.snapshot()
	if dump == nil {
		return "", false
	}
	return fmt.Sprintf("clock=%02d:%02d drift_min=%g state=%s",
		dump.Hour, dump.Min, DriftMin(dump.Hour, dump.Min, BeijingNow()), StateLabel(dump.State)), true
}

func (d *device) hasFreshSensor(before time.Time) bool {
	_, _, _, last, _ := d.snapshot()
	return !last.IsZero() && last.After(before)
}

func (d *device) waitSensorAfter(before time.Time, window time.Duration, ctx context.Context) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-d.done:
			return false
		default:
		}
		if d.hasFreshSensor(before) {
			return true
		}
		time.Sleep(frameWaitPollDelay)
	}
	return d.hasFreshSensor(before)
}

func (d *device) sensorSummary(prefix string) string {
	_, _, s, _, _ := d.snapshot()
	if s == nil {
		return prefix + " 已下发（暂无传感器帧）"
	}
	return fmt.Sprintf("%s sensor_raw=%s clock=%02d:%02d:%02d state=%s temp_c=%g rop=%d hall=%d vib=%d key=%d",
		prefix, s.Raw, s.Hour, s.Min, s.Sec, StateLabel(s.State), TempC(s.Rt), s.Rop, s.Hall, s.Vib, s.Key)
}
