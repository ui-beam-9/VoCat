package sipgw

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// RegisteredUser 描述一个当前在线的软电话账号。
type RegisteredUser struct {
	Username    string
	DeviceID    string
	DisplayName string
	ContactURI  string
	ContactAddr *net.UDPAddr
	Source      string
	Transport   string
	UserAgent   string
	Expires     time.Time
}

// Registrar 是软电话 SIP 注册服务。它接受 REGISTER 请求并通过 Digest 认证
// 维护在线账号，同时把 INVITE/ACK/BYE/CANCEL/PRACK 等信令通过回调外发。
type Registrar struct {
	cfg    Config
	logger *slog.Logger
	ua     *sipgo.UserAgent
	client *sipgo.Client
	server *sipgo.Server

	mu            sync.RWMutex
	users         map[string]*RegisteredUser // username(小写) -> user
	byDevice      map[string]*RegisteredUser // deviceID -> user
	nonces        map[string]time.Time       // 认证 nonce -> 过期时间
	onlineSignals map[string]chan struct{}

	onInvite func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onCancel func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onAck    func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onBye    func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onPrack  func(deviceID string, req *sip.Request, tx sip.ServerTransaction)

	concurrencySem chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

// NewRegistrar 创建 SIP 注册服务。
func NewRegistrar(cfg Config, logger *slog.Logger) (*Registrar, error) {
	cfg = cfg.Normalize()
	if logger == nil {
		logger = slog.Default()
	}
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("VoCat/1.0"))
	if err != nil {
		return nil, fmt.Errorf("创建 SIP UserAgent 失败: %w", err)
	}
	return &Registrar{
		cfg:            cfg,
		logger:         logger,
		ua:             ua,
		users:          make(map[string]*RegisteredUser),
		byDevice:       make(map[string]*RegisteredUser),
		nonces:         make(map[string]time.Time),
		onlineSignals:  make(map[string]chan struct{}),
		concurrencySem: make(chan struct{}, 64),
	}, nil
}

// Config 返回生效的配置。
func (r *Registrar) Config() Config { return r.cfg }

// GetClient 返回用于外发 INVITE 的 SIP Client。
func (r *Registrar) GetClient() *sipgo.Client { return r.client }

// GetExternalIP 返回配置或自动探测得到的对外 IP。
func (r *Registrar) GetExternalIP() string { return r.cfg.ExternalIP }

// CountRegistered 返回在线账号数量。
func (r *Registrar) CountRegistered() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byDevice)
}

// Start 启动注册服务（UDP + TCP 同时监听）。
func (r *Registrar) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.ctx, r.cancel = context.WithCancel(ctx)

	srv, err := sipgo.NewServer(r.ua)
	if err != nil {
		return fmt.Errorf("创建 SIP Server 失败: %w", err)
	}
	r.server = srv

	srv.OnRegister(r.handleRegister)
	srv.OnInvite(r.handleInvite)
	srv.OnAck(r.handleAck)
	srv.OnBye(r.handleBye)
	srv.OnRequest("CANCEL", r.handleCancel)
	srv.OnRequest("PRACK", r.handlePrack)
	// Linphone 注册后通常发送 PUBLISH 上报在线状态，直接回 200 OK。
	srv.OnRequest("PUBLISH", func(req *sip.Request, tx sip.ServerTransaction) {
		r.respond(tx, req, 200, "OK")
	})
	// OPTIONS 心跳一并回 200，便于软电话保活。
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		r.respond(tx, req, 200, "OK")
	})

	host, portText, err := net.SplitHostPort(r.cfg.Listen)
	if err != nil {
		return fmt.Errorf("解析 SIP 监听地址失败: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("解析 SIP 监听端口失败: %w", err)
	}

	clientHost := r.cfg.ExternalIP
	if clientHost == "" {
		switch host {
		case "", "0.0.0.0", "::":
			clientHost = firstNonLoopbackIPv4()
		default:
			clientHost = host
		}
	}
	if clientHost == "" {
		clientHost = "127.0.0.1"
	}

	client, err := sipgo.NewClient(
		r.ua,
		sipgo.WithClientHostname(clientHost),
		sipgo.WithClientPort(port),
		sipgo.WithClientNAT(),
	)
	if err != nil {
		return fmt.Errorf("创建 SIP Client 失败: %w", err)
	}
	r.client = client

	addr := r.cfg.Listen
	r.logger.Info("SIP 网关启动", "listen", addr, "realm", r.cfg.Realm, "users", len(r.cfg.Users))

	go func() {
		if err := srv.ListenAndServe(r.ctx, "udp", addr); err != nil && r.ctx.Err() == nil {
			r.logger.Error("SIP UDP 监听失败", "err", err)
		}
	}()
	go func() {
		if err := srv.ListenAndServe(r.ctx, "tcp", addr); err != nil && r.ctx.Err() == nil {
			r.logger.Error("SIP TCP 监听失败", "err", err)
		}
	}()
	go r.cleanupLoop()

	return nil
}

// Stop 停止注册服务。
func (r *Registrar) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	if r.server != nil {
		_ = r.server.Close()
	}
	if r.client != nil {
		_ = r.client.Close()
	}
	if r.ua != nil {
		_ = r.ua.Close()
	}
	return nil
}

