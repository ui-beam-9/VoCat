// Package sipgw 提供面向软电话（Linphone 等）的 SIP 注册与信令网关。
//
// 该网关是 CS 域（电路域）语音桥接的 SIP 侧：接受软电话的 REGISTER 注册，
// 并把软电话发起的 INVITE/BYE/CANCEL 等信令通过回调委托给通话管理器，
// 由后者把它与 4G 模组的电路域呼叫对接起来。
package sipgw

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultListen 是 SIP 网关默认监听地址。
const DefaultListen = "0.0.0.0:5060"

// DefaultRealm 是 SIP Digest 认证默认域。
const DefaultRealm = "vocat.local"

// DefaultRTPPortMin / DefaultRTPPortMax 是 RTP 媒体端口默认范围。
const (
	DefaultRTPPortMin = 10000
	DefaultRTPPortMax = 20000
)

// User 描述一个绑定到某台受管设备的软电话账号。
type User struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
	DeviceID    string `json:"device_id"`
}

// Config 是 SIP 网关配置。
type Config struct {
	// Enabled 为 false 时网关不启动。
	Enabled bool `json:"enabled"`
	// Listen 是 SIP 监听地址，形如 "0.0.0.0:5060"。
	Listen string `json:"listen"`
	// Transport 是首选传输协议（udp/tcp），默认 udp。UDP 与 TCP 会同时监听。
	Transport string `json:"transport"`
	// Realm 是 Digest 认证域。
	Realm string `json:"realm"`
	// ExternalIP 是公网/对外 IP，留空时自动探测。
	ExternalIP string `json:"external_ip"`
	// RTPPortMin / RTPPortMax 描述允许的媒体端口范围（仅供展示与文档用途）。
	RTPPortMin int `json:"rtp_port_min"`
	RTPPortMax int `json:"rtp_port_max"`
	// Users 是允许注册的软电话账号列表。
	Users []User `json:"users"`
}

// DefaultConfig 返回带默认值的配置。
func DefaultConfig() Config {
	return Config{
		Listen:     DefaultListen,
		Transport:  "udp",
		Realm:      DefaultRealm,
		RTPPortMin: DefaultRTPPortMin,
		RTPPortMax: DefaultRTPPortMax,
	}
}

// Normalize 填充默认值并做基础清洗，不返回错误。
func (config Config) Normalize() Config {
	if strings.TrimSpace(config.Listen) == "" {
		config.Listen = DefaultListen
	}
	config.Listen = strings.TrimSpace(config.Listen)
	config.Transport = strings.ToLower(strings.TrimSpace(config.Transport))
	if config.Transport != "tcp" {
		config.Transport = "udp"
	}
	config.Realm = strings.TrimSpace(config.Realm)
	if config.Realm == "" {
		config.Realm = DefaultRealm
	}
	config.ExternalIP = strings.TrimSpace(config.ExternalIP)
	if config.RTPPortMin <= 0 {
		config.RTPPortMin = DefaultRTPPortMin
	}
	if config.RTPPortMax <= 0 || config.RTPPortMax < config.RTPPortMin {
		config.RTPPortMax = DefaultRTPPortMax
	}
	users := make([]User, 0, len(config.Users))
	seen := make(map[string]struct{}, len(config.Users))
	for _, user := range config.Users {
		user.Username = strings.TrimSpace(user.Username)
		user.Password = strings.TrimSpace(user.Password)
		user.DisplayName = strings.TrimSpace(user.DisplayName)
		user.DeviceID = strings.TrimSpace(user.DeviceID)
		if user.Username == "" {
			continue
		}
		key := strings.ToLower(user.Username)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		users = append(users, user)
	}
	config.Users = users
	return config
}

// Validate 校验配置，返回可读的错误信息。
func (config Config) Validate() error {
	normalized := config.Normalize()
	host, portText, err := net.SplitHostPort(normalized.Listen)
	if err != nil {
		return fmt.Errorf("sip listen address %q is invalid: %w", config.Listen, err)
	}
	_ = host
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("sip listen port %q is invalid", portText)
	}
	if normalized.ExternalIP != "" && net.ParseIP(normalized.ExternalIP) == nil {
		return errors.New("sip external_ip must be a valid IP address")
	}
	for _, user := range normalized.Users {
		if user.Password == "" {
			return fmt.Errorf("sip user %q must set a password", user.Username)
		}
		if len(user.Password) > 128 || strings.ContainsAny(user.Password, "\r\n\x00") {
			return fmt.Errorf("sip user %q password contains invalid characters", user.Username)
		}
		if len(user.Username) > 64 || strings.ContainsAny(user.Username, "\r\n\x00 :@") {
			return fmt.Errorf("sip user %q name contains invalid characters", user.Username)
		}
	}
	return nil
}

// UserCount 返回有效账号数量。
func (config Config) UserCount() int {
	return len(config.Normalize().Users)
}
