// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudpath Authors

package plugin

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDeclaredInputContractRejectsBeforeEncoding(t *testing.T) {
	encoders := map[string]func(string, string) ([]byte, error){
		"legacy": encodeCommand,
		"v1":     func(action, args string) ([]byte, error) { return encodeV1Command("guard-1", action, args) },
	}
	cases := []struct{ name, action, args string }{
		{"buzzer-missing-freq", "buzzer", "{\"duration\":1}"},
		{"buzzer-missing-duration", "buzzer", "{\"freq\":1}"},
		{"buzzer-empty", "buzzer", "{}"},
		{"buzzer-null", "buzzer", "{\"freq\":null,\"duration\":1}"},
		{"buzzer-frequency-zero", "buzzer", "{\"freq\":0,\"duration\":1}"},
		{"buzzer-frequency-high", "buzzer", "{\"freq\":9,\"duration\":1}"},
		{"buzzer-duration-high", "buzzer", "{\"freq\":1,\"duration\":9}"},
		{"motor-empty", "motor", "{}"},
		{"root-null", "motor", "null"},
		{"root-array", "motor", "[]"},
		{"motor-null", "motor", "{\"steps\":null}"},
		{"led-empty", "led", "{}"},
		{"led-two-choices", "led", "{\"mask\":0,\"pattern\":9}"},
		{"led-null-and-pattern", "led", "{\"mask\":null,\"pattern\":1}"},
		{"display-mode-and-digits", "display", "{\"mode\":\"clock\",\"digits\":[1,2,3,4,5,6,7,8]}"},
		{"display-codes-and-digits", "display", "{\"codes\":[1,2,3,4,5,6,7,8],\"digits\":[1,2,3,4,5,6,7,8]}"},
		{"display-empty-alternative", "display", "{\"codes\":[],\"digits\":[1,2,3,4,5,6,7,8]}"},
	}
	for protocol, encode := range encoders {
		for _, tc := range cases {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				frame, err := encode(tc.action, tc.args)
				if err == nil || len(frame) != 0 {
					t.Fatalf("invalid declared input produced a frame: %q err=%v", frame, err)
				}
			})
		}
	}
}

func TestV1SyncRejectsMalformedWireValues(t *testing.T) {
	cases := []struct{ name, action, args string }{
		{"sync-time-line", "sync", "{\"time\":\"120000\\nCMD:unexpected:diag\"}"},
		{"sync-time-nul", "sync", "{\"time\":\"1200\\u000000\"}"},
		{"sync-hour", "sync", "{\"time\":\"250000\"}"},
		{"sync-seconds", "sync", "{\"time\":\"125960\"}"},
		{"sync-hhmm", "sync", "{\"hhmm\":\"2500\"}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := encodeV1Command("guard-1", tc.action, tc.args)
			if err == nil || len(frame) != 0 {
				t.Fatalf("unsafe sync produced a frame: %q err=%v", frame, err)
			}
		})
	}
	if frame, err := encodeV1Command("bad\x00id", actionSensor, ""); err == nil || len(frame) != 0 {
		t.Fatalf("NUL command id produced a frame: %q err=%v", frame, err)
	}
}

func TestLegacyRawRejectsDecodedControlBytesAndLimit(t *testing.T) {
	for _, args := range []string{"{\"args\":\"V\\nD\"}", "{\"args\":\"V\\rD\"}", "{\"args\":\"V\\u0000D\"}", "{\"args\":\"XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\"}"} {
		if frame, err := encodeCommand(actionRaw, args); err == nil || len(frame) != 0 {
			t.Fatalf("raw transport gate bypass: %q err=%v", frame, err)
		}
	}
}

func TestInvalidV1InputNeverWritesOrReservesWaiter(t *testing.T) {
	p := &fakePort{}
	d := newFakeV1Device("board-guard", p)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.sendV1Command(ctx, "guard-1", actionLED, `{"mask":0,"pattern":9}`); err == nil {
		t.Fatal("ambiguous input accepted")
	}
	if got := p.joined(); got != "" {
		t.Fatalf("invalid input reached UART: %q", got)
	}
	if len(d.waiters) != 0 {
		t.Fatalf("invalid input reserved ACK waiter: %+v", d.waiters)
	}
}

func TestExplicitZeroInputsRemainValid(t *testing.T) {
	for _, tc := range []struct{ action, args string }{
		{actionBuzzer, `{"freq":1,"duration":0}`}, {actionMotor, `{"steps":0}`}, {actionLED, `{"pattern":0}`},
	} {
		if _, err := encodeCommand(tc.action, tc.args); err != nil {
			t.Fatal(err)
		}
		if _, err := encodeV1Command("guard-1", tc.action, tc.args); err != nil {
			t.Fatal(err)
		}
	}
	if frame, err := encodeV1Command("guard-1", actionLED, `{"mask":0}`); err != nil || !strings.Contains(string(frame), "mask=00") {
		t.Fatalf("zero mask: %q %v", frame, err)
	}
	if frame, err := encodeCommand(actionLED, `{"mask":0}`); err == nil || len(frame) != 0 {
		t.Fatalf("legacy silently converted v1-only mask into a pattern: %q %v", frame, err)
	}
	if frame, err := encodeV1Command("guard-1", actionSync, `{"time":"123456","hhmm":"1234"}`); err != nil || !strings.Contains(string(frame), "time=123456") {
		t.Fatalf("compatible sync priority changed: %q %v", frame, err)
	}
}

func TestLegacyRawUsesDecodedUTF8ByteLimit(t *testing.T) {
	for _, value := range []string{strings.Repeat("X", 64), strings.Repeat("界", 21) + "X"} {
		frame, err := encodeCommand(actionRaw, `{"args":"`+value+`"}`)
		if err != nil || string(frame) != value || len(frame) != 64 {
			t.Fatalf("exact 64-byte raw value changed: %q err=%v", frame, err)
		}
	}
	if frame, err := encodeCommand(actionRaw, `{"args":"`+strings.Repeat("界", 22)+`"}`); err == nil || len(frame) != 0 {
		t.Fatalf("UTF-8 byte limit bypass: %q err=%v", frame, err)
	}
}