// --- 回调注册 ---

func (r *Registrar) SetOnInvite(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onInvite = fn
	r.mu.Unlock()
}

func (r *Registrar) SetOnCancel(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onCancel = fn
	r.mu.Unlock()
}

func (r *Registrar) SetOnAck(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onAck = fn
	r.mu.Unlock()
}

func (r *Registrar) SetOnBye(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onBye = fn
	r.mu.Unlock()
}

func (r *Registrar) SetOnPrack(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onPrack = fn
	r.mu.Unlock()
}

// --- REGISTER ---

func (r *Registrar) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	if from == nil {
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}
	username := from.Address.User
	if username == "" {
		r.respond(tx, req, 400, "Bad Request - Missing From User")
		return
	}
	key := strings.ToLower(username)

	authHeader := req.GetHeader("Authorization")
	if authHeader == nil {
		r.sendAuthChallenge(tx, req)
		return
	}
	if !r.validateAuth(username, authHeader.Value()) {
		r.logger.Warn("REGISTER 认证失败", "username", username)
		r.sendAuthChallenge(tx, req)
		return
	}

	userCfg := r.findUserConfig(username)
	if userCfg == nil {
		r.respond(tx, req, 403, "Forbidden - User not configured")
		return
	}

	expires := r.parseExpires(req)
	if expires == 0 {
		r.unregisterUser(key)
		r.respond(tx, req, 200, "OK")
		r.logger.Info("软电话注销成功", "username", username, "device", userCfg.DeviceID)
		return
	}

	contact := req.Contact()
	contactURI := ""
	if contact != nil {
		contactURI = contact.Address.String()
	}
	userAgent := ""
	if uaHeader := req.GetHeader("User-Agent"); uaHeader != nil {
		userAgent = uaHeader.Value()
	}

	var contactAddr *net.UDPAddr
	if src := fmt.Sprint(req.Source()); src != "" && src != "<nil>" {
		if addr, err := net.ResolveUDPAddr("udp", src); err == nil {
			contactAddr = addr
		}
	}

	r.registerUser(key, username, userCfg.DeviceID, userCfg.DisplayName, contactURI, contactAddr, req.Transport(), userAgent, expires)

	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if contact != nil {
		res.AppendHeader(contact)
	}
	res.AppendHeader(sip.NewHeader("Expires", fmt.Sprintf("%d", expires)))
	tx.Respond(res)

	r.logger.Info("软电话注册成功",
		"username", username, "device", userCfg.DeviceID,
		"contact", contactURI, "expires", expires, "ua", userAgent)
}

func (r *Registrar) sendAuthChallenge(tx sip.ServerTransaction, req *sip.Request) {
	nonce := generateNonce()
	r.mu.Lock()
	r.nonces[nonce] = time.Now().Add(60 * time.Second)
	r.mu.Unlock()

	res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	value := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, r.cfg.Realm, nonce)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", value))
	_ = tx.Respond(res)
}

func (r *Registrar) validateAuth(username, authValue string) bool {
	params := parseDigestAuth(authValue)
	nonce := params["nonce"]
	response := params["response"]
	uri := params["uri"]

	r.mu.Lock()
	nonceTime, ok := r.nonces[nonce]
	if ok {
		delete(r.nonces, nonce)
	}
	r.mu.Unlock()
	if !ok || time.Now().After(nonceTime) {
		return false
	}

	userCfg := r.findUserConfig(username)
	if userCfg == nil {
		return false
	}
	ha1 := md5sum(username + ":" + r.cfg.Realm + ":" + userCfg.Password)
	ha2 := md5sum("REGISTER:" + uri)
	return response == md5sum(ha1+":"+nonce+":"+ha2)
}

// parseDigestAuth 解析 Digest Authorization 头为键值对。
func parseDigestAuth(authValue string) map[string]string {
	result := make(map[string]string)
	authValue = strings.TrimPrefix(authValue, "Digest ")
	authValue = strings.TrimPrefix(authValue, "digest ")
	for _, part := range strings.Split(authValue, ",") {
		part = strings.TrimSpace(part)
		index := strings.Index(part, "=")
		if index < 0 {
			continue
		}
		key := strings.TrimSpace(part[:index])
		value := strings.TrimSpace(part[index+1:])
		result[key] = strings.Trim(value, `"`)
	}
	return result
}

func (r *Registrar) parseExpires(req *sip.Request) int {
	if header := req.GetHeader("Expires"); header != nil {
		var expires int
		if _, err := fmt.Sscanf(header.Value(), "%d", &expires); err == nil {
			return expires
		}
	}
	if contact := req.Contact(); contact != nil {
		contactStr := contact.String()
		if index := strings.Index(strings.ToLower(contactStr), "expires="); index >= 0 {
			var expires int
			if _, err := fmt.Sscanf(contactStr[index+8:], "%d", &expires); err == nil {
				return expires
			}
		}
	}
	return 3600
}

