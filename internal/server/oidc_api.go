package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"

	"vocat/internal/oidc"
	"vocat/internal/store"
)

const (
	// oidcPendingTTL 是服务端临时授权状态（state/nonce/PKCE）的存活时间。
	oidcPendingTTL = 10 * time.Minute
	// oidcPendingLimit 限制服务端临时授权状态的最大条数，超出后淘汰最旧一条。
	oidcPendingLimit = 200
	// oidcRequestTimeout 是访问 IDP 的单次请求整体超时。
	oidcRequestTimeout = 15 * time.Second
	// oidcConnectTimeout 是访问 IDP 的建连/TLS/响应头超时。
	oidcConnectTimeout = 15 * time.Second
	// oidcLoginURL 是登录页 OIDC 按钮指向的地址。
	oidcLoginURL = "/api/auth/oidc/login"
	// oidcLoginPopupMode 是登录页弹窗登录（POST /api/auth/oidc/login/start）的 pending state 模式：
	// 回调不 302，而是在弹出窗口内返回结果页并通过 postMessage 通知 opener。
	// oidc.NewAuthorizationRequest 只接受 login/bind，授权 URL 本身与 mode 无关，
	// 因此发起时按 login 构造，弹窗语义仅记录在 pending state 里。
	oidcLoginPopupMode = "login_popup"
)

// oidcPendingState 记录一次 OIDC 授权流程的服务端临时状态。
// state 不通过 Cookie 传递：绑定授权在独立弹出窗口完成、登录回调是跨站跳转，
// SameSite 严格 Cookie 在这些场景下会丢失。
type oidcPendingState struct {
	Nonce        string
	CodeVerifier string
	RedirectURI  string
	RedirectPath string
	Mode         string
	CreatedAt    time.Time
}

// oidcSnapshot 是 Server 内存缓存的一次性快照，避免每个请求都读数据库。
type oidcSnapshot struct {
	Config         oidc.Config
	Metadata       oidc.Metadata
	Discovered     bool
	DiscoveryError string
	Binding        oidc.Binding
	Bound          bool
}

