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

	mu               sync.RWMutex
	users            map[string]*RegisteredUser // username(小写) -> user
	byDevice         map[string]*RegisteredUser // deviceID -> user
	nonces           map[string]time.Time       // 认证 nonce -> 过期时间
	onlineSignals    map[string]chan struct{}
	internalSessions map[string]*internalSession // 网内通话 Call-ID -> 会话（BYE 拦截用）

	// sipHost 是 VoCat SIP 服务器对外可见的 host（IP 或域名），
	// 用于构建 MESSAGE/INVITE 的 From 等 header，让对端正确识别来源。
	sipHost string

	onInvite  func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onCancel  func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onAck     func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onBye     func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onPrack   func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	onMessage func(deviceID string, req *sip.Request, tx sip.ServerTransaction)

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
		cfg:              cfg,
		logger:           logger,
		ua:               ua,
		users:            make(map[string]*RegisteredUser),
		byDevice:         make(map[string]*RegisteredUser),
		nonces:           make(map[string]time.Time),
		onlineSignals:    make(map[string]chan struct{}),
		internalSessions: make(map[string]*internalSession),
		concurrencySem:   make(chan struct{}, 64),
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

// ForwardIncomingSMS 把收到的 IMS/CS 短信转发给当前所有在线的软电话。
// 转发用 SIP MESSAGE，From 头设为实际发件号码（sip:<from>@vowifi），
// Body 是短信文本。Linphone 收到后会在自己的聊天界面显示。
// 这是尽力而为：软电话可能全部离线，消息自然丢弃。
func (r *Registrar) ForwardIncomingSMS(ctx context.Context, fromNumber, text string) int {
	r.mu.RLock()
	targets := make([]*RegisteredUser, 0, len(r.users))
	for _, u := range r.users {
		if u == nil || u.ContactAddr == nil || time.Now().After(u.Expires) {
			continue
		}
		targets = append(targets, u)
	}
	r.mu.RUnlock()

	if len(targets) == 0 {
		r.logger.Debug("无在线软电话，IMS 短信不转发")
		return 0
	}

	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	sent := 0
	for _, user := range targets {
		if err := r.sendQuickMessage(ctx2, user, fromNumber, text); err == nil {
			sent++
		}
	}
	r.logger.Info("IMS 短信已转发软电话", "from", fromNumber, "recipients", sent, "total", len(targets))
	return sent
}

// sendQuickMessage 向单个软电话发一条轻量 MESSAGE（不关心响应）。
func (r *Registrar) sendQuickMessage(ctx context.Context, target *RegisteredUser, fromNumber, text string) error {
	// toHost：VoCat SIP 监听地址，用软电话 Contact 所在 IP（最保守）。
	toHost := target.ContactAddr.IP.String()

	fwd := sip.NewRequest(sip.MESSAGE, sip.Uri{
		Scheme: "sip",
		User:   target.Username,
		Host:   toHost,
		Port:   target.ContactAddr.Port,
	})
	fwd.SetDestination(target.ContactAddr.String())
	// MESSAGE 通常较小用 UDP；如果 MTU 瓶颈再改 TCP。
	if target.Transport != "" {
		fwd.SetTransport(target.Transport)
	}
	// From：实际发件号码，DisplayName 让 Linphone 直接显示号码而非完整 URI。
	from := &sip.FromHeader{
		Address:     sip.Uri{Scheme: "sip", User: fromNumber, Host: r.sipHost},
		DisplayName: fromNumber,
	}
	from.Params.Add("tag", sip.GenerateTagN(8))
	fwd.AppendHeader(from)

	to := &sip.ToHeader{
		Address: sip.Uri{Scheme: "sip", User: target.Username, Host: toHost},
	}
	fwd.AppendHeader(to)

	callID := sip.CallIDHeader(fmt.Sprintf("ims-sms-%s-%s", sip.GenerateBranch(), target.Username))
	fwd.AppendHeader(&callID)

	cseq := sip.CSeqHeader{SeqNo: 1, MethodName: sip.MESSAGE}
	fwd.AppendHeader(&cseq)

	contentType := sip.NewHeader("Content-Type", "text/plain;charset=utf-8")
	fwd.AppendHeader(contentType)

	maxfwd := sip.MaxForwardsHeader(70)
	fwd.AppendHeader(&maxfwd)

	fwd.SetBody([]byte(text))

	return r.client.WriteRequest(fwd, sipgo.ClientRequestBuild)
}