func (r *Registrar) registerUser(
	key, username, deviceID, displayName, contactURI string,
	contactAddr *net.UDPAddr, transport, userAgent string, expires int,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	user := &RegisteredUser{
		Username:    username,
		DeviceID:    deviceID,
		DisplayName: displayName,
		ContactURI:  contactURI,
		ContactAddr: contactAddr,
		Transport:   transport,
		UserAgent:   userAgent,
		Expires:     time.Now().Add(time.Duration(expires) * time.Second),
	}
	if contactAddr != nil {
		user.Source = contactAddr.String()
	}
	r.users[key] = user
	if deviceID != "" {
		r.byDevice[deviceID] = user
		if ch, ok := r.onlineSignals[deviceID]; ok && ch != nil {
			close(ch)
		}
		r.onlineSignals[deviceID] = make(chan struct{})
	}
}

func (r *Registrar) unregisterUser(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if user, ok := r.users[key]; ok {
		user.ContactAddr = nil
		user.ContactURI = ""
		user.Source = ""
		user.Transport = ""
	}
}

// GetUserByDevice 按设备 ID 查在线账号，账号已过期时返回 nil。
func (r *Registrar) GetUserByDevice(deviceID string) *RegisteredUser {
	r.mu.RLock()
	defer r.mu.RUnlock()
	user := r.byDevice[deviceID]
	if user != nil && time.Now().Before(user.Expires) && user.ContactAddr != nil {
		return user
	}
	return nil
}

// GetUserByUsername 按用户名查在线账号。
func (r *Registrar) GetUserByUsername(username string) *RegisteredUser {
	r.mu.RLock()
	defer r.mu.RUnlock()
	user := r.users[strings.ToLower(username)]
	if user != nil && time.Now().Before(user.Expires) {
		return user
	}
	return nil
}

func (r *Registrar) findUserConfig(username string) *User {
	for index := range r.cfg.Users {
		if strings.EqualFold(r.cfg.Users[index].Username, username) {
			return &r.cfg.Users[index]
		}
	}
	return nil
}

func (r *Registrar) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.cleanup()
		}
	}
}

func (r *Registrar) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for key, user := range r.users {
		if now.After(user.Expires) {
			if user.DeviceID != "" {
				delete(r.byDevice, user.DeviceID)
			}
			delete(r.users, key)
		}
	}
	for nonce, expiry := range r.nonces {
		if now.After(expiry) {
			delete(r.nonces, nonce)
		}
	}
}

// --- 呼叫信令回调路由 ---

func (r *Registrar) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	if from == nil {
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}
	username := from.Address.User
	user := r.GetUserByUsername(username)
	if user == nil {
		r.respond(tx, req, 403, "Forbidden - Not Registered")
		return
	}
	r.respond(tx, req, 100, "Trying")

	r.mu.RLock()
	fn := r.onInvite
	r.mu.RUnlock()
	if fn == nil {
		r.respond(tx, req, 503, "Service Unavailable")
		return
	}
	select {
	case r.concurrencySem <- struct{}{}:
		go func() {
			defer func() { <-r.concurrencySem }()
			fn(user.DeviceID, req, tx)
		}()
	default:
		r.respond(tx, req, 503, "Service Unavailable")
	}
}

func (r *Registrar) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	user := r.userFromRequest(req)
	if user == nil {
		return
	}
	r.mu.RLock()
	fn := r.onAck
	r.mu.RUnlock()
	if fn != nil {
		go fn(user.DeviceID, req, tx)
	}
}

func (r *Registrar) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	r.respond(tx, req, 200, "OK")
	user := r.userFromRequest(req)
	if user == nil {
		return
	}
	r.mu.RLock()
	fn := r.onBye
	r.mu.RUnlock()
	if fn != nil {
		go fn(user.DeviceID, req, tx)
	}
}

func (r *Registrar) handleCancel(req *sip.Request, tx sip.ServerTransaction) {
	// CANCEL 必须无条件回 200 OK，否则呼叫方会持续重传。
	r.respond(tx, req, 200, "OK")
	user := r.userFromRequest(req)
	r.mu.RLock()
	fn := r.onCancel
	r.mu.RUnlock()
	if fn != nil && user != nil {
		go fn(user.DeviceID, req, tx)
	}
}

func (r *Registrar) handlePrack(req *sip.Request, tx sip.ServerTransaction) {
	user := r.userFromRequest(req)
	r.mu.RLock()
	fn := r.onPrack
	r.mu.RUnlock()
	if fn != nil && user != nil {
		go fn(user.DeviceID, req, tx)
		return
	}
	_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
}

func (r *Registrar) userFromRequest(req *sip.Request) *RegisteredUser {
	from := req.From()
	if from == nil {
		return nil
	}
	return r.GetUserByUsername(from.Address.User)
}

func (r *Registrar) respond(tx sip.ServerTransaction, req *sip.Request, code int, reason string) {
	if err := tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil)); err != nil {
		r.logger.Warn("发送 SIP 响应失败", "code", code, "reason", reason, "err", err)
	}
}

func md5sum(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func generateNonce() string {
	buffer := make([]byte, 16)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipv4 := ipNet.IP.To4(); ipv4 != nil {
			return ipv4.String()
		}
	}
	return ""
}