// newOIDCHTTPClient 构造访问 IDP 的普通 HTTP 客户端。
// 自建 IDP 常部署在内网，因此刻意不复用 restrictedHTTPClient（它只允许公网地址），
// 这里使用带 15s 连接/整体超时的普通客户端。
func newOIDCHTTPClient() *http.Client {
	return &http.Client{
		Timeout: oidcRequestTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   oidcConnectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   oidcConnectTimeout,
			ResponseHeaderTimeout: oidcConnectTimeout,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// oidcClient 返回访问 IDP 的 HTTP 客户端（延迟初始化，兼容直接构造的 Server）。
func (s *Server) oidcClient() *http.Client {
	s.oidcMu.RLock()
	client := s.oidcHTTPClient
	s.oidcMu.RUnlock()
	if client != nil {
		return client
	}
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidcHTTPClient == nil {
		s.oidcHTTPClient = newOIDCHTTPClient()
	}
	return s.oidcHTTPClient
}

// oidcState 读取 OIDC 配置/端点/绑定缓存快照。
func (s *Server) oidcState() oidcSnapshot {
	s.oidcMu.RLock()
	defer s.oidcMu.RUnlock()
	return oidcSnapshot{
		Config:         s.oidcConfig,
		Metadata:       s.oidcMetadata,
		Discovered:     s.oidcDiscovered,
		DiscoveryError: s.oidcDiscoveryError,
		Binding:        s.oidcBinding,
		Bound:          s.oidcBound,
	}
}

// updateOIDCDiscovery 写入端点发现结果；discoveryErr 非空时记录错误并清空端点。
func (s *Server) updateOIDCDiscovery(metadata oidc.Metadata, discoveryErr error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if discoveryErr == nil {
		s.oidcMetadata = metadata
		s.oidcDiscovered = true
		s.oidcDiscoveryError = ""
		return
	}
	s.oidcMetadata = oidc.Metadata{}
	s.oidcDiscovered = false
	s.oidcDiscoveryError = discoveryErr.Error()
}

// clearOIDCDiscovery 清空端点缓存（OIDC 关闭或未配置时）。
func (s *Server) clearOIDCDiscovery() {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	s.oidcMetadata = oidc.Metadata{}
	s.oidcDiscovered = false
	s.oidcDiscoveryError = ""
}

// loadOIDCConfig 在启动时把 OIDC 配置、绑定关系和端点发现结果载入内存缓存。
func (s *Server) loadOIDCConfig(ctx context.Context) {
	config := oidc.Config{}
	setting, err := s.store.AppSetting(ctx, oidc.SettingConfigKey)
	switch {
	case err == nil:
		parsed, parseErr := oidc.ParseConfig(setting.Value)
		if parseErr != nil {
			s.logger.Warn("加载 OIDC 配置失败", "error", parseErr)
		} else {
			config = parsed
		}
	case !errors.Is(err, store.ErrNotFound):
		s.logger.Warn("读取 OIDC 配置失败", "error", err)
	}

	binding := oidc.Binding{}
	bound := false
	setting, err = s.store.AppSetting(ctx, oidc.SettingBindingKey)
	switch {
	case err == nil:
		parsed, parseErr := oidc.ParseBinding(setting.Value)
		if parseErr != nil {
			s.logger.Warn("加载 OIDC 绑定信息失败", "error", parseErr)
		} else {
			binding, bound = parsed, true
		}
	case !errors.Is(err, store.ErrNotFound):
		s.logger.Warn("读取 OIDC 绑定信息失败", "error", err)
	}

	s.oidcMu.Lock()
	s.oidcConfig = config
	s.oidcBinding = binding
	s.oidcBound = bound
	s.oidcMetadata = oidc.Metadata{}
	s.oidcDiscovered = false
	s.oidcDiscoveryError = ""
	s.oidcMu.Unlock()

	if config.Enabled && oidcConfigComplete(config) {
		// 启动时异步发现端点，避免 IDP 不可达拖慢服务启动。
		go func() {
			if refreshErr := s.refreshOIDCDiscovery(context.Background()); refreshErr != nil {
				s.logger.Warn("OIDC 端点发现失败", "error", refreshErr)
			}
		}()
	}
}

// refreshOIDCDiscovery 重新请求 IDP 的 discovery 文档并刷新端点缓存。
func (s *Server) refreshOIDCDiscovery(ctx context.Context) error {
	snapshot := s.oidcState()
	if !snapshot.Config.Enabled || !oidcConfigComplete(snapshot.Config) {
		s.clearOIDCDiscovery()
		return nil
	}
	discoveryContext, cancel := context.WithTimeout(ctx, oidcRequestTimeout)
	defer cancel()
	metadata, err := oidc.Discover(discoveryContext, s.oidcClient(), snapshot.Config.WellKnownURL)
	s.updateOIDCDiscovery(metadata, err)
	return err
}

// oidcConfigComplete 判断配置是否已填齐发现与客户端所需的最小信息。
func oidcConfigComplete(config oidc.Config) bool {
	return strings.TrimSpace(config.WellKnownURL) != "" && strings.TrimSpace(config.ClientID) != ""
}

// oidcLoginButtonText 返回登录按钮文案（空值时退回包内默认值）。
func oidcLoginButtonText(config oidc.Config) string {
	text := strings.TrimSpace(config.LoginButtonText)
	if text == "" {
		return oidc.DefaultLoginButtonText
	}
	return text
}

// enabled 表示登录页可以展示 OIDC 入口：已启用 + 配置完整 + 已发现端点。
func (snapshot oidcSnapshot) enabled() bool {
	return snapshot.Config.Enabled && oidcConfigComplete(snapshot.Config) && snapshot.Discovered
}

// bindingValid 表示绑定关系仍然生效：已绑定，且（端点已发现时）issuer 与当前配置一致。
// issuer 变更视为绑定失效，需要重新绑定。
func (snapshot oidcSnapshot) bindingValid() bool {
	if !snapshot.Bound || strings.TrimSpace(snapshot.Binding.Subject) == "" {
		return false
	}
	currentIssuer := strings.TrimSpace(snapshot.Metadata.Issuer)
	boundIssuer := strings.TrimSpace(snapshot.Binding.Issuer)
	if snapshot.Discovered && currentIssuer != "" && !strings.EqualFold(currentIssuer, boundIssuer) {
		return false
	}
	return true
}

// oidcStorePending 写入一次授权流程的临时状态，并按 TTL/上限清理旧记录。
func (s *Server) oidcStorePending(state string, pending oidcPendingState) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidcPending == nil {
		s.oidcPending = make(map[string]oidcPendingState)
	}
	now := time.Now()
	for key, entry := range s.oidcPending {
		if now.Sub(entry.CreatedAt) > oidcPendingTTL {
			delete(s.oidcPending, key)
		}
	}
	if len(s.oidcPending) >= oidcPendingLimit {
		oldestKey := ""
		var oldest time.Time
		for key, entry := range s.oidcPending {
			if oldestKey == "" || entry.CreatedAt.Before(oldest) {
				oldestKey, oldest = key, entry.CreatedAt
			}
		}
		if oldestKey != "" {
			delete(s.oidcPending, oldestKey)
		}
	}
	s.oidcPending[state] = pending
}

// oidcTakePending 取出并删除一次授权状态（一次性使用，防止重放）。
func (s *Server) oidcTakePending(state string) (oidcPendingState, bool) {
	if state == "" {
		return oidcPendingState{}, false
	}
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	pending, ok := s.oidcPending[state]
	if !ok {
		return oidcPendingState{}, false
	}
	delete(s.oidcPending, state)
	if time.Since(pending.CreatedAt) > oidcPendingTTL {
		return oidcPendingState{}, false
	}
	return pending, true
}

// storeOIDCBinding 持久化绑定关系并刷新内存缓存。
func (s *Server) storeOIDCBinding(ctx context.Context, binding oidc.Binding) error {
	raw, err := oidc.MarshalBinding(binding)
	if err != nil {
		return err
	}
	if err := s.store.UpsertAppSetting(ctx, store.AppSetting{
		Key:   oidc.SettingBindingKey,
		Value: raw,
	}); err != nil {
		return err
	}
	s.oidcMu.Lock()
	s.oidcBinding = binding
	s.oidcBound = true
	s.oidcMu.Unlock()
	return nil
}

// clearOIDCBinding 删除持久化绑定关系并刷新内存缓存。
func (s *Server) clearOIDCBinding(ctx context.Context) error {
	err := s.store.DeleteAppSetting(ctx, oidc.SettingBindingKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.oidcMu.Lock()
	s.oidcBinding = oidc.Binding{}
	s.oidcBound = false
	s.oidcMu.Unlock()
	return nil
}

// oidcSecureRequest 判断当前请求是否可视为 HTTPS（用于 callback URL 选择）。
func (s *Server) oidcSecureRequest(r *http.Request) bool {
	return r.TLS != nil || s.secureCookies
}

// oidcRequestAuthenticated 仅做会话有效性判断，不写响应。
func (s *Server) oidcRequestAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	_, err = s.auth.Authenticate(r.Context(), cookie.Value)
	return err == nil
}

// sanitizeOIDCRedirectPath 只接受站内相对路径，防止开放重定向。
func sanitizeOIDCRedirectPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") {
		return "/"
	}
	if strings.HasPrefix(value, "//") {
		return "/"
	}
	if strings.ContainsAny(value, ":\\\r\n\t") {
		return "/"
	}
	return value
}

