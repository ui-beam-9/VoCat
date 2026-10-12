package cscall

import "testing"

// TestBuildAndParseSDP 验证 SDP 生成后可被自身解析回相同的媒体地址。
func TestBuildAndParseSDP(t *testing.T) {
	body := buildSDP("192.168.1.50", 12345)
	info, err := parseSDP([]byte(body))
	if err != nil {
		t.Fatalf("parseSDP 失败: %v", err)
	}
	if info.ConnectionIP != "192.168.1.50" {
		t.Fatalf("连接地址 = %q, 期望 192.168.1.50", info.ConnectionIP)
	}
	if info.MediaPort != 12345 {
		t.Fatalf("媒体端口 = %d, 期望 12345", info.MediaPort)
	}
}

// TestParseSDPWithIPv6 验证 IPv6 连接行也能解析。
func TestParseSDPWithIPv6(t *testing.T) {
	body := "v=0\r\nc=IN IP6 2001:db8::1\r\nm=audio 5004 RTP/AVP 0\r\n"
	info, err := parseSDP([]byte(body))
	if err != nil {
		t.Fatalf("parseSDP 失败: %v", err)
	}
	if info.ConnectionIP != "2001:db8::1" || info.MediaPort != 5004 {
		t.Fatalf("解析结果 = %+v", info)
	}
}

// TestParseSDPWithoutAudio 验证缺少音频端口时报错。
func TestParseSDPWithoutAudio(t *testing.T) {
	if _, err := parseSDP([]byte("v=0\r\nc=IN IP4 10.0.0.1\r\n")); err == nil {
		t.Fatal("缺少 m=audio 时应返回错误")
	}
}
