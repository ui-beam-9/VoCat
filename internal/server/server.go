package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"vocat/internal/auth"
	"vocat/internal/device"
	"vocat/internal/exportproxy"
	"vocat/internal/extensions"
	"vocat/internal/httpsmode"
	"vocat/internal/loghub"
	"vocat/internal/oidc"
	"vocat/internal/sipgw"
	"vocat/internal/store"
	"vocat/internal/update"
	"vocat/internal/voicegateway"
	"vocat/internal/vowifi"
)

const (
	sessionCookieName = "vocat_session"
	csrfCookieName    = "vocat_csrf"
	csrfHeaderName    = "X-CSRF-Token"
)

type Options struct {
	Store               *store.Store
	Auth                *auth.Service
	Devices             DeviceController
	VoWiFi              VoWiFiController
	VoiceGateway        VoiceGatewayController
	Logs                *loghub.Hub
	Assets              fs.FS
	Logger              *slog.Logger
	SecureCookies       bool
	MaxRequestBodyBytes int64
	Extensions          *extensions.Manager
	ExportProxy         *exportproxy.Manager
	DeveloperEnabled    bool
	UpdateRepository    string
	UpdateToken         string
	HTTPS               *httpsmode.Manager
	// RingtoneFile 为自定义来电铃声文件的存储路径（数据目录下固定文件名）。
	RingtoneFile string
}

// Server is the single HTTP handler for the JSON API and embedded SPA.
type Server struct {
	store                     *store.Store
	auth                      *auth.Service
	devices                   DeviceController
	vowifi                    VoWiFiController
	voiceGateway              VoiceGatewayController
	ussdSessions              ussdSessionStore
	logs                      *loghub.Hub
	assets                    fs.FS
	indexHTML                 []byte
	fileServer                http.Handler
	logger                    *slog.Logger
	secureCookies             bool
	maxRequestBodyBytes       int64
	startedAt                 time.Time
	handler                   http.Handler
	websheets                 *websheetManager
	accessMu                  sync.RWMutex
	access                    parsedAccessConfig
	loginLimiter              *loginRateLimiter
	extensions                *extensions.Manager
	exportProxy               *exportproxy.Manager
	developerEnabled          bool
	updateRepository          string
	updateToken               string
	updateCheck               func(context.Context, string, string, string) (update.CheckResult, error)
	updateApply               func(context.Context, *slog.Logger, update.Options, bool) (update.CheckResult, error)
	updateRestart             func(*slog.Logger) error
	updateMu                  sync.Mutex
	updateApplying            bool
	https                     *httpsmode.Manager
	netTraffic                *liveNetTracker
	hostStats                 *hostStatsSampler
	publicIPMu                sync.RWMutex
	publicIPs                 map[string]cachedPublicIP
	lookupPublicIP            func(context.Context, string) (exportproxy.PublicIPInfo, error)
	automaticTasks            *automaticTaskScheduler
	smsSyncMu                 sync.Mutex
	esimSIMResetAttempted     map[string]bool // 由 smsSyncMu 保护；读取成功后允许下一次故障恢复。
	smsStorageMu              sync.Mutex
	smsStorage                map[string]device.SMSStorageUsage
	cellularDataOnce          sync.Once
	cellularDataMonitorOnce   sync.Once
	cellularDataEventOnce     sync.Once
	cellularDataLifecycleOnce sync.Once
	cellularData              *cellularDataRuntime
	phoneInfoMu               sync.RWMutex
	phoneInfoAPIKey           string
	phoneInfoCache            map[string]phoneInfoCacheEntry
	ringtoneFile              string
	// OIDC 登录/绑定：内存缓存配置、已发现端点、绑定关系与待处理授权状态。
	oidcMu             sync.RWMutex
	oidcConfig         oidc.Config
	oidcMetadata       oidc.Metadata
	oidcDiscovered     bool
	oidcDiscoveryError string
	oidcBinding        oidc.Binding
	oidcBound          bool
	oidcPending        map[string]oidcPendingState
	oidcHTTPClient     *http.Client
}

