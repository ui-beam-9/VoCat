package cscall

import (
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// sdpInfo 是从对端 SDP 中解析出的媒体地址信息。
type sdpInfo struct {
	ConnectionIP string
	MediaPort    int
}

// parseSDP 从 SDP 报文体中提取连接地址与音频端口。
// 仅需要 audio 媒体段与 c= 连接行，满足 G.711 单向/双向桥接的最简需求。
func parseSDP(body []byte) (sdpInfo, error) {
	var info sdpInfo
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "c=IN IP4"):
			parts := strings.Fields(strings.TrimPrefix(line, "c=IN IP4"))
			if len(parts) > 0 {
				info.ConnectionIP = strings.TrimSpace(parts[0])
			}
		case strings.HasPrefix(line, "c=IN IP6"):
			parts := strings.Fields(strings.TrimPrefix(line, "c=IN IP6"))
			if len(parts) > 0 {
				info.ConnectionIP = strings.TrimSpace(parts[0])
			}
		case strings.HasPrefix(line, "m=audio"):
			parts := strings.Fields(strings.TrimPrefix(line, "m=audio"))
			if len(parts) > 0 {
				port, err := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err == nil {
					info.MediaPort = port
				}
			}
		}
	}
	if info.MediaPort == 0 {
		return sdpInfo{}, errors.New("SDP 中未找到音频端口")
	}
	return info, nil
}

// buildSDP 生成仅提供 PCMU(0) 的极简 SDP。
func buildSDP(ip string, port int) string {
	sessionID := time.Now().UnixNano()
	sessionVersion := rand.Int31()
	return fmt.Sprintf(
		"v=0\r\n"+
			"o=- %d %d IN IP4 %s\r\n"+
			"s=VoCat CS Call\r\n"+
			"c=IN IP4 %s\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP 0\r\n"+
			"a=rtpmap:0 PCMU/8000\r\n"+
			"a=ptime:20\r\n"+
			"a=sendrecv\r\n",
		sessionID, sessionVersion, ip, ip, port)
}
