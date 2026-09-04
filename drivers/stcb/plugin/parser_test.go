package plugin

import "testing"

// legacy 探针 V 帧（bring-up 兼容）：只解析板载硬件事实；帧内业务状态位不参与语义。
func TestParseSensorGolden(t *testing.T) {
	s, ok := ParseSensor("V:0930552000000000000000010")
	if !ok {
		t.Fatal("expected valid sensor frame")
	}
	if s.Hour != 9 || s.Min != 30 || s.Sec != 55 {
		t.Fatalf("clock = %02d:%02d:%02d", s.Hour, s.Min, s.Sec)
	}
	if s.Hall != 0 || s.Vib != 1 || s.Key != 0 {
		t.Fatalf("hall/vib/key = %d/%d/%d", s.Hall, s.Vib, s.Key)
	}
}

func TestParseSensorRejectsOutOfRange(t *testing.T) {
	// 秒 99 越界（BCD 合法性校验）。
	if _, ok := ParseSensor("V:0930992000000000000000000"); ok {
		t.Fatal("expected reject sec 99")
	}
}

func TestParseSensorToleratesDamagedSeparator(t *testing.T) {
	// 响铃期损坏分隔符（':' 变 U+FFFD）仍应解析出硬件值。
	if _, ok := ParseSensor("V\uFFFD0930552000000000000000010"); !ok {
		t.Fatal("expected tolerate damaged separator")
	}
}

func TestTempCMidScale(t *testing.T) {
	// Rt=512 → 分压中点 → 10K → 25°C。
	got := TempC(512)
	if got < 24.9 || got > 25.1 {
		t.Fatalf("TempC(512) = %f, want ~25.0", got)
	}
}
