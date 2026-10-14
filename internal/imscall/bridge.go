// Package imscall 提供语音网关的 IMS（VoWiFi）桥接后端。
//
// 软电话（Linphone 等）注册到 sipgw 后，其 SIP 信令由本包的 Manager 以
// B2BUA（背靠背用户代理）方式转发到设备的 VoWiFi/IMS 会话；媒体侧把
// vowifi.CallMedia 暴露的 8kHz 单声道 PCM 与软电话的 RTP（PCMU）双向桥接。
package imscall

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/cscall"
	"vocat/internal/vowifi"
)

// rtpFrameSamples 是一帧 20ms RTP 对应的 PCM 采样数（8kHz）。
const rtpFrameSamples = 160

// MediaBridge 是一路 IMS 通话的 PCM ↔ RTP 媒体桥。
//
// 下行：CallMedia.ReadPCM → G.711μ 编码 → RTP(PT=0, 20ms) → 软电话；
// 上行：软电话 RTP → G.711μ 解码 → CallMedia.WritePCM。
// 媒体尚未挂接或无数据可读时发送静音包保活，行为与 cscall.AudioBridge 对齐。
type MediaBridge struct {
	deviceID string
	logger   *slog.Logger
	rtpConn  *net.UDPConn

	clientAddr atomic.Pointer[net.UDPAddr]

	mu    sync.Mutex
	media vowifi.CallMedia

	seqNum    uint16
	timestamp uint32
	ssrc      uint32

	started  atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewMediaBridge 创建媒体桥并绑定一个随机 UDP 端口用于 RTP。
func NewMediaBridge(deviceID string, logger *slog.Logger) (*MediaBridge, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("绑定 RTP 端口失败: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &MediaBridge{
		deviceID: deviceID,
		logger:   logger,
		rtpConn:  conn,
		ssrc:     0x23456789,
		stop:     make(chan struct{}),
	}, nil
}

// LocalPort 返回 RTP 本地监听端口（写入 SDP）。
func (bridge *MediaBridge) LocalPort() int {
	return bridge.rtpConn.LocalAddr().(*net.UDPAddr).Port
}

// SetClientAddr 设置软电话的 RTP 地址（来自 SDP 协商）。
func (bridge *MediaBridge) SetClientAddr(ip string, port int) {
	addr := &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
	bridge.clientAddr.Store(addr)
	bridge.logger.Info("MediaBridge 设置 RTP 客户端地址",
		"device_id", bridge.deviceID, "ip", ip, "port", port)
}

// AttachMedia 挂接 IMS 通话媒体（通话进入 early_media/active 后调用）。
func (bridge *MediaBridge) AttachMedia(media vowifi.CallMedia) {
	bridge.mu.Lock()
	bridge.media = media
	bridge.mu.Unlock()
	bridge.logger.Info("MediaBridge 已挂接 IMS 通话媒体",
		"device_id", bridge.deviceID, "codec", media.Codec())
}

func (bridge *MediaBridge) currentMedia() vowifi.CallMedia {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.media
}

// Start 启动双向桥接，幂等。
func (bridge *MediaBridge) Start(ctx context.Context) {
	if !bridge.started.CompareAndSwap(false, true) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	bridge.wg.Add(2)
	go func() {
		<-bridge.stop
		cancel()
	}()
	go bridge.loopDownlink(loopCtx)
	go bridge.loopUplink()
	bridge.logger.Info("MediaBridge 已启动",
		"device_id", bridge.deviceID, "rtp_port", bridge.LocalPort())
}

// Stop 停止桥接并释放资源。
func (bridge *MediaBridge) Stop() {
	bridge.stopOnce.Do(func() {
		close(bridge.stop)
		_ = bridge.rtpConn.Close()
		go func() {
			waitCh := make(chan struct{})
			go func() {
				bridge.wg.Wait()
				close(waitCh)
			}()
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
				bridge.logger.Warn("MediaBridge 停止超时，强制放弃等待", "device_id", bridge.deviceID)
			}
		}()
	})
}