func New(options Options) (*Server, error) {
	if options.Store == nil {
		return nil, errors.New("server: store is required")
	}
	if options.Auth == nil {
		return nil, errors.New("server: auth service is required")
	}
	if options.Assets == nil {
		return nil, errors.New("server: SPA assets are required")
	}
	indexHTML, err := fs.ReadFile(options.Assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("server: read embedded index.html: %w", err)
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.MaxRequestBodyBytes <= 0 {
		options.MaxRequestBodyBytes = 1 << 20
	}
	if strings.TrimSpace(options.UpdateRepository) == "" {
		options.UpdateRepository = update.DefaultRepository
	}

	server := &Server{
		store:               options.Store,
		auth:                options.Auth,
		devices:             options.Devices,
		vowifi:              options.VoWiFi,
		voiceGateway:        options.VoiceGateway,
		ussdSessions:        newUSSDSessionStore(),
		logs:                options.Logs,
		assets:              options.Assets,
		indexHTML:           indexHTML,
		fileServer:          http.FileServer(http.FS(options.Assets)),
		logger:              options.Logger,
		secureCookies:       options.SecureCookies,
		maxRequestBodyBytes: options.MaxRequestBodyBytes,
		startedAt:           time.Now().UTC(),
		websheets:           newWebsheetManager(),
		loginLimiter:        newLoginRateLimiter(),
		extensions:          options.Extensions,
		exportProxy:         options.ExportProxy,
		developerEnabled:    options.DeveloperEnabled,
		updateRepository:    strings.TrimSpace(options.UpdateRepository),
		updateToken:         strings.TrimSpace(options.UpdateToken),
		https:               options.HTTPS,
		ringtoneFile:        strings.TrimSpace(options.RingtoneFile),
		netTraffic:          newLiveNetTracker(),
		hostStats:           newHostStatsSampler(),
		publicIPs:           make(map[string]cachedPublicIP),
		smsStorage:          make(map[string]device.SMSStorageUsage),
		lookupPublicIP:      exportproxy.LookupPublicIP,
		updateCheck:         update.CheckLatest,
		updateApply:         update.ApplyLatest,
		updateRestart:       update.RestartService,
		phoneInfoCache:      make(map[string]phoneInfoCacheEntry),
		oidcPending:         make(map[string]oidcPendingState),
		oidcHTTPClient:      newOIDCHTTPClient(),
	}
	server.cellularDataRuntime()
	server.loadAccessConfig(context.Background())
	server.loadUILanguage(context.Background())
	server.loadPhoneInfoAPIKey(context.Background())
	server.loadLoggingConfig(context.Background())
	server.loadOIDCConfig(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleLiveness)
	mux.HandleFunc("/readyz", server.handleReadiness)
	mux.HandleFunc("/metrics", server.handleMetrics)
	mux.HandleFunc("/api/health", server.handleHealth)
	mux.HandleFunc("/api/auth/login", server.handleLogin)
	mux.HandleFunc("/api/auth/session", server.handleSession)
	mux.HandleFunc("/api/auth/logout", server.handleLogout)
	// OIDC 公开接口：登录页与绑定弹窗在未登录/无会话 Cookie 场景下也要能访问，
	// 因此必须绕过 requireAuthenticated 直接注册在 mux 上。
	mux.HandleFunc("/api/auth/oidc/status", server.handleOIDCStatus)
	mux.HandleFunc("/api/auth/oidc/login", server.handleOIDCLogin)
	// 弹窗登录需要先取一次性授权地址（POST），与整页跳转的 /api/auth/oidc/login 精确路径互不冲突。
	mux.HandleFunc("/api/auth/oidc/login/start", server.handleOIDCLoginStart)
	mux.HandleFunc("/api/auth/oidc/callback", server.handleOIDCCallback)
	mux.HandleFunc("/api", server.handleAPI)
	mux.HandleFunc("/api/", server.handleAPI)
	mux.HandleFunc("/websheets/", server.handleWebsheet)
	mux.HandleFunc("/plugin-assets/", server.handlePluginAsset)
	mux.HandleFunc("/", server.handleSPA)

	server.handler = server.recoverPanics(
		server.securityHeaders(server.accessControl(server.logUserOperation(mux))),
	)
	return server, nil
}

// VoWiFiController is the asynchronous runtime boundary used by the HTTP
// layer. State transitions continue after the request completes and are
// surfaced by the normal device status endpoints.
type VoWiFiController interface {
	State(string) (vowifi.State, error)
	RequestEnabled(string, bool) (vowifi.State, error)
	RequestReconnect(string) (vowifi.State, error)
}

type VoWiFiSMSSyncController interface {
	ModemSMSSyncBlocked(string) bool
}

type VoWiFiMaintenanceController interface {
	BeginMaintenance(string) error
	EndMaintenance(string)
}

type VoWiFiUnregisterController interface {
	Unregister(context.Context, string) error
}

type VoWiFiCallController interface {
	Calls(string) ([]vowifi.Call, error)
	DialCall(context.Context, string, string) (vowifi.Call, error)
	AnswerCall(context.Context, string, string) (vowifi.Call, error)
	HangupCall(context.Context, string, string) error
	SendDTMF(context.Context, string, string, string) error
	SetCallHold(context.Context, string, string, bool) (vowifi.Call, error)
}

type VoWiFiCallMediaController interface {
	CallMedia(context.Context, string, string) (vowifi.CallMedia, error)
}

// VoiceGatewayController 是 CS 域语音网关（SIP/软电话桥接）的配置与状态边界。
type VoiceGatewayController interface {
	Redacted(context.Context) (sipgw.Config, error)
	Save(context.Context, sipgw.Config) (sipgw.Config, error)
	Status(context.Context) voicegateway.Status
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ready(ctx); err != nil {
		s.logger.Error("health check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"status":   "ok",
			"database": "ok",
			"time":     time.Now().UTC().Format(time.RFC3339),
		},
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	limiterKey := s.loginKey(r, request.Username)
	if retryAfter, locked := s.loginLimiter.checkLocked(limiterKey); locked {
		s.auditAuth(r, request.Username, "locked")
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed login attempts; please try again later")
		return
	}

	credentials, err := s.auth.Login(r.Context(), request.Username, request.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		lockout, newlyLocked := s.loginLimiter.recordFailure(limiterKey)
		s.auditAuth(r, request.Username, "failure")
		if newlyLocked {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(lockout.Seconds())))
			writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed login attempts; please try again later")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	if err != nil {
		s.logger.Error("login failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		return
	}

	s.loginLimiter.recordSuccess(limiterKey)
	s.auditAuth(r, credentials.Principal.Username, "success")
	s.setAuthCookies(w, credentials.SessionToken, credentials.CSRFToken, credentials.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"user":          credentials.Principal,
			"csrf_token":    credentials.CSRFToken,
			"expires_at":    credentials.ExpiresAt.Format(time.RFC3339),
			"authenticated": true,
			"status":        "ok",
		},
	})
}

