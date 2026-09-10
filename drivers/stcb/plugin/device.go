// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

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
var serialOpen = serial.Open

const (
	legacyFrameWindow  = 800 * time.Millisecond
	legacySyncWindow   = 1200 * time.Millisecond
	frameWaitPollDelay = 20 * time.Millisecond
	writeByteDelay     = 20 * time.Millisecond
)

// deviceConfig 是打开一台 STC-B 设备的参数（来自插件实例配置）。
type deviceConfig struct {
	ID       string
	Name     string
	Port     string
	Baud     int
	Protocol string
}

// device 是一台已打开串口的 STC-B 设备：RX 循环持续解析 Protocol v1 状态/事件/诊断，
// 以及 legacy 探针固件的 V 帧（bring-up 兼容）。
type device struct {
	instanceID string
	cfg        deviceConfig
	portName   string
	port       serial.Port
	onEvent    func(entityID, eventType string)

	mu         sync.Mutex
	sensor     *Sensor
	lastSensor time.Time
	full       *FullState
	lastFull   time.Time
	diagnostic map[string]string
	lastDiag   time.Time
	protocolV1 bool
	dead       bool

	cmdMu   sync.Mutex
	waiters map[string]chan DeviceAck

	done     chan struct{}
	doneOnce sync.Once
}

// openDevice 打开串口并启动 RX 循环。拔线/端口错误通过 done 通知上层。
func openDevice(ctx context.Context, cfg deviceConfig, onEvent func(entityID, eventType string)) (*device, error) {
	baud := effectiveBaud(cfg.Baud) // STC-B Full Firmware v1 / SDK 示例的当前串口契约
	port, err := serialOpen(cfg.Port, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("stcb: open %s: %w", cfg.Port, err)
	}
	if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("stcb: set timeout: %w", err)
	}
	d := &device{
		cfg:        cfg,
		portName:   cfg.Port,
		port:       port,
		onEvent:    onEvent,
		protocolV1: effectiveProtocol(cfg.Protocol) == "v1",
		waiters:    map[string]chan DeviceAck{},
		done:       make(chan struct{}),
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
	if strings.HasPrefix(line, "HELLO:stcb-full:v1") {
		d.mu.Lock()
		d.protocolV1 = true
		d.mu.Unlock()
		return
	}
	if full, ok := ParseFullState(line); ok {
		d.mu.Lock()
		d.full = &full
		d.lastFull = time.Now()
		d.protocolV1 = true
		d.mu.Unlock()
		return
	}
	if ack, ok := ParseDeviceAck(line); ok {
		d.mu.Lock()
		ch := d.waiters[ack.ID]
		d.mu.Unlock()
		if ch != nil {
			select {
			case ch <- ack:
			default:
			}
		}
		return
	}
	if ev, ok := ParseProtocolEvent(line); ok {
		if d.onEvent != nil {
			entityID, eventType := NormalizeProtocolEvent(ev)
			d.onEvent(entityID, eventType)
		}
		return
	}
	if diag, ok := ParseDiagnostic(line); ok {
		d.mu.Lock()
		d.diagnostic = diag
		d.lastDiag = time.Now()
		d.mu.Unlock()
		return
	}
	if sensor, ok := ParseSensor(line); ok {
		d.mu.Lock()
		d.sensor = &sensor
		d.lastSensor = time.Now()
		d.mu.Unlock()
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
func (d *device) snapshot() (sensor *Sensor, lastSensor time.Time, dead bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sensor, d.lastSensor, d.dead
}

func (d *device) fullSnapshot() (full *FullState, at time.Time, v1 bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.full, d.lastFull, d.protocolV1
}

func (d *device) diagnosticSnapshot() (map[string]string, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.diagnostic))
	for k, v := range d.diagnostic {
		out[k] = v
	}
	return out, d.lastDiag
}

func (d *device) isProtocolV1() bool {
	_, _, v1 := d.fullSnapshot()
	return v1
}

func (d *device) online() bool {
	_, lastSensor, dead := d.snapshot()
	if dead {
		return false
	}
	last := lastSensor
	_, lastFull, _ := d.fullSnapshot()
	if lastFull.After(last) {
		last = lastFull
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

// writeSlow 逐字节节流：固件 UART 命令缓冲仅 1 字节。2026-09-09 真板压测
// 5ms/byte 出现 badarg（丢字节），20ms/byte 100/100 通过；兼顾 4s ACK 窗口。
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
		case <-time.After(writeByteDelay):
		}
	}
	return nil
}

