package imscall

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// channelMedia 是可编程的 CallMedia 替身：ReadPCM 从 channel 取数据，WritePCM 记录写入。
type channelMedia struct {
	readCh  chan []int16
	mu      sync.Mutex
	written []int16
}

func newChannelMedia() *channelMedia {
	return &channelMedia{readCh: make(chan []int16, 64)}
}

func (m *channelMedia) Codec() string { return "PCMA" }

func (m *channelMedia) ReadPCM(ctx context.Context) ([]int16, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case chunk := <-m.readCh:
		return chunk, nil
	}
}

func (m *channelMedia) WritePCM(samples []int16) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.written = append(m.written, samples...)
	return nil
}

func (m *channelMedia) writtenSamples() []int16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int16(nil), m.written...)
}

// openClientSocket 打开一个模拟软电话的 UDP 端口。
func openClientSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("创建客户端 UDP 失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func bridgeClientAddr(t *testing.T, conn *net.UDPConn) (string, int) {
	t.Helper()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String(), addr.Port
}

// readRTPPacket 带超时读取一个 RTP 包。
func readRTPPacket(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("读取 RTP 失败: %v", err)
	}
	return buffer[:n]
}

// TestBridgeDownlink 验证 IMS PCM → μ-law → RTP 的下行通路。
func TestBridgeDownlink(t *testing.T) {
	media := newChannelMedia()
	bridge, err := NewMediaBridge("dev-1", nil)
	if err != nil {
		t.Fatalf("创建媒体桥失败: %v", err)
	}
	defer bridge.Stop()

	client := openClientSocket(t)
	ip, port := bridgeClientAddr(t, client)
	bridge.SetClientAddr(ip, port)
	bridge.AttachMedia(media)
	bridge.Start(context.Background())

	// 喂入一帧 160 采样。
	samples := make([]int16, rtpFrameSamples)
	for i := range samples {
		samples[i] = int16(i * 100)
	}
	media.readCh <- samples

	packet := readRTPPacket(t, client)
	if len(packet) != 12+rtpFrameSamples {
		t.Fatalf("RTP 包长度 = %d，期望 %d", len(packet), 12+rtpFrameSamples)
	}
	if packet[0] != 0x80 || packet[1]&0x7F != 0 {
		t.Fatalf("RTP 头不符合 PCMU 预期: %02x %02x", packet[0], packet[1])
	}
	// 解码后与源样本比较（允许 μ-law 量化误差）。
	for i := 0; i < rtpFrameSamples; i++ {
		decoded := int(decodeUlawToSamples(packet[12+i : 12+i+1])[0])
		diff := decoded - int(samples[i])
		if diff < 0 {
			diff = -diff
		}
		if diff > 2000 {
			t.Fatalf("样本 %d 量化误差过大: 源=%d 解码=%d", i, samples[i], decoded)
		}
	}
}

// TestBridgeUplink 验证软电话 RTP → μ-law 解码 → IMS PCM 的上行通路。
func TestBridgeUplink(t *testing.T) {
	media := newChannelMedia()
	bridge, err := NewMediaBridge("dev-1", nil)
	if err != nil {
		t.Fatalf("创建媒体桥失败: %v", err)
	}
	defer bridge.Stop()

	client := openClientSocket(t)
	bridge.AttachMedia(media)
	bridge.Start(context.Background())

	// 构造并发送一个 PCMU RTP 包（160 采样）。
	samples := make([]int16, rtpFrameSamples)
	for i := range samples {
		samples[i] = int16(1000 - i)
	}
	payload := encodeSamplesToUlaw(samples)
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80
	packet[1] = 0x00
	binary.BigEndian.PutUint16(packet[2:4], 1)
	binary.BigEndian.PutUint32(packet[8:12], 0xAABBCCDD)
	copy(packet[12:], payload)
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bridge.LocalPort()}
	if _, err := client.WriteToUDP(packet, target); err != nil {
		t.Fatalf("发送 RTP 失败: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(media.writtenSamples()) >= rtpFrameSamples {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	written := media.writtenSamples()
	if len(written) < rtpFrameSamples {
		t.Fatalf("上行写入样本数 = %d，期望至少 %d", len(written), rtpFrameSamples)
	}
	// 与源样本比较（允许 μ-law 量化误差）。
	for i := 0; i < rtpFrameSamples; i++ {
		diff := int(written[i]) - int(samples[i])
		if diff < 0 {
			diff = -diff
		}
		if diff > 2000 {
			t.Fatalf("样本 %d 量化误差过大: 源=%d 写入=%d", i, samples[i], written[i])
		}
	}
}

// TestBridgeSilenceWithoutMedia 未挂接媒体时发送静音包保活。
func TestBridgeSilenceWithoutMedia(t *testing.T) {
	bridge, err := NewMediaBridge("dev-1", nil)
	if err != nil {
		t.Fatalf("创建媒体桥失败: %v", err)
	}
	defer bridge.Stop()

	client := openClientSocket(t)
	ip, port := bridgeClientAddr(t, client)
	bridge.SetClientAddr(ip, port)
	bridge.Start(context.Background())

	packet := readRTPPacket(t, client)
	if len(packet) != 12+rtpFrameSamples {
		t.Fatalf("静音包长度 = %d，期望 %d", len(packet), 12+rtpFrameSamples)
	}
	// μ-law 静音编码为 0xFF。
	for i := 12; i < len(packet); i++ {
		if packet[i] != 0xFF {
			t.Fatalf("静音包 payload[%d] = %02x，期望 0xFF", i-12, packet[i])
		}
	}
}

// TestUlawSampleRoundTrip 样本级 μ-law 编解码往返。
func TestUlawSampleRoundTrip(t *testing.T) {
	values := []int16{0, 1, -1, 100, -100, 1000, -1000, 8000, -8000, 32000, -32000}
	ulaw := encodeSamplesToUlaw(values)
	if len(ulaw) != len(values) {
		t.Fatalf("μ-law 长度 = %d，期望 %d", len(ulaw), len(values))
	}
	decoded := decodeUlawToSamples(ulaw)
	for i, value := range values {
		diff := int(decoded[i]) - int(value)
		if diff < 0 {
			diff = -diff
		}
		if diff > 2000 {
			t.Fatalf("样本 %d 往返误差过大: 输入=%d 输出=%d", i, value, decoded[i])
		}
	}
}