// oidcBindingConflict 判断新的 OIDC 身份是否与现有绑定冲突。
// issuer 变更视为旧绑定失效（允许重新绑定），同 issuer 下 subject 不一致必须拒绝。
func oidcBindingConflict(identity oidc.Identity, binding oidc.Binding) bool {
	if strings.TrimSpace(binding.Subject) == "" {
		return false
	}
	boundIssuer := strings.TrimSpace(binding.Issuer)
	identityIssuer := strings.TrimSpace(identity.Issuer)
	if boundIssuer != "" && !strings.EqualFold(boundIssuer, identityIssuer) {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(binding.Subject), strings.TrimSpace(identity.Subject))
}

// handleOIDCStatus 是登录页读取的公开状态接口。
func (s *Server) handleOIDCStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	snapshot := s.oidcState()
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"enabled":           snapshot.enabled(),
			"bound":             snapshot.bindingValid(),
			"login_button_text": oidcLoginButtonText(snapshot.Config),
			"login_url":         oidcLoginURL,
		},
	})
}

// handleOIDCLogin 发起授权码流程：302 到 IDP 授权端点。
// mode=login（缺省）用于登录页；mode=bind 用于绑定弹窗。
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "" {
		mode = "login"
	}
	if mode != "login" && mode != "bind" {
		writeError(w, http.StatusBadRequest, "invalid_oidc_mode", "mode 只能为 login 或 bind")
		return
	}
	// 绑定模式会写入管理员绑定关系，必须由已登录管理员发起，
	// 否则匿名用户可以抢先把自己的 OIDC 身份绑成管理员，再走 OIDC 登录。
	if mode == "bind" && !s.oidcRequestAuthenticated(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
		return
	}
	redirectPath := "/"
	if mode == "login" {
		redirectPath = sanitizeOIDCRedirectPath(r.URL.Query().Get("redirect"))
	}
	snapshot := s.oidcState()
	if !snapshot.enabled() {
		writeError(w, http.StatusServiceUnavailable, "oidc_unavailable", "OIDC 未启用或端点尚未就绪")
		return
	}
	callbackURL := oidc.SelectCallbackURL(
		snapshot.Config.CallbackURLs,
		r.Host,
		s.oidcSecureRequest(r),
	)
	authorization, err := oidc.NewAuthorizationRequest(
		snapshot.Metadata,
		snapshot.Config,
		callbackURL,
		redirectPath,
		mode,
	)
	if err != nil {
		s.logger.Error("构建 OIDC 授权请求失败", "error", err)
		writeError(w, http.StatusInternalServerError, "oidc_authorization_failed", err.Error())
		return
	}
	s.oidcStorePending(authorization.State, oidcPendingState{
		Nonce:        authorization.Nonce,
		CodeVerifier: authorization.CodeVerifier,
		RedirectURI:  authorization.RedirectURI,
		RedirectPath: redirectPath,
		Mode:         mode,
		CreatedAt:    time.Now(),
	})
	http.Redirect(w, r, authorization.URL, http.StatusFound)
}

