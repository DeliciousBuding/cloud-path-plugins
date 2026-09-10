// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/driver"
	"go.bug.st/serial"
)

type fakePort struct {
	mu     sync.Mutex
	writes [][]byte
	closed bool
}

func (p *fakePort) SetMode(*serial.Mode) error { return nil }
func (p *fakePort) Read([]byte) (int, error)   { return 0, nil }
func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, append([]byte(nil), b...))
	p.mu.Unlock()
	return len(b), nil
}
func (p *fakePort) Drain() error             { return nil }
func (p *fakePort) ResetInputBuffer() error  { return nil }
func (p *fakePort) ResetOutputBuffer() error { return nil }
func (p *fakePort) SetDTR(bool) error        { return nil }
func (p *fakePort) SetRTS(bool) error        { return nil }
func (p *fakePort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (p *fakePort) SetReadTimeout(time.Duration) error { return nil }
func (p *fakePort) Close() error                       { p.mu.Lock(); p.closed = true; p.mu.Unlock(); return nil }
func (p *fakePort) Break(time.Duration) error          { return nil }
func (p *fakePort) isClosed() bool                     { p.mu.Lock(); defer p.mu.Unlock(); return p.closed }
func (p *fakePort) joined() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, w := range p.writes {
		b.Write(w)
	}
	return b.String()
}

func newFakeV1Device(id string, p *fakePort) *device {
	return &device{cfg: deviceConfig{ID: id, Protocol: "v1"}, port: p, protocolV1: true, waiters: map[string]chan DeviceAck{}, done: make(chan struct{})}
}

// TestDeviceEventFlowToCapabilityScope 锁定设备事件扇入链（P5 软件层）：
// 固件 EVENT:<body> 行 → onEvent 回调携带 capability 命名空间的
// (entityID, eventType)。物理按键/磁铁无法自动化，这是事件全部软件路径
// 的可测边界；驱动不得重新引入药盒业务语义（hall=close 是硬件事实）。
func TestDeviceEventFlowToCapabilityScope(t *testing.T) {
	p := &fakePort{}
	d := newFakeV1Device("board-1", p)
	var mu sync.Mutex
	var got []string
	d.onEvent = func(entityID, eventType string) {
		mu.Lock()
		got = append(got, entityID+" "+eventType)
		mu.Unlock()
	}

	d.handleLine("EVENT:key1=press")
	d.handleLine("EVENT:key1=release")
	d.handleLine("EVENT:hall=close")
	d.handleLine("EVENT:hall=away")
	d.handleLine("EVENT:vib=quake")
	d.handleLine("EVENT:nav=2:press")
	d.handleLine("EVENT:key3:release")
	d.handleLine("EVENT:")     // 空体：不是事件
	d.handleLine("GARBAGE")    // 无关行：不产生事件
	d.handleLine("STATE:junk") // 非法 STATE：解析失败即丢弃

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"key1 " + capKey + "/press",
		"key1 " + capKey + "/release",
		"hall " + capHall + "/close",
		"hall " + capHall + "/away",
		"vibration " + capVib + "/quake",
		"navigation " + capNav + "/2:press",
		"key3 " + capKey + "/release",
	}
	if len(got) != len(want) {
		t.Fatalf("事件数 = %d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件[%d] = %q, want %q（全部: %v）", i, got[i], want[i], got)
		}
	}
}

// TestNormalizeProtocolEventUnknownPassthrough 锁定未知事件体的归属语义：
// 不认识的事件原样作为 eventType 交给上层（entity 为空），绝不静默吞掉
// （新增传感器事件形态时上层仍可观测），也绝不猜一个 capability 前缀。
func TestNormalizeProtocolEventUnknownPassthrough(t *testing.T) {
	entity, eventType := NormalizeProtocolEvent("future-sensor=triggered")
	if entity != "" || eventType != "future-sensor=triggered" {
		t.Fatalf("未知事件 = (%q, %q), want (\"\", 原样)", entity, eventType)
	}
}

func TestSendV1CommandWaitsForMatchingACK(t *testing.T) {
	p := &fakePort{}
	d := newFakeV1Device("board-1", p)
	result := make(chan error, 1)
	go func() {
		_, err := d.sendV1Command(context.Background(), "42-led", actionLED, `{"pattern":9}`)
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.joined(), "CMD:42-led:led:mask=FF") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d.handleLine("ACK:42-led:ok")
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ACK did not release command")
	}
}