// loopDownlink 下行：IMS PCM → G.711μ → RTP → 软电话。
func (bridge *MediaBridge) loopDownlink(ctx context.Context) {
	defer bridge.wg.Done()
	silence := make([]int16, rtpFrameSamples)
	pending := make([]int16, 0, rtpFrameSamples*2)
	nextSend := time.Now()
	for {
		select {
		case <-bridge.stop:
			return
		default:
		}
		// 凑满一帧 160 采样。
		for len(pending) < rtpFrameSamples {
			media := bridge.currentMedia()
			if media == nil {
				// 媒体未挂接：发静音保持 RTP 流，避免软电话超时。
				pending = append(pending, silence...)
				break
			}
			readCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
			chunk, err := media.ReadPCM(readCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					// 读取失败（通话可能正在结束），短暂等待避免空转。
					if !bridge.sleep(20 * time.Millisecond) {
						return
					}
				}
				// 超时或失败都补静音帧，保持 RTP 连续。
				pending = append(pending, silence...)
				break
			}
			if len(chunk) == 0 {
				continue
			}
			pending = append(pending, chunk...)
		}
		frame := pending[:rtpFrameSamples]
		pending = append([]int16(nil), pending[rtpFrameSamples:]...)

		// 按 20ms 节奏限速，防止 ReadPCM 立即返回时出现突发发送。
		if wait := time.Until(nextSend); wait > 0 {
			if !bridge.sleep(wait) {
				return
			}
		}
		nextSend = time.Now().Add(20 * time.Millisecond)

		clientAddr := bridge.clientAddr.Load()
		if clientAddr == nil {
			continue
		}
		payload := encodeSamplesToUlaw(frame)
		packet := bridge.buildRTPPacket(payload)
		bridge.seqNum++
		bridge.timestamp += rtpFrameSamples
		_, _ = bridge.rtpConn.WriteToUDP(packet, clientAddr)
	}
}

// loopUplink 上行：软电话 RTP → G.711μ 解码 → IMS PCM。
func (bridge *MediaBridge) loopUplink() {
	defer bridge.wg.Done()
	buffer := make([]byte, 1500)
	for {
		select {
		case <-bridge.stop:
			return
		default:
		}
		_ = bridge.rtpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, srcAddr, err := bridge.rtpConn.ReadFromUDP(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-bridge.stop:
				return
			default:
				bridge.logger.Warn("MediaBridge RTP 接收错误", "device_id", bridge.deviceID, "err", err)
				return
			}
		}
		if n < 12 {
			continue
		}
		// 动态学习客户端地址，兼容 NAT 对称端口变化。
		if current := bridge.clientAddr.Load(); current == nil || current.String() != srcAddr.String() {
			bridge.clientAddr.Store(srcAddr)
		}
		payloadType := buffer[1] & 0x7F
		if payloadType != 0 {
			continue
		}
		media := bridge.currentMedia()
		if media == nil {
			// IMS 媒体尚未就绪，丢弃上行避免写入报错。
			continue
		}
		samples := decodeUlawToSamples(buffer[12:n])
		// 写入失败多为通话结束或媒体未就绪，直接丢弃。
		_ = media.WritePCM(samples)
	}
}

// sleep 等待指定时长，收到停止信号时返回 false。
func (bridge *MediaBridge) sleep(d time.Duration) bool {
	select {
	case <-bridge.stop:
		return false
	case <-time.After(d):
		return true
	}
}

// buildRTPPacket 构造 RTP 数据包 (PT=0 PCMU, 8000Hz)。
func (bridge *MediaBridge) buildRTPPacket(payload []byte) []byte {
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80 // V=2, P=0, X=0, CC=0
	packet[1] = 0x00 // M=0, PT=0 (PCMU)
	binary.BigEndian.PutUint16(packet[2:4], bridge.seqNum)
	binary.BigEndian.PutUint32(packet[4:8], bridge.timestamp)
	binary.BigEndian.PutUint32(packet[8:12], bridge.ssrc)
	copy(packet[12:], payload)
	return packet
}

// encodeSamplesToUlaw 把 16-bit PCM 采样编码为 μ-law。
func encodeSamplesToUlaw(samples []int16) []byte {
	out := make([]byte, len(samples))
	for i, sample := range samples {
		out[i] = cscall.LinearToUlaw(sample)
	}
	return out
}

// decodeUlawToSamples 把 μ-law 解码为 16-bit PCM 采样。
func decodeUlawToSamples(ulaw []byte) []int16 {
	out := make([]int16, len(ulaw))
	for i, value := range ulaw {
		out[i] = cscall.UlawToLinear(value)
	}
	return out
}