// Start 启动注册服务（UDP + TCP 同时监听）。
func (r *Registrar) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.ctx, r.cancel = context.WithCancel(ctx)

	srv, err := sipgo.NewServer(r.ua, sipgo.WithServerLogger(r.logger))
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
	// MESSAGE：网内 SIP IM 转发（同一 VoCat 下注册的软电话互发消息）。
	srv.OnRequest("MESSAGE", r.handleMessage)
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
	r.sipHost = clientHost

	client, err := sipgo.NewClient(
		r.ua,
		sipgo.WithClientHostname(clientHost),
		sipgo.WithClientPort(port),
		sipgo.WithClientNAT(),
		sipgo.WithClientLogger(r.logger),
	)
	if err != nil {
		return fmt.Errorf("创建 SIP Client 失败: %w", err)
	}
	r.client = client

	addr := r.cfg.Listen
	r.logger.Info("SIP 网关启动", "listen", addr, "realm", r.cfg.Realm, "users", len(r.cfg.Users))

	// auto：UDP + TCP 同时监听，按客户端注册的传输方式自适应；
	// udp/tcp：只监听用户指定的传输方式。
	networks := []string{"udp", "tcp"}
	if r.cfg.Transport == "udp" || r.cfg.Transport == "tcp" {
		networks = []string{r.cfg.Transport}
	}
	for _, network := range networks {
		go func(n string) {
			if err := srv.ListenAndServe(r.ctx, n, addr); err != nil && r.ctx.Err() == nil {
				r.logger.Error("SIP 监听失败", "transport", n, "err", err)
			}
		}(network)
	}
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