// loginKey builds the rate-limit key from the client address and username so
// brute-force attempts against one account from one source are throttled.
func (s *Server) loginKey(r *http.Request, username string) string {
	address := s.currentAccessConfig().clientIP(r)
	return address.String() + "|" + strings.ToLower(strings.TrimSpace(username))
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	sessionToken, ok := s.sessionToken(w, r)
	if !ok {
		return
	}
	existingCSRF := ""
	if cookie, cookieErr := r.Cookie(csrfCookieName); cookieErr == nil {
		existingCSRF = cookie.Value
	}
	session, csrfToken, err := s.auth.CSRFToken(r.Context(), sessionToken, existingCSRF)
	if errors.Is(err, auth.ErrUnauthorized) {
		s.authenticationRequired(w, r)
		return
	}
	if err != nil {
		s.logger.Error("load session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		return
	}
	s.setCSRFCookie(w, csrfToken, session.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"user":          session.Principal,
			"csrf_token":    csrfToken,
			"expires_at":    session.ExpiresAt.Format(time.RFC3339),
			"authenticated": true,
		},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	sessionToken, ok := s.sessionToken(w, r)
	if !ok {
		return
	}
	csrfToken, ok := s.validateDoubleSubmitCSRF(w, r)
	if !ok {
		return
	}
	if _, err := s.auth.ValidateCSRF(r.Context(), sessionToken, csrfToken); err != nil {
		switch {
		case errors.Is(err, auth.ErrUnauthorized):
			s.authenticationRequired(w, r)
		case errors.Is(err, auth.ErrInvalidCSRF):
			writeError(w, http.StatusForbidden, "invalid_csrf", "CSRF validation failed")
		default:
			s.logger.Error("logout validation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		}
		return
	}
	if err := s.auth.Logout(r.Context(), sessionToken); err != nil {
		s.logger.Error("logout failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		return
	}
	s.clearAuthCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]bool{"logged_out": true},
	})
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "API endpoint not found")
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	// The UI language preference is not sensitive; exposing the read side lets
	// the login page render in the persisted language before authentication.
	if r.Method == http.MethodGet &&
		strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/") == "settings/preferences" {
		s.writeUIPreferences(w, r)
		return
	}
	if !s.requireAuthenticated(w, r) {
		return
	}
	if r.Method != http.MethodGet &&
		r.Method != http.MethodHead &&
		r.Method != http.MethodOptions {
		sessionToken, ok := s.sessionToken(w, r)
		if !ok {
			return
		}
		csrfToken, ok := s.validateDoubleSubmitCSRF(w, r)
		if !ok {
			return
		}
		if _, err := s.auth.ValidateCSRF(r.Context(), sessionToken, csrfToken); err != nil {
			switch {
			case errors.Is(err, auth.ErrUnauthorized):
				s.authenticationRequired(w, r)
			case errors.Is(err, auth.ErrInvalidCSRF):
				writeError(w, http.StatusForbidden, "invalid_csrf", "CSRF validation failed")
			default:
				s.logger.Error("API CSRF validation failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			}
			return
		}
	}
	if s.routeDeviceAPI(w, r) {
		return
	}
	if s.routeGeneralAPI(w, r) {
		return
	}
	s.handleAPINotFound(w, r)
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name != "." && fs.ValidPath(name) {
		if info, err := fs.Stat(s.assets, name); err == nil && info.Mode().IsRegular() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			s.fileServer.ServeHTTP(w, r)
			return
		}
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", mime.TypeByExtension(".html"))
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.indexHTML))
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil ||
			(mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return errors.New("Content-Type must be application/json")
		}
	}

	// MaxBytesReader's ResponseWriter parameter is deprecated and unused by Go.
	// Passing nil also makes the request body and response data flows explicitly
	// separate for static analysis.
	r.Body = http.MaxBytesReader(nil, r.Body, s.maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return fmt.Errorf("request body exceeds %d bytes", s.maxRequestBodyBytes)
		}
		return errors.New("request body must contain one valid JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one valid JSON object")
	}
	return nil
}

