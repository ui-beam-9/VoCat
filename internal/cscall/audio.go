package cscall

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// alsaBinCache 缓存 arecord/aplay 的绝对路径。
// 后台守护进程启动时继承的 $PATH 常被裁剪，用裸命令名调用会在子进程
// Start() 时因找不到可执行文件而报错，因此启动时探测一次绝对路径并复用。
var alsaBinCache = struct {
	sync.Mutex
	arecord string
	aplay   string
	ok      bool
}{}

func resolveAlsaBin(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil && path != "" {
		return path, nil
	}
	candidates := []string{
		"/usr/bin/" + name,
		"/bin/" + name,
		"/usr/local/bin/" + name,
		"/sbin/" + name,
		"/usr/sbin/" + name,
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s 未找到：请确认已安装 alsa-utils（如 apk add alsa-utils）", name)
}

func alsaEnv() []string {
	return []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
}

// AudioBridge 是 PCM ↔ RTP 音频桥接器：通过 arecord/aplay 操作 ALSA 声卡
// （模组 UAC 音频设备，如 "hw:1,0"），与软电话的 RTP 流双向桥接。
type AudioBridge struct {
	alsaDev    string
	deviceID   string
	logger     *slog.Logger
	rtpConn    *net.UDPConn
	clientAddr atomic.Pointer[net.UDPAddr]

	// pcmReady 来自 +QPCMV URC 的 PCM 流控状态。
	pcmReady atomic.Bool

	captureCmd  *exec.Cmd
	playbackCmd *exec.Cmd
	captureOut  io.ReadCloser
	playbackIn  io.WriteCloser

	seqNum    uint16
	timestamp uint32
	ssrc      uint32

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewAudioBridge 创建音频桥接器并绑定一个随机 UDP 端口用于 RTP。
func NewAudioBridge(alsaDev, deviceID string, logger *slog.Logger) (*AudioBridge, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("绑定 RTP 端口失败: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	bridge := &AudioBridge{
		alsaDev:  alsaDev,
		deviceID: deviceID,
		logger:   logger,
		rtpConn:  conn,
		ssrc:     0x12345678,
		stop:     make(chan struct{}),
	}
	bridge.pcmReady.Store(false)
	return bridge, nil
}

// LocalPort 返回 RTP 本地监听端口（写入 SDP）。
func (bridge *AudioBridge) LocalPort() int {
	return bridge.rtpConn.LocalAddr().(*net.UDPAddr).Port
}

// SetClientAddr 设置软电话的 RTP 地址（来自 SDP 协商）。
func (bridge *AudioBridge) SetClientAddr(ip string, port int) {
	addr := &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
	bridge.clientAddr.Store(addr)
	bridge.logger.Info("AudioBridge 设置 RTP 客户端地址",
		"device_id", bridge.deviceID, "ip", ip, "port", port)
}

// SetPCMReady 设置 PCM 流控状态。
func (bridge *AudioBridge) SetPCMReady(ready bool) { bridge.pcmReady.Store(ready) }

// Start 启动双向桥接。
func (bridge *AudioBridge) Start() error {
	alsaBinCache.Lock()
	if !alsaBinCache.ok {
		arecord, arecordErr := resolveAlsaBin("arecord")
		aplay, aplayErr := resolveAlsaBin("aplay")
		if arecordErr == nil && aplayErr == nil {
			alsaBinCache.arecord = arecord
			alsaBinCache.aplay = aplay
			alsaBinCache.ok = true
		}
	}
	arecordPath, aplayPath := alsaBinCache.arecord, alsaBinCache.aplay
	alsaBinCache.Unlock()

	if arecordPath == "" || aplayPath == "" {
		if _, err := resolveAlsaBin("arecord"); err != nil {
			return fmt.Errorf("启动音频桥失败: %w", err)
		}
		return fmt.Errorf("启动音频桥失败: %w", fmt.Errorf("aplay 未找到：请确认已安装 alsa-utils"))
	}
	bridge.logger.Info("AudioBridge 使用 alsa 工具",
		"device_id", bridge.deviceID, "arecord", arecordPath, "aplay", aplayPath)

	// 下行采集: 模组 → 软电话。每 40ms 输出 640 字节（320 samples × 2B, 8kHz S16_LE mono）。
	bridge.captureCmd = exec.Command(arecordPath,
		"-D", bridge.alsaDev,
		"-f", "S16_LE",
		"-r", "8000",
		"-c", "1",
		"-t", "raw",
		"--buffer-size", "640",
		"--period-size", "320",
	)
	bridge.captureCmd.Env = alsaEnv()
	var err error
	bridge.captureOut, err = bridge.captureCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 arecord 管道失败: %w", err)
	}
	if err := bridge.captureCmd.Start(); err != nil {
		return fmt.Errorf("启动 arecord 失败: %w", err)
	}

	// 上行播放: 软电话 → 模组。
	bridge.playbackCmd = exec.Command(aplayPath,
		"-D", bridge.alsaDev,
		"-f", "S16_LE",
		"-r", "8000",
		"-c", "1",
		"-t", "raw",
		"--buffer-size", "1600",
		"--period-size", "800",
	)
	bridge.playbackCmd.Env = alsaEnv()
	bridge.playbackIn, err = bridge.playbackCmd.StdinPipe()
	if err != nil {
		_ = bridge.captureCmd.Process.Kill()
		return fmt.Errorf("创建 aplay 管道失败: %w", err)
	}
	if err := bridge.playbackCmd.Start(); err != nil {
		_ = bridge.captureCmd.Process.Kill()
		return fmt.Errorf("启动 aplay 失败: %w", err)
	}

	bridge.logger.Info("AudioBridge 已启动",
		"device_id", bridge.deviceID, "alsa", bridge.alsaDev, "rtp_port", bridge.LocalPort())

	bridge.wg.Add(2)
	go bridge.loopCapture()
	go bridge.loopPlayback()
	go bridge.loopSilenceKeepalive()
	return nil
}

// loopSilenceKeepalive 在 PCM 声道建立之前（最多 5 秒）每 20ms 发一个静音包，
// 防止软电话因长时间收不到 RTP 而超时挂断。
func (bridge *AudioBridge) loopSilenceKeepalive() {
	silencePCM := make([]byte, 320)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-bridge.stop:
			return
		case <-deadline:
			return
		case <-ticker.C:
			clientAddr := bridge.clientAddr.Load()
			if clientAddr == nil || bridge.pcmReady.Load() {
				if bridge.pcmReady.Load() {
					return
				}
				continue
			}
			payload := EncodePCMToUlaw(silencePCM)
			packet := bridge.buildRTPPacket(payload)
			bridge.seqNum++
			bridge.timestamp += 160
			_, _ = bridge.rtpConn.WriteToUDP(packet, clientAddr)
		}
	}
}