func (r *Registrar) SetOnMessage(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onMessage = fn
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
	// SIP 层入口日志：判定软电话的外呼 INVITE 是否真的到达 VoCat。
	callID := ""
	if header := req.GetHeader("Call-ID"); header != nil {
		callID = header.Value()
	}
	fromUser, toUser := "", ""
	if fromHeader := req.From(); fromHeader != nil {
		fromUser = fromHeader.Address.User
	}
	if toHeader := req.To(); toHeader != nil {
		toUser = toHeader.Address.User
	}
	r.logger.Info("收到软电话 SIP INVITE",
		"method", fmt.Sprint(req.Method), "call_id", callID,
		"from", fromUser, "to", toUser, "source", fmt.Sprint(req.Source()))

	from := req.From()
	if from == nil {
		r.logger.Warn("软电话 INVITE 被拒：缺少 From 头，回 400")
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}
	username := from.Address.User
	user := r.GetUserByUsername(username)
	if user == nil {
		// 软电话确实发出了外呼 INVITE，但该用户名不在注册表中（未注册或注册已过期）。
		r.logger.Warn("软电话 INVITE 被拒：该用户未注册，回 403", "username", username)
		r.respond(tx, req, 403, "Forbidden - Not Registered")
		return
	}

	// 网内路由：如果被叫也在本网关注册，直接在内部桥接两条 SIP 事务，
	// 不走 IMS/CS 运营商通道。主叫和被叫都是本地用户时才走这条路。
	callee := r.GetUserByUsername(toUser)
	if callee != nil && callee.Username != username {
		// 内部路由自己管理生命周期，不走外部 onInvite 回调，不占并发槽。
		RouteInternalInvite(r.ctx, r.logger, r.client, r, user, callee, req, tx)
		return
	}

	r.mu.RLock()
	fn := r.onInvite
	cancelFn := r.onCancel
	r.mu.RUnlock()
	if fn == nil {
		r.logger.Error("软电话 INVITE 被拒：网关未注册呼叫处理器，回 503", "username", username)
		r.respond(tx, req, 503, "Service Unavailable")
		return
	}

	select {
	case r.concurrencySem <- struct{}{}:
	default:
		r.logger.Warn("软电话 INVITE 被拒：并发槽已满，回 503", "username", username)
		r.respond(tx, req, 503, "Service Unavailable")
		return
	}
	defer func() { <-r.concurrencySem }()

	// INVITE 服务端事务必须存活到整通呼叫结束：sipgo 在 handler 返回后一定会调用
	// TerminateGracefully，若此时只回过 100/180 等临时响应，事务会被立即终结并从
	// 存储中移除（日志表现为「发送 183 早期媒体失败 err=transaction terminated」，
	// 且软电话的 ACK 变成 ack missed、CANCEL 无法按 RFC 匹配）。这里同步调用
	// handler（sipgo 每条报文独立 goroutine，阻塞是安全的），让事务一直保留。
	deviceID := user.DeviceID
	if stx, ok := tx.(*sip.ServerTx); ok {
		// 匹配到 INVITE 事务的 CANCEL 由 sipgo 自行回 200 OK + 487，不再走
		// handleCancel，因此必须通过 OnCancel 回调转发清理逻辑；回调在 FSM 内部
		// 持锁执行，必须异步派发，否则回调里调用 tx.Respond 会与 fsmMu 自死锁。
		stx.OnCancel(func(cancelReq *sip.Request) {
			if cancelFn == nil {
				return
			}
			go cancelFn(deviceID, cancelReq, tx)
		})
	}

	// 入口日志：用于区分「软电话 INVITE 已到达 VoCat 并被受理」与「从未到达」。
	calleeName := ""
	if to := req.To(); to != nil {
		calleeName = to.Address.User
	}
	r.logger.Info("受理软电话外呼 INVITE", "username", username, "device", deviceID, "callee", calleeName)
	r.respond(tx, req, 100, "Trying")
	fn(deviceID, req, tx)
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
	// 网内通话的 BYE 优先拦截：回 200 后转发给对侧，结束会话。
	// SIP 中 BYE 是独立事务，不会走 RouteInternalInvite 的事件流；
	// IMS/CS 设备桥不感知网内通话，必须在 onBye 回调之前处理。
	if callIDHdr := req.CallID(); callIDHdr != nil {
		r.mu.Lock()
		sess, ok := r.internalSessions[callIDHdr.Value()]
		if ok {
			delete(r.internalSessions, callIDHdr.Value())
		}
		r.mu.Unlock()
		if ok {
			r.respond(tx, req, 200, "OK")
			r.logger.Info("网内通话一方挂断，转发 BYE 给对侧", "call_id", callIDHdr.Value())
			sess.forwardByeToPeer(req)
			return
		}
	}
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

// registerInternalSession 登记一条进行中的网内通话。
func (r *Registrar) registerInternalSession(sess *internalSession) {
	r.mu.Lock()
	r.internalSessions[sess.callID] = sess
	r.mu.Unlock()
}

// unregisterInternalSession 注销网内通话（通话结束或异常兜底时调用）。
func (r *Registrar) unregisterInternalSession(callID string) {
	r.mu.Lock()
	delete(r.internalSessions, callID)
	r.mu.Unlock()
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

// handleMessage 处理 SIP MESSAGE 请求。
// 路由优先级：
// 1. 被叫是本地注册用户 → 网内转发到被叫的 Contact，透传响应（同步）。
// 2. 被叫非本地 → 交给外部 onMessage 回调处理（例如走 IMS/CS SMS 发到手机号）。
// 3. 没有 onMessage 回调 → 回 480 Temporarily Unavailable。
func (r *Registrar) handleMessage(req *sip.Request, tx sip.ServerTransaction) {
	to := req.To()
	if to == nil {
		r.respond(tx, req, 400, "Bad Request - Missing To")
		return
	}
	from := req.From()
	if from == nil {
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}

	// 主叫必须是本地注册用户，否则 VoCat 不应替它发 MESSAGE。
	caller := r.GetUserByUsername(from.Address.User)
	if caller == nil {
		r.logger.Info("MESSAGE 主叫未注册，拒绝", "from", from.Address.User)
		r.respond(tx, req, 403, "Forbidden - Not Registered")
		return
	}

	callee := to.Address.User
	target := r.GetUserByUsername(callee)
	callID := ""
	if h := req.GetHeader("Call-ID"); h != nil {
		callID = h.Value()
	}

	// 分支一：被叫也在本地注册 → 网内转发。
	if target != nil {
		r.logger.Info("网内 MESSAGE 转发", "from", caller.Username, "to", callee, "call_id", callID, "target", target.Source)
		r.forwardMessageToContact(req, tx, target, callID)
		return
	}

	// 分支二：被叫非本地（手机号、外部 SIP 等）→ 交给外部回调（IMS/CS SMS）。
	r.mu.RLock()
	fn := r.onMessage
	r.mu.RUnlock()
	if fn != nil {
		r.logger.Info("MESSAGE 转交外部回调", "from", caller.Username, "to", callee, "call_id", callID)
		go fn(caller.DeviceID, req, tx)
		return
	}

	// 分支三：没有 onMessage 回调，无法路由，回 480。
	r.logger.Info("MESSAGE 被叫非本地且无外部回调，拒绝", "callee", callee)
	r.respond(tx, req, 480, "Temporarily Unavailable - No Route")
}

// forwardMessageToContact 把 MESSAGE 转发到指定 RegisteredUser 的 Contact URI，并透传响应。
func (r *Registrar) forwardMessageToContact(req *sip.Request, tx sip.ServerTransaction, target *RegisteredUser, callID string) {
	callee := target.Username
	forward := sip.NewRequest(sip.MESSAGE, sip.Uri{
		Scheme: "sip",
		User:   callee,
		Host:   target.ContactAddr.IP.String(),
		Port:   target.ContactAddr.Port,
	})
	forward.SetDestination(target.ContactAddr.String())
	if target.Transport != "" {
		forward.SetTransport(target.Transport)
	}
	// Via/Max-Forwards 让 sipgo 自动生成；Route/Record-Route 丢掉（内部网内不走运营商）。
	sip.CopyHeaders("From", req, forward)
	sip.CopyHeaders("To", req, forward)
	sip.CopyHeaders("Call-ID", req, forward)
	sip.CopyHeaders("CSeq", req, forward)
	sip.CopyHeaders("Content-Type", req, forward)
	// 给被叫显示主叫用户名（如 1002），而非完整 sip:1002@vocat.local
	if fh := forward.From(); fh != nil {
		if origFrom := req.From(); origFrom != nil {
			fh.DisplayName = origFrom.Address.User
		}
	}
	forward.SetBody(req.Body())

	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()

	fwdTx, err := r.client.TransactionRequest(ctx, forward)
	if err != nil {
		r.logger.Warn("网内 MESSAGE 转发失败", "call_id", callID, "err", err)
		r.respond(tx, req, 500, "Internal Server Error")
		return
	}

	for response := range fwdTx.Responses() {
		reason := response.Reason
		if err := tx.Respond(sip.NewResponseFromRequest(req, response.StatusCode, reason, response.Body())); err != nil {
			r.logger.Warn("透传 MESSAGE 响应失败", "call_id", callID, "err", err)
		}
		return
	}
	r.respond(tx, req, 504, "Gateway Timeout - No Response")
}

func fromUser(req *sip.Request) string {
	if from := req.From(); from != nil {
		return from.Address.User
	}
	return ""
}

func (r *Registrar) userFromRequest(req *sip.Request) *RegisteredUser {
	from := req.From()
	if from == nil {
		return nil
	}
	return r.GetUserByUsername(from.Address.User)
}

func (r *Registrar) respond(tx sip.ServerTransaction, req *sip.Request, code int, reason string) {
	callID := ""
	if header := req.GetHeader("Call-ID"); header != nil {
		callID = header.Value()
	}
	if err := tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil)); err != nil {
		r.logger.Warn("发送 SIP 响应失败", "code", code, "reason", reason, "call_id", callID, "err", err)
		return
	}
	r.logger.Debug("已发送 SIP 响应", "code", code, "reason", reason, "call_id", callID)
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