func (s *Server) sessionToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		s.authenticationRequired(w, r)
		return "", false
	}
	return cookie.Value, true
}

func (s *Server) requireAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	sessionToken, ok := s.sessionToken(w, r)
	if !ok {
		return false
	}
	if _, err := s.auth.Authenticate(r.Context(), sessionToken); err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			s.authenticationRequired(w, r)
		} else {
			s.logger.Error("request authentication failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
		}
		return false
	}
	return true
}

// authenticationRequired preserves JSON semantics for API clients while
// making a direct browser navigation land on the login screen instead of a
// raw {"error":...} document. Frontend fetches explicitly request JSON and
// are handled by the shared vocat:unauthorized event.
func (s *Server) authenticationRequired(w http.ResponseWriter, r *http.Request) {
	s.clearAuthCookies(w)
	w.Header().Set("Cache-Control", "no-store")
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "authentication is required")
}

func (s *Server) validateDoubleSubmitCSRF(w http.ResponseWriter, r *http.Request) (string, bool) {
	headerToken := r.Header.Get(csrfHeaderName)
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || headerToken == "" || cookie.Value == "" ||
		subtle.ConstantTimeCompare([]byte(headerToken), []byte(cookie.Value)) != 1 {
		writeError(w, http.StatusForbidden, "invalid_csrf", "CSRF validation failed")
		return "", false
	}
	return headerToken, true
}

