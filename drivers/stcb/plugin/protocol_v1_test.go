package plugin

import "testing"

func TestParseFullState(t *testing.T) {
	line := "STATE:seq=0001,clock=16:42:03,temp=1D6,light=038,nav=3FF,ext0=000,ext1=001,hall=1,vib=0,k1=1,k2=0,k3=0,navkey=0,motor=free,beep=busy,led=FF,display=clock,page=date"
	s, ok := ParseFullState(line)
	if !ok {
		t.Fatal("expected valid STATE")
	}
	if s.Clock != "16:42:03" || s.Temp != 0x1D6 || s.Light != 0x038 || s.Hall != 1 || s.Key1 != 1 || s.Motor != "free" || s.Beep != "busy" || s.LED != 255 || s.Display != "clock" || s.Page != "date" {
		t.Fatalf("state=%+v", s)
	}
}

func TestParseFullStateRejectsMalformed(t *testing.T) {
	if _, ok := ParseFullState("STATE:clock=99:99:99"); ok {
		t.Fatal("accepted invalid state")
	}
	if _, ok := ParseFullState("V:164203000"); ok {
		t.Fatal("accepted legacy frame")
	}
}

func TestParseDeviceAck(t *testing.T) {
	ack, ok := ParseDeviceAck("ACK:39-display:ok")
	if !ok || !ack.OK || ack.ID != "39-display" || ack.Detail != "ok" {
		t.Fatalf("ack=%+v ok=%v", ack, ok)
	}
	errAck, ok := ParseDeviceAck("ERR:40-motor:busy")
	if !ok || errAck.OK || errAck.Detail != "busy" {
		t.Fatalf("err=%+v ok=%v", errAck, ok)
	}
}

func TestEncodeV1Command(t *testing.T) {
	cases := []struct{ action, args, want string }{
		{actionBuzzer, `{"freq":4,"duration":3}`, "CMD:c1:beep:freq=1200,dur=18\r\n"},
		{actionLED, `{"pattern":9}`, "CMD:c1:led:mask=FF\r\n"},
		{actionDisplay, `{"digits":[1,2,3,4,5,6,7,8]}`, "CMD:c1:display:digits=12345678\r\n"},
		{actionDisplay, `{"mode":"clock"}`, "CMD:c1:display:mode=clock\r\n"},
		{actionDisplay, `{"mode":"date"}`, "CMD:c1:display:mode=date\r\n"},
		{actionDisplay, `{"mode":"sensors"}`, "CMD:c1:display:mode=sensors\r\n"},
		{actionMotor, `{"steps":2}`, "CMD:c1:motor:speed=100,steps=100\r\n"},
		{actionMotor, `{"steps":0}`, "CMD:c1:motorstop\r\n"},
		{actionSync, `{"hhmm":"1234"}`, "CMD:c1:sync:hhmm=1234\r\n"},
	}
	for _, tc := range cases {
		got, err := encodeV1Command("c1", tc.action, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s got %q want %q", tc.action, got, tc.want)
		}
	}
}