// handleOIDCLoginStart 是登录页弹窗登录的公开发起接口：返回一次性授权地址，
// 由前端写入居中弹出窗口完成 IDP 授权；回调在弹窗内返回结果页并通知 opener。
func (s *Server) handleOIDCLoginStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		Redirect string `json:"redirect"`
	}
	// 允许空 body：redirect 缺失时按 "/" 处理。
	if r.ContentLength != 0 {
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	redirectPath := sanitizeOIDCRedirectPath(request.Redirect)
	snapshot := s.oidcState()
	if !snapshot.enabled() {
		writeError(w, http.StatusBadRequest, "oidc_unavailable", "OIDC 未启用或端点尚未就绪")
		return
	}
	callbackURL := oidc.SelectCallbackURL(
		snapshot.Config.CallbackURLs,
		r.Host,
		s.oidcSecureRequest(r),
	)
	authorization, err := oidc.NewAuthorizationRequest(
		snapshot.Metadata,
		snapshot.Config,
		callbackURL,
		redirectPath,
		"login",
	)
	if err != nil {
		s.logger.Error("构建 OIDC 弹窗登录授权请求失败", "error", err)
		writeError(w, http.StatusInternalServerError, "oidc_authorization_failed", err.Error())
		return
	}
	s.oidcStorePending(authorization.State, oidcPendingState{
		Nonce:        authorization.Nonce,
		CodeVerifier: authorization.CodeVerifier,
		RedirectURI:  authorization.RedirectURI,
		RedirectPath: redirectPath,
		Mode:         oidcLoginPopupMode,
		CreatedAt:    time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"authorization_url": authorization.URL,
			"mode":              oidcLoginPopupMode,
			"redirect":          redirectPath,
		},
	})
}