func (s *Server) setAuthCookies(w http.ResponseWriter, sessionToken string, csrfToken string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	s.setCSRFCookie(w, csrfToken, expiresAt)
}

func (s *Server) setCSRFCookie(w http.ResponseWriter, csrfToken string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrfToken,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: false,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			Expires:  time.Unix(1, 0),
			MaxAge:   -1,
			HttpOnly: name == sessionCookieName,
			Secure:   s.secureCookies,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

func requireMethod(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}

type operationStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *operationStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *operationStatusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// logUserOperation records state-changing API actions, not request traffic.
// GET/HEAD polling, assets, health checks and the live log stream are never
// emitted, keeping the diagnostic page focused on actions a user initiated.
func (s *Server) logUserOperation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions ||
			strings.HasPrefix(r.URL.Path, "/api/auth/") || strings.HasPrefix(r.URL.Path, "/api/logs/") {
			next.ServeHTTP(w, r)
			return
		}
		writer := &operationStatusWriter{ResponseWriter: w}
		next.ServeHTTP(writer, r)
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		outcome := "success"
		message := "user operation completed"
		if status >= http.StatusBadRequest {
			level = slog.LevelWarn
			outcome = "failed"
			message = "user operation failed"
		}
		s.logger.Log(r.Context(), level, message,
			"category", operationPathCategory(r.URL.Path),
			"event", "user.operation",
			"operation", strings.TrimPrefix(r.URL.Path, "/api/"),
			"outcome", outcome,
			"status", status,
		)
	})
}

func operationPathCategory(path string) string {
	path = strings.ToLower(path)
	switch {
	case strings.Contains(path, "/sms"):
		return "sms"
	case strings.Contains(path, "/call"):
		return "call"
	case strings.Contains(path, "/vowifi") || strings.Contains(path, "/ims"):
		return "vowifi"
	case strings.Contains(path, "/network") || strings.Contains(path, "/operator"):
		return "network"
	case strings.Contains(path, "/device") || strings.Contains(path, "/esim") ||
		strings.Contains(path, "/ussd") || strings.Contains(path, "/at"):
		return "hardware"
	default:
		return "operation"
	}
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=()")
		if r.URL.Path == oidc.DefaultCallbackPath {
			// OIDC 授权弹窗的结果页依赖内联脚本向 opener 发送 postMessage 并关闭窗口，
			// 因此仅对该路径放行内联脚本；新窗口流程不再被嵌入，仍保持 X-Frame-Options: DENY。
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set(
				"Content-Security-Policy",
				"default-src 'self'; base-uri 'self'; frame-ancestors 'none'; "+
					"object-src 'none'; form-action 'self'; "+
					"script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; font-src 'self'; "+
					"img-src 'self' data:; connect-src 'self'",
			)
		} else if strings.HasPrefix(r.URL.Path, "/websheets/") || strings.HasPrefix(r.URL.Path, "/plugin-assets/") {
			// The self-hosted E911 websheet is embedded in an iframe by the SPA, so
			// it must be frameable same-origin. Every other route stays DENY.
			w.Header().Set("X-Frame-Options", "SAMEORIGIN")
			w.Header().Set(
				"Content-Security-Policy",
				"default-src 'self'; base-uri 'self'; frame-ancestors 'self'; "+
					"object-src 'none'; form-action 'self'; "+
					"script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; font-src 'self'; "+
					"img-src 'self' data:; connect-src 'self'",
			)
		} else {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set(
				"Content-Security-Policy",
				"default-src 'self'; base-uri 'self'; frame-ancestors 'none'; "+
					"object-src 'none'; form-action 'self'; "+
					"script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; "+
					"img-src 'self' data:; connect-src 'self'",
			)
		}
		if s.secureCookies && s.https == nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic while serving request", "panic", recovered)
				writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