func TestSendV1CommandReturnsDeviceError(t *testing.T) {
	p := &fakePort{}
	d := newFakeV1Device("board-1", p)
	result := make(chan error, 1)
	go func() {
		_, err := d.sendV1Command(context.Background(), "43-motor", actionMotor, `{"steps":2}`)
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.joined(), "CMD:43-motor:motor") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d.handleLine("ERR:43-motor:busy")
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ERR did not release command")
	}
}

func TestExecuteRoutesByDeviceID(t *testing.T) {
	d := New()
	p1, p2 := &fakePort{}, &fakePort{}
	d.devices[instanceDeviceKey("", "one")] = &device{cfg: deviceConfig{ID: "one"}, port: p1, waiters: map[string]chan DeviceAck{}, done: make(chan struct{})}
	d.devices[instanceDeviceKey("", "two")] = &device{cfg: deviceConfig{ID: "two"}, port: p2, waiters: map[string]chan DeviceAck{}, done: make(chan struct{})}
	resp, err := d.Execute(context.Background(), &driver.ExecuteRequest{DeviceID: "two", IdempotencyKey: "7", Action: actionLED, ArgsJSON: `{"pattern":9}`})
	if err != nil || resp.State != driver.CommandStateSucceeded {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if p1.joined() != "" {
		t.Fatalf("command leaked to device one: %q", p1.joined())
	}
	if got := p2.joined(); got != "L90" {
		t.Fatalf("device two got %q", got)
	}
}

func TestOpenTwoDevicesAndCloseOneIndependently(t *testing.T) {
	oldOpen := serialOpen
	defer func() { serialOpen = oldOpen }()
	ports := map[string]*fakePort{"COM3": {}, "COM4": {}, "COM5": {}}
	serialOpen = func(name string, _ *serial.Mode) (serial.Port, error) { return ports[name], nil }
	d := New()
	for _, tc := range []struct{ id, port string }{{"board-1", "COM3"}, {"board-2", "COM4"}, {"board-3", "COM5"}} {
		resp, err := d.OpenDevice(context.Background(), &driver.OpenDeviceRequest{DeviceID: tc.id, ConnectionHints: map[string]string{"port": tc.port, "baud": "115200"}})
		if err != nil || !resp.Status.IsOK() {
			t.Fatalf("open %s: resp=%+v err=%v", tc.id, resp, err)
		}
	}
	if len(d.devices) != 3 {
		t.Fatalf("devices=%d", len(d.devices))
	}
	_, _ = d.CloseDevice(context.Background(), &driver.CloseDeviceRequest{DeviceID: "board-1"})
	if !ports["COM3"].isClosed() {
		t.Fatal("board-1 port was not closed")
	}
	if ports["COM4"].isClosed() {
		t.Fatal("board-2 was affected by board-1 close")
	}
	if d.device("", "board-2") == nil || d.device("", "board-3") == nil {
		t.Fatal("unrelated board disappeared")
	}
}

func TestConcurrentExecuteThreeDevicesNoCrossTalk(t *testing.T) {
	d := New()
	ports := map[string]*fakePort{}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("board-%d", i)
		p := &fakePort{}
		ports[id] = p
		d.devices[instanceDeviceKey("", id)] = &device{cfg: deviceConfig{ID: id}, port: p, waiters: map[string]chan DeviceAck{}, done: make(chan struct{})}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 1; i <= 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("board-%d", i)
			resp, err := d.Execute(context.Background(), &driver.ExecuteRequest{DeviceID: id, IdempotencyKey: fmt.Sprintf("c%d", i), Action: actionLED, ArgsJSON: fmt.Sprintf(`{"pattern":%d}`, i)})
			if err != nil || resp.State != driver.CommandStateSucceeded {
				errs <- fmt.Errorf("%s resp=%+v err=%v", id, resp, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("board-%d", i)
		want := fmt.Sprintf("L%d0", i)
		if got := ports[id].joined(); got != want {
			t.Fatalf("%s got %q want %q", id, got, want)
		}
	}
}