// handleOIDCCallback 处理 IDP 回调：登录模式签发会话并 302 回站内（或弹窗模式返回结果页），
// 绑定模式写入绑定关系并返回带 postMessage 的结果页。
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	query := r.URL.Query()
	pending, ok := s.oidcTakePending(strings.TrimSpace(query.Get("state")))
	if !ok {
		s.oidcCallbackFailure(w, r, "", "OIDC 授权状态已失效，请重新发起")
		return
	}
	if idpError := strings.TrimSpace(query.Get("error")); idpError != "" {
		message := "IDP 返回错误：" + idpError
		if description := strings.TrimSpace(query.Get("error_description")); description != "" {
			message += "（" + description + "）"
		}
		s.oidcCallbackFailure(w, r, pending.Mode, message)
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		s.oidcCallbackFailure(w, r, pending.Mode, "回调缺少 code 参数")
		return
	}
	snapshot := s.oidcState()
	if !snapshot.Discovered {
		s.oidcCallbackFailure(w, r, pending.Mode, "OIDC 端点尚未就绪，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), oidcRequestTimeout)
	defer cancel()
	tokens, err := oidc.Exchange(
		ctx,
		s.oidcClient(),
		snapshot.Metadata,
		snapshot.Config,
		code,
		pending.RedirectURI,
		pending.CodeVerifier,
	)
	if err != nil {
		s.oidcCallbackFailure(w, r, pending.Mode, err.Error())
		return
	}
	identity, err := oidc.ResolveIdentity(ctx, s.oidcClient(), snapshot.Metadata, tokens)
	if err != nil {
		s.oidcCallbackFailure(w, r, pending.Mode, err.Error())
		return
	}
	if pending.Mode == "bind" {
		s.oidcCompleteBinding(w, r, identity, snapshot)
		return
	}
	s.oidcCompleteLogin(w, r, pending, identity, snapshot)
}

// oidcCallbackFailure 按模式输出失败结果：弹窗登录返回登录失败结果页，
// 整页跳转登录回登录页，绑定模式（含未知 state 的兜底）返回绑定失败结果页。
func (s *Server) oidcCallbackFailure(w http.ResponseWriter, r *http.Request, mode string, message string) {
	switch mode {
	case oidcLoginPopupMode:
		s.recordAudit(
			r.Context(),
			"admin",
			"auth.oidc.login",
			"session",
			"admin",
			"failure",
			requestRemoteHost(r),
		)
		writeOIDCLoginResult(w, false, message)
	case "login":
		s.recordAudit(
			r.Context(),
			"admin",
			"auth.oidc.login",
			"session",
			"admin",
			"failure",
			requestRemoteHost(r),
		)
		http.Redirect(w, r, "/login", http.StatusFound)
	default:
		s.recordAudit(
			r.Context(),
			"admin",
			"settings.oidc.binding.bind",
			"settings",
			"oidc",
			"failure",
			requestRemoteHost(r),
		)
		writeOIDCBindResult(w, false, message)
	}
}

// oidcCompleteBinding 处理绑定模式回调：校验身份后写入绑定关系。
func (s *Server) oidcCompleteBinding(
	w http.ResponseWriter,
	r *http.Request,
	identity oidc.Identity,
	snapshot oidcSnapshot,
) {
	if snapshot.Bound && oidcBindingConflict(identity, snapshot.Binding) {
		s.recordAudit(
			r.Context(),
			"admin",
			"settings.oidc.binding.bind",
			"settings",
			"oidc",
			"failure",
			requestRemoteHost(r),
		)
		writeOIDCBindResult(w, false, oidc.ErrIdentityMismatch.Error())
		return
	}
	binding := oidc.Binding{
		Issuer:  identity.Issuer,
		Subject: identity.Subject,
		Email:   identity.Email,
		Name:    identity.Name,
		BoundAt: time.Now().UTC(),
	}
	if err := s.storeOIDCBinding(r.Context(), binding); err != nil {
		s.logger.Error("保存 OIDC 绑定关系失败", "error", err)
		s.recordAudit(
			r.Context(),
			"admin",
			"settings.oidc.binding.bind",
			"settings",
			"oidc",
			"failure",
			requestRemoteHost(r),
		)
		writeOIDCBindResult(w, false, "保存绑定关系失败，请稍后重试")
		return
	}
	s.recordAudit(
		r.Context(),
		"admin",
		"settings.oidc.binding.bind",
		"settings",
		"oidc",
		"success",
		requestRemoteHost(r),
	)
	writeOIDCBindResult(w, true, "")
}

// oidcCompleteLogin 处理登录回调：校验绑定身份后用已确认的管理员签发会话。
// 弹窗登录模式返回结果页并通知 opener，整页跳转模式仍 302 回站内。
func (s *Server) oidcCompleteLogin(
	w http.ResponseWriter,
	r *http.Request,
	pending oidcPendingState,
	identity oidc.Identity,
	snapshot oidcSnapshot,
) {
	fail := func(message string) {
		s.oidcCallbackFailure(w, r, pending.Mode, message)
	}
	if !snapshot.bindingValid() {
		fail(oidc.ErrNotBound.Error())
		return
	}
	if err := oidc.VerifyBinding(identity, snapshot.Binding); err != nil {
		fail(err.Error())
		return
	}
	admin, err := s.store.CurrentAdmin(r.Context())
	if err != nil {
		s.logger.Error("读取管理员账号失败", "error", err)
		fail("读取管理员账号失败")
		return
	}
	credentials, err := s.auth.IssueSession(r.Context(), admin)
	if err != nil {
		s.logger.Error("OIDC 登录签发会话失败", "error", err)
		fail("签发登录会话失败")
		return
	}
	s.recordAudit(
		r.Context(),
		credentials.Principal.Username,
		"auth.oidc.login",
		"session",
		credentials.Principal.Username,
		"success",
		requestRemoteHost(r),
	)
	s.setAuthCookies(w, credentials.SessionToken, credentials.CSRFToken, credentials.ExpiresAt)
	if pending.Mode == oidcLoginPopupMode {
		writeOIDCLoginResult(w, true, "")
		return
	}
	http.Redirect(w, r, sanitizeOIDCRedirectPath(pending.RedirectPath), http.StatusFound)
}

// handleOIDCSettings 读写 OIDC 配置（需登录）。
func (s *Server) handleOIDCSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": s.oidcSettingsPayload()})
	case http.MethodPut:
		s.saveOIDCSettings(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// saveOIDCSettings 保存配置并立即重新发现端点。
func (s *Server) saveOIDCSettings(w http.ResponseWriter, r *http.Request) {
	var request oidc.Config
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	current := s.oidcState().Config
	merged := oidc.MergeConfigSecret(request, current)
	normalized, err := oidc.NormalizeConfig(merged)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_config", err.Error())
		return
	}
	if normalized.Enabled && !oidcConfigComplete(normalized) {
		writeError(w, http.StatusBadRequest, "invalid_oidc_config", oidc.ErrNotConfigured.Error())
		return
	}

	// 保存后立即重新发现端点：启用时发现失败视为配置不可用，直接拒绝保存；
	// 未启用时失败只记录 discovery.error，不阻断保存。
	var metadata oidc.Metadata
	var discoveryErr error
	if strings.TrimSpace(normalized.WellKnownURL) != "" {
		ctx, cancel := context.WithTimeout(r.Context(), oidcRequestTimeout)
		defer cancel()
		metadata, discoveryErr = oidc.Discover(ctx, s.oidcClient(), normalized.WellKnownURL)
		if discoveryErr != nil && normalized.Enabled {
			writeError(w, http.StatusBadRequest, "oidc_discovery_failed", discoveryErr.Error())
			return
		}
	}

	raw, err := oidc.MarshalConfig(normalized)
	if err != nil {
		s.logger.Error("编码 OIDC 配置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		return
	}
	if err := s.store.UpsertAppSetting(r.Context(), store.AppSetting{
		Key:   oidc.SettingConfigKey,
		Value: raw,
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.oidcMu.Lock()
	s.oidcConfig = normalized
	s.oidcMu.Unlock()
	s.updateOIDCDiscovery(metadata, discoveryErr)
	s.audit(r, "settings.oidc.update", "settings", "oidc", "success")
	writeJSON(w, http.StatusOK, map[string]any{"data": s.oidcSettingsPayload()})
}

// handleOIDCDiscover 仅做 Well-known 发现预检，供设置表单自动填充端点。
func (s *Server) handleOIDCDiscover(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		WellKnownURL string `json:"well_known_url"`
	}
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	wellKnownURL := strings.TrimSpace(request.WellKnownURL)
	if wellKnownURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_well_known_url", "well_known_url 不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), oidcRequestTimeout)
	defer cancel()
	metadata, err := oidc.Discover(ctx, s.oidcClient(), wellKnownURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oidc_discovery_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": metadata})
}

// handleOIDCBinding 管理管理员与 OIDC 身份的一对一绑定关系（需登录）。
func (s *Server) handleOIDCBinding(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": s.oidcBindingPayload()})
	case http.MethodPost:
		s.startOIDCBinding(w, r)
	case http.MethodDelete:
		if err := s.clearOIDCBinding(r.Context()); err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.audit(r, "settings.oidc.binding.delete", "settings", "oidc", "success")
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"bound": false}})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// startOIDCBinding 发起绑定流程，返回 IDP 授权地址。
func (s *Server) startOIDCBinding(w http.ResponseWriter, r *http.Request) {
	snapshot := s.oidcState()
	if !snapshot.enabled() {
		writeError(w, http.StatusBadRequest, "oidc_unavailable", "OIDC 未启用或端点尚未就绪")
		return
	}
	callbackURL := oidc.SelectCallbackURL(
		snapshot.Config.CallbackURLs,
		r.Host,
		s.oidcSecureRequest(r),
	)
	authorization, err := oidc.NewAuthorizationRequest(
		snapshot.Metadata,
		snapshot.Config,
		callbackURL,
		"",
		"bind",
	)
	if err != nil {
		s.logger.Error("构建 OIDC 绑定授权请求失败", "error", err)
		writeError(w, http.StatusInternalServerError, "oidc_authorization_failed", err.Error())
		return
	}
	s.oidcStorePending(authorization.State, oidcPendingState{
		Nonce:        authorization.Nonce,
		CodeVerifier: authorization.CodeVerifier,
		RedirectURI:  authorization.RedirectURI,
		RedirectPath: "",
		Mode:         "bind",
		CreatedAt:    time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"authorization_url": authorization.URL,
			"mode":              "bind",
		},
	})
}