// Stop 停止桥接并释放资源。
func (bridge *AudioBridge) Stop() {
	bridge.stopOnce.Do(func() {
		close(bridge.stop)
		_ = bridge.rtpConn.Close()
		if bridge.captureCmd != nil && bridge.captureCmd.Process != nil {
			_ = bridge.captureCmd.Process.Kill()
		}
		if bridge.playbackIn != nil {
			_ = bridge.playbackIn.Close()
		}
		if bridge.playbackCmd != nil && bridge.playbackCmd.Process != nil {
			_ = bridge.playbackCmd.Process.Kill()
		}
		go func() {
			waitCh := make(chan struct{})
			go func() {
				bridge.wg.Wait()
				close(waitCh)
			}()
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
				bridge.logger.Warn("AudioBridge 停止超时，强制放弃等待", "device_id", bridge.deviceID)
			}
		}()
	})
}

// loopCapture 下行: arecord stdout → G.711μ 编码 → RTP → 软电话。
func (bridge *AudioBridge) loopCapture() {
	defer bridge.wg.Done()
	pcmBuffer := make([]byte, 640)
	for {
		select {
		case <-bridge.stop:
			return
		default:
		}
		n, err := io.ReadFull(bridge.captureOut, pcmBuffer)
		if err != nil {
			select {
			case <-bridge.stop:
				return
			default:
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					bridge.logger.Warn("AudioBridge arecord 读取错误", "device_id", bridge.deviceID, "err", err)
				}
				return
			}
		}
		clientAddr := bridge.clientAddr.Load()
		if clientAddr == nil {
			continue
		}
		// 拆成 2 个 20ms 帧。
		for frame := 0; frame < 2 && frame*320 < n; frame++ {
			start := frame * 320
			end := start + 320
			if end > n {
				end = n
			}
			payload := EncodePCMToUlaw(pcmBuffer[start:end])
			packet := bridge.buildRTPPacket(payload)
			bridge.seqNum++
			bridge.timestamp += 160
			_, _ = bridge.rtpConn.WriteToUDP(packet, clientAddr)
		}
	}
}

// loopPlayback 上行: 软电话 RTP → G.711μ 解码 → aplay stdin。
func (bridge *AudioBridge) loopPlayback() {
	defer bridge.wg.Done()
	buffer := make([]byte, 1500)
	accumulator := make([]byte, 0, 1600)
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
				if len(accumulator) > 0 {
					_, _ = bridge.playbackIn.Write(accumulator)
					accumulator = accumulator[:0]
				}
				continue
			}
			select {
			case <-bridge.stop:
				return
			default:
				bridge.logger.Warn("AudioBridge RTP 接收错误", "device_id", bridge.deviceID, "err", err)
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
		if !bridge.pcmReady.Load() {
			// 模组 PCM 尚未就绪，暂不向上行注入，避免爆音或阻塞。
			continue
		}
		payloadType := buffer[1] & 0x7F
		if payloadType != 0 {
			continue
		}
		pcmData := DecodeUlawToPCM(buffer[12:n])
		accumulator = append(accumulator, pcmData...)
		if len(accumulator) >= 1600 {
			_, _ = bridge.playbackIn.Write(accumulator[:1600])
			remaining := make([]byte, len(accumulator)-1600)
			copy(remaining, accumulator[1600:])
			accumulator = remaining
		}
	}
}

// buildRTPPacket 构造 RTP 数据包 (PT=0 PCMU, 8000Hz)。
func (bridge *AudioBridge) buildRTPPacket(payload []byte) []byte {
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80 // V=2, P=0, X=0, CC=0
	packet[1] = 0x00 // M=0, PT=0 (PCMU)
	binary.BigEndian.PutUint16(packet[2:4], bridge.seqNum)
	binary.BigEndian.PutUint32(packet[4:8], bridge.timestamp)
	binary.BigEndian.PutUint32(packet[8:12], bridge.ssrc)
	copy(packet[12:], payload)
	return packet
}
