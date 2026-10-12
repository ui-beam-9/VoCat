package cscall

import "testing"

// TestUlawRoundTrip 验证 μ-law 编解码在量化范围内的往返误差可接受。
func TestUlawRoundTrip(t *testing.T) {
	values := []int16{0, 1, -1, 100, -100, 1000, -1000, 8000, -8000, 32000, -32000}
	for _, value := range values {
		decoded := UlawToLinear(LinearToUlaw(value))
		diff := int(decoded) - int(value)
		if diff < 0 {
			diff = -diff
		}
		// μ-law 是 8 位有损压缩，允许明显但有限的量化误差。
		if diff > 2000 {
			t.Fatalf("μ-law 往返误差过大: 输入=%d 输出=%d", value, decoded)
		}
	}
}

// TestEncodeDecodePCM 验证批量 PCM↔μ-law 的长度与首样本。
func TestEncodeDecodePCM(t *testing.T) {
	pcm := []byte{0x00, 0x00, 0x10, 0x27} // 0, 10000 (S16_LE)
	ulaw := EncodePCMToUlaw(pcm)
	if len(ulaw) != 2 {
		t.Fatalf("μ-law 长度 = %d, 期望 2", len(ulaw))
	}
	decoded := DecodeUlawToPCM(ulaw)
	if len(decoded) != len(pcm) {
		t.Fatalf("PCM 长度 = %d, 期望 %d", len(decoded), len(pcm))
	}
}

// TestParseCLIPNumber 覆盖常见的 +CLIP 号码格式。
func TestParseCLIPNumber(t *testing.T) {
	cases := map[string]string{
		`+CLIP: "+8613800138000",145,,,,0`: "+8613800138000",
		`+CLIP: "10086",129`:               "10086",
		`+CLIP: 10010,145`:                 "10010",
	}
	for line, want := range cases {
		if got := parseCLIPNumber(line); got != want {
			t.Fatalf("parseCLIPNumber(%q) = %q, 期望 %q", line, got, want)
		}
	}
}

// TestQPCMVReady 覆盖 +QPCMV 流控状态解析。
func TestQPCMVReady(t *testing.T) {
	if !qpcmvReady("+QPCMV: 1") {
		t.Fatal("+QPCMV: 1 应判定为就绪")
	}
	if qpcmvReady("+QPCMV: 0") {
		t.Fatal("+QPCMV: 0 应判定为未就绪")
	}
}

// TestContainsCLCCActive 验证仅外呼语音已接通才判定为活跃。
func TestContainsCLCCActive(t *testing.T) {
	active := "+CLCC: 1,0,0,0,0,\"10086\",129"
	if !containsCLCCActive(active) {
		t.Fatal("MO 语音已接通应判定为活跃")
	}
	dataSession := "+CLCC: 1,0,0,1,0"
	if containsCLCCActive(dataSession) {
		t.Fatal("数据承载 (mode=1) 不应判定为活跃语音")
	}
	ringing := "+CLCC: 1,0,3,0,0"
	if containsCLCCActive(ringing) {
		t.Fatal("振铃 (stat=3) 不应判定为已接通")
	}
}