// oidcSettingsPayload 组装设置页需要的完整配置视图（secret 脱敏）。
func (s *Server) oidcSettingsPayload() map[string]any {
	snapshot := s.oidcState()
	redacted := oidc.RedactConfig(snapshot.Config)
	scopes := redacted.Scopes
	if len(scopes) == 0 {
		scopes = append([]string(nil), oidc.DefaultScopes...)
	}
	callbackURLs := redacted.CallbackURLs
	if callbackURLs == nil {
		callbackURLs = []string{}
	}
	var binding any
	if snapshot.Bound {
		binding = snapshot.Binding
	}
	return map[string]any{
		"enabled":                   snapshot.Config.Enabled,
		"well_known_url":            redacted.WellKnownURL,
		"client_id":                 redacted.ClientID,
		"client_secret":             redacted.ClientSecret,
		"scopes":                    scopes,
		"callback_urls":             callbackURLs,
		"login_button_text":         oidcLoginButtonText(snapshot.Config),
		"callback_path":             oidc.DefaultCallbackPath,
		"default_scopes":            oidc.DefaultScopes,
		"default_login_button_text": oidc.DefaultLoginButtonText,
		"configured":                oidcConfigComplete(snapshot.Config),
		"discovery":                 oidcDiscoveryPayload(snapshot),
		"binding":                   binding,
	}
}