// sendV1Command sends one correlated Device Protocol v1 command and waits for
// the physical firmware ACK/ERR. Commands are serialized because the 8051
// firmware has a single pending-ACK slot.
func (d *device) sendV1Command(ctx context.Context, id, action, argsJSON string) (string, error) {
	d.cmdMu.Lock()
	defer d.cmdMu.Unlock()
	return d.sendV1CommandLocked(ctx, id, action, argsJSON, defaultV1ACKTimeout)
}

func (d *device) sendV1CommandLocked(ctx context.Context, id, action, argsJSON string, timeout time.Duration) (string, error) {
	wireID := id
	if len(wireID) > 11 {
		wireID = wireID[:11]
	}
	frame, err := encodeV1Command(wireID, action, argsJSON)
	if err != nil {
		return "", err
	}
	ch := make(chan DeviceAck, 1)
	d.mu.Lock()
	d.waiters[wireID] = ch
	d.mu.Unlock()
	defer func() { d.mu.Lock(); delete(d.waiters, wireID); d.mu.Unlock() }()
	if err := d.writeSlow(ctx, frame); err != nil {
		return "", err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-d.done:
		return "", fmt.Errorf("stcb: port dead during command")
	case <-timer.C:
		return "", fmt.Errorf("stcb: device ACK timeout id=%s", wireID)
	case ack := <-ch:
		if !ack.OK {
			return "", fmt.Errorf("stcb: device ERR id=%s code=%s", ack.ID, ack.Detail)
		}
		return fmt.Sprintf("device ACK id=%s detail=%s", ack.ID, ack.Detail), nil
	}
}

// sendCommand 执行一条 action，并返回**真实执行结果**的一行非敏感摘要。
// Protocol v1 固件：等待相同 id 的板端 ACK/ERR，这是唯一成功判据。
// legacy 探针固件（V/B/L/N/T）：没有关联 ACK，只能诚实报告下发与回帧事实。
func (d *device) sendCommand(ctx context.Context, id, action, argsJSON string) (string, error) {
	if d.isProtocolV1() {
		if action == actionToneSequence {
			return d.sendV1ToneSequence(ctx, id, argsJSON)
		}
		return d.sendV1Command(ctx, id, action, argsJSON)
	}
	switch action {
	case actionBuzzer, actionLED, actionDisplay, actionMotor:
		frame, err := encodeCommand(action, argsJSON)
		if err != nil {
			return "", err
		}
		if err := d.writeSlow(ctx, frame); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s(%s) 已下发，args=%s（legacy 固件无关联 ACK）", action, wireByte(action), argsJSON), nil

	case actionSensor:
		_, before, _ := d.snapshot()
		if err := d.write([]byte("V")); err != nil {
			return "", err
		}
		if d.waitSensorAfter(before, legacyFrameWindow, ctx) {
			return d.sensorSummary(action), nil
		}
		return fmt.Sprintf("%s(V) 已下发，%s内未收到回帧", action, legacyFrameWindow.String()), nil

	case actionSync:
		frame, err := encodeSync(argsJSON)
		if err != nil {
			return "", err
		}
		_, before, _ := d.snapshot()
		if err := d.writeSlow(ctx, frame); err != nil {
			return "", err
		}
		// 对时的真实结果只能由新回帧证明；没有新帧就诚实说没有。
		if d.waitSensorAfter(before, legacySyncWindow, ctx) {
			if s, _, _ := d.snapshot(); s != nil {
				return fmt.Sprintf("synced clock=%02d:%02d:%02d（回帧确认）", s.Hour, s.Min, s.Sec), nil
			}
		}
		return fmt.Sprintf("sync %s 已下发，%s内未收到回帧",
			strings.TrimPrefix(string(frame), "T"), legacySyncWindow.String()), nil

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
		_, before, _ := d.snapshot()
		if err := d.write(frame); err != nil {
			return "", err
		}
		if d.waitSensorAfter(before, legacyFrameWindow, ctx) {
			return d.sensorSummary("raw(" + argsJSON + ")"), nil
		}
		return fmt.Sprintf("raw(%s) 已下发，%s内未收到回帧", argsJSON, legacyFrameWindow.String()), nil

	default:
		return "", fmt.Errorf("stcb: legacy 固件不支持命令 %q（diag 等需要 Protocol v1 固件）", action)
	}
}

// ---- legacy 回帧等待与摘要（无回帧绝不冒充成功）----

func (d *device) hasFreshSensor(before time.Time) bool {
	_, last, _ := d.snapshot()
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
	s, _, _ := d.snapshot()
	if s == nil {
		return prefix + " 已下发（暂无传感器帧）"
	}
	return fmt.Sprintf("%s sensor_raw=%s clock=%02d:%02d:%02d temp_c=%g light=%d hall=%d vib=%d key=%d",
		prefix, s.Raw, s.Hour, s.Min, s.Sec, TempC(s.Rt), s.Rop, s.Hall, s.Vib, s.Key)
}