// oidcDiscoveryPayload 返回缓存端点；未发现但有错误时返回 {"error": ...}，否则为 null。
func oidcDiscoveryPayload(snapshot oidcSnapshot) any {
	if snapshot.Discovered {
		return snapshot.Metadata
	}
	if snapshot.DiscoveryError != "" {
		return map[string]any{"error": snapshot.DiscoveryError}
	}
	return nil
}

// oidcBindingPayload 返回绑定状态视图。
func (s *Server) oidcBindingPayload() map[string]any {
	snapshot := s.oidcState()
	var binding any
	if snapshot.Bound {
		binding = snapshot.Binding
	}
	return map[string]any{
		"enabled":           snapshot.enabled(),
		"bound":             snapshot.bindingValid(),
		"binding":           binding,
		"login_button_text": oidcLoginButtonText(snapshot.Config),
	}
}

// oidcResultPageTemplate 是 OIDC 授权弹窗（绑定/登录）的结果页：在弹出窗口内渲染后，
// 通过 postMessage 通知 opener（站内页面），并按需关闭弹出窗口。
const oidcResultPageTemplate = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<title>%s</title>
</head>
<body>
<p>%s</p>
<script>
(function () {
  var message = %s;
  var closeWhenDone = %t;
  try {
    if (window.parent && window.parent !== window) {
      window.parent.postMessage(message, "*");
    }
    if (window.opener) {
      window.opener.postMessage(message, "*");
      if (closeWhenDone) {
        window.close();
      }
    }
  } catch (error) {
    // 忽略跨窗口通信异常，页面本身仍展示结果。
  }
})();
</script>
</body>
</html>
`

// oidcResultPage 描述一张 OIDC 弹窗结果页：标题、正文、postMessage 载荷与 HTTP 状态码。
type oidcResultPage struct {
	Title         string
	Text          string
	Message       map[string]any
	CloseOnOpener bool
	Status        int
}

// writeOIDCResultPage 输出通用结果页，绑定与登录两种弹窗共用。
func writeOIDCResultPage(w http.ResponseWriter, page oidcResultPage) {
	encoded, err := json.Marshal(page.Message)
	if err != nil {
		encoded = []byte(`{"type":"vocat:oidc-error","message":"操作失败"}`)
	}
	// 避免 message 中出现 </script> 提前闭合脚本标签。
	script := strings.ReplaceAll(string(encoded), "</", "<\\/")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(page.Status)
	_, _ = fmt.Fprintf(
		w,
		oidcResultPageTemplate,
		html.EscapeString(page.Title),
		html.EscapeString(page.Text),
		script,
		page.CloseOnOpener,
	)
}

// writeOIDCBindResult 输出绑定结果页（成功/失败），行为与消息格式保持兼容。
func writeOIDCBindResult(w http.ResponseWriter, success bool, detail string) {
	if success {
		writeOIDCResultPage(w, oidcResultPage{
			Title:         "OIDC 绑定成功",
			Text:          "绑定成功，可以关闭此窗口",
			Message:       map[string]any{"type": "vocat:oidc-bound"},
			CloseOnOpener: true,
			Status:        http.StatusOK,
		})
		return
	}
	writeOIDCResultPage(w, oidcResultPage{
		Title:         "OIDC 绑定失败",
		Text:          "绑定失败：" + detail,
		Message:       map[string]any{"type": "vocat:oidc-error", "message": detail},
		CloseOnOpener: true,
		Status:        http.StatusBadRequest,
	})
}

// writeOIDCLoginResult 输出登录弹窗结果页（成功/失败）。
// 两种结果都会在 postMessage 后关闭弹出窗口：成功/失败提示统一由站内弹窗展示，
// 避免用户还要在 OIDC 弹出窗口里阅读结果。
func writeOIDCLoginResult(w http.ResponseWriter, success bool, detail string) {
	if success {
		writeOIDCResultPage(w, oidcResultPage{
			Title:         "OIDC 登录成功",
			Text:          "登录成功，可以关闭此窗口",
			Message:       map[string]any{"type": "vocat:oidc-login"},
			CloseOnOpener: true,
			Status:        http.StatusOK,
		})
		return
	}
	writeOIDCResultPage(w, oidcResultPage{
		Title:         "OIDC 登录失败",
		Text:          "登录失败：" + detail,
		Message:       map[string]any{"type": "vocat:oidc-error", "message": detail},
		CloseOnOpener: true,
		Status:        http.StatusBadRequest,
	})
}
