package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"

	"vocat/internal/auth"
	"vocat/internal/oidc"
	"vocat/internal/store"
)

// fakeOIDCIdentityProvider 是用 httptest 搭建的最小 IDP：发现文档 + 令牌端点 + userinfo。
// 令牌端点会校验授权 URL 中携带的 PKCE challenge 与回调地址，验证真实的 S256 流程。
type fakeOIDCIdentityProvider struct {
	server *httptest.Server

	mu            sync.Mutex
	subject       string
	email         string
	name          string
	codeChallenge string
	redirectURI   string
	clientSecret  string
}

func newFakeOIDCIdentityProvider(t *testing.T) *fakeOIDCIdentityProvider {
	t.Helper()
	provider := &fakeOIDCIdentityProvider{
		subject: "oidc-subject-1",
		email:   "admin@example.com",
		name:    "VoCat Admin",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                 provider.server.URL,
			"authorization_endpoint": provider.server.URL + "/authorize",
			"token_endpoint":         provider.server.URL + "/token",
			"userinfo_endpoint":      provider.server.URL + "/userinfo",
			"jwks_uri":               provider.server.URL + "/jwks",
			"scopes_supported":       []string{"openid", "profile", "email"},
		})
	})
	mux.HandleFunc("/token", provider.handleToken)
	mux.HandleFunc("/userinfo", provider.handleUserinfo)
	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *fakeOIDCIdentityProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	p.mu.Lock()
	challenge := p.codeChallenge
	redirectURI := p.redirectURI
	expectedSecret := p.clientSecret
	p.mu.Unlock()

	form := r.PostForm
	verifier := form.Get("code_verifier")
	digest := sha256.Sum256([]byte(verifier))
	validGrant := form.Get("grant_type") == "authorization_code" &&
		form.Get("code") == "test-code" &&
		verifier != "" &&
		form.Get("code_challenge_method") == "" && // 令牌请求不应再带 challenge 参数
		base64.RawURLEncoding.EncodeToString(digest[:]) == challenge &&
		form.Get("redirect_uri") == redirectURI
	if !validGrant {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "invalid_grant",
			"error_description": "授权码或 PKCE 校验失败",
		})
		return
	}
	if expectedSecret != "" && form.Get("client_secret") != expectedSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "access-token-1",
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

func (p *fakeOIDCIdentityProvider) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer access-token-1" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}
	p.mu.Lock()
	subject, email, name := p.subject, p.email, p.name
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"sub": subject, "email": email, "name": name})
}

// setIdentity 切换 fake IDP 返回的用户身份。
func (p *fakeOIDCIdentityProvider) setIdentity(subject, email, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subject, p.email, p.name = subject, email, name
}

// recordAuthorization 记录授权 URL 里的 PKCE challenge 与 redirect_uri，供令牌端点校验。
func (p *fakeOIDCIdentityProvider) recordAuthorization(t *testing.T, authorization *url.URL) {
	t.Helper()
	query := authorization.Query()
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("授权 URL 缺少 PKCE S256 参数: %s", authorization.String())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codeChallenge = query.Get("code_challenge")
	p.redirectURI = query.Get("redirect_uri")
}

// oidcTestApplication 把 store 一并暴露给测试，便于校验落库内容。
type oidcTestApplication struct {
	server *httptest.Server
	client *http.Client
	store  *store.Store
}

func newOIDCTestApplication(t *testing.T) *oidcTestApplication {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
	})
	authService, err := auth.New(database, auth.Options{
		SessionTTL: time.Hour,
		BcryptCost: bcrypt.MinCost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authService.EnsureAdmin(ctx, "admin", "correct-password"); err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>SPA shell</html>")},
	}
	handler, err := New(Options{
		Store:               database,
		Auth:                authService,
		Assets:              assets,
		MaxRequestBodyBytes: 64 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &oidcTestApplication{
		server: httpServer,
		client: &http.Client{Jar: jar},
		store:  database,
	}
}

// newBrowserClient 构造一个带独立 Cookie 且不自动跟随 302 的浏览器客户端。
func newBrowserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// callbackURL 返回当前测试服务的回调地址。
func (app *oidcTestApplication) callbackURL() string {
	return app.server.URL + oidc.DefaultCallbackPath
}

// do 发送请求并返回响应与响应体。
func (app *oidcTestApplication) do(
	t *testing.T,
	client *http.Client,
	method string,
	path string,
	body string,
	csrf string,
) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, app.server.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求 %s %s 失败: %v", method, path, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		request.Header.Set(csrfHeaderName, csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取 %s %s 响应失败: %v", method, path, err)
	}
	return response, payload
}

// loginWithPassword 用密码登录并返回 CSRF token。
func (app *oidcTestApplication) loginWithPassword(t *testing.T) string {
	t.Helper()
	response, payload := app.do(
		t,
		app.client,
		http.MethodPost,
		"/api/auth/login",
		`{"username":"admin","password":"correct-password"}`,
		"",
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("密码登录状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var parsed struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	if parsed.Data.CSRFToken == "" {
		t.Fatal("密码登录未返回 CSRF token")
	}
	return parsed.Data.CSRFToken
}

// oidcConfigDocument 是保存 OIDC 配置时的请求文档。
type oidcConfigDocument struct {
	Enabled         bool     `json:"enabled"`
	WellKnownURL    string   `json:"well_known_url"`
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret"`
	Scopes          []string `json:"scopes"`
	CallbackURLs    []string `json:"callback_urls"`
	LoginButtonText string   `json:"login_button_text"`
}

// configureOIDC 通过 PUT /api/settings/oidc 保存一份可用配置，返回响应 data。
func (app *oidcTestApplication) configureOIDC(
	t *testing.T,
	csrf string,
	idp *fakeOIDCIdentityProvider,
	mutate func(*oidcConfigDocument),
) map[string]any {
	t.Helper()
	document := oidcConfigDocument{
		Enabled:         true,
		WellKnownURL:    idp.server.URL,
		ClientID:        "vocat-client",
		ClientSecret:    "super-secret",
		Scopes:          []string{"openid", "profile", "email"},
		CallbackURLs:    []string{app.callbackURL()},
		LoginButtonText: "使用 OIDC 登录",
	}
	if mutate != nil {
		mutate(&document)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("编码 OIDC 配置失败: %v", err)
	}
	response, payload := app.do(
		t,
		app.client,
		http.MethodPut,
		"/api/settings/oidc",
		string(raw),
		csrf,
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("保存 OIDC 配置状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var parsed struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("解析保存 OIDC 配置响应失败: %v", err)
	}
	return parsed.Data
}

// startBinding 发起一次绑定并返回 IDP 授权 URL。
func (app *oidcTestApplication) startBinding(t *testing.T, csrf string) *url.URL {
	t.Helper()
	response, payload := app.do(
		t,
		app.client,
		http.MethodPost,
		"/api/settings/oidc/binding",
		"",
		csrf,
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("发起绑定状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var parsed struct {
		Data struct {
			AuthorizationURL string `json:"authorization_url"`
			Mode             string `json:"mode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("解析发起绑定响应失败: %v", err)
	}
	if parsed.Data.Mode != "bind" {
		t.Fatalf("绑定模式 = %q，期望 bind", parsed.Data.Mode)
	}
	authorization, err := url.Parse(parsed.Data.AuthorizationURL)
	if err != nil {
		t.Fatalf("解析授权地址失败: %v", err)
	}
	if authorization.Path != "/authorize" {
		t.Fatalf("授权地址路径 = %q，期望 /authorize", authorization.Path)
	}
	return authorization
}

// completeCallback 用授权 URL 中的 state 模拟 IDP 回调。
func (app *oidcTestApplication) completeCallback(
	t *testing.T,
	client *http.Client,
	idp *fakeOIDCIdentityProvider,
	authorization *url.URL,
) (*http.Response, []byte) {
	t.Helper()
	idp.recordAuthorization(t, authorization)
	state := authorization.Query().Get("state")
	if state == "" {
		t.Fatal("授权 URL 缺少 state")
	}
	return app.do(
		t,
		client,
		http.MethodGet,
		"/api/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=test-code",
		"",
		"",
	)
}

// bindAdmin 完成一次「发起绑定 + 回调」并断言成功。
func (app *oidcTestApplication) bindAdmin(
	t *testing.T,
	idp *fakeOIDCIdentityProvider,
	csrf string,
) {
	t.Helper()
	authorization := app.startBinding(t, csrf)
	response, payload := app.completeCallback(t, app.client, idp, authorization)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("绑定回调状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "绑定成功，可以关闭此窗口") {
		t.Fatalf("绑定成功页缺少成功文案: %s", payload)
	}
	if !strings.Contains(string(payload), "vocat:oidc-bound") {
		t.Fatalf("绑定成功页缺少 postMessage: %s", payload)
	}
}

// oidcLoginPopupStart 是一次弹窗登录发起接口的响应视图。
type oidcLoginPopupStart struct {
	Authorization *url.URL
	Mode          string
	Redirect      string
}

// startLoginPopup 通过公开接口发起一次弹窗登录，返回 IDP 授权 URL 与回传字段。
func (app *oidcTestApplication) startLoginPopup(
	t *testing.T,
	client *http.Client,
	redirect string,
) oidcLoginPopupStart {
	t.Helper()
	body, err := json.Marshal(map[string]string{"redirect": redirect})
	if err != nil {
		t.Fatalf("编码弹窗登录请求失败: %v", err)
	}
	response, payload := app.do(
		t,
		client,
		http.MethodPost,
		"/api/auth/oidc/login/start",
		string(body),
		"",
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("弹窗登录发起状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var parsed struct {
		Data struct {
			AuthorizationURL string `json:"authorization_url"`
			Mode             string `json:"mode"`
			Redirect         string `json:"redirect"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("解析弹窗登录响应失败: %v", err)
	}
	if parsed.Data.Mode != "login_popup" {
		t.Fatalf("弹窗登录模式 = %q，期望 login_popup", parsed.Data.Mode)
	}
	authorization, err := url.Parse(parsed.Data.AuthorizationURL)
	if err != nil {
		t.Fatalf("解析弹窗登录授权地址失败: %v", err)
	}
	if authorization.Path != "/authorize" {
		t.Fatalf("弹窗登录授权地址路径 = %q，期望 /authorize", authorization.Path)
	}
	return oidcLoginPopupStart{
		Authorization: authorization,
		Mode:          parsed.Data.Mode,
		Redirect:      parsed.Data.Redirect,
	}
}

// TestOIDCStatusIsPublicAndDisabledByDefault 验证状态接口无需登录即可读取，默认未启用。
func TestOIDCStatusIsPublicAndDisabledByDefault(t *testing.T) {
	app := newOIDCTestApplication(t)
	response, payload := app.do(t, app.client, http.MethodGet, "/api/auth/oidc/status", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态接口状态码 = %d，响应 = %s", response.StatusCode, payload)
	}
	var parsed struct {
		Data struct {
			Enabled         bool   `json:"enabled"`
			Bound           bool   `json:"bound"`
			LoginButtonText string `json:"login_button_text"`
			LoginURL        string `json:"login_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("解析状态响应失败: %v", err)
	}
	if parsed.Data.Enabled || parsed.Data.Bound {
		t.Fatalf("默认状态 = %+v，期望 enabled/bound 均为 false", parsed.Data)
	}
	if parsed.Data.LoginButtonText != oidc.DefaultLoginButtonText {
		t.Fatalf("默认按钮文案 = %q", parsed.Data.LoginButtonText)
	}
	if parsed.Data.LoginURL != "/api/auth/oidc/login" {
		t.Fatalf("login_url = %q", parsed.Data.LoginURL)
	}
}

// TestOIDCSettingsRequireAuthentication 验证设置接口必须登录，登出后重新变为未授权。
func TestOIDCSettingsRequireAuthentication(t *testing.T) {
	app := newOIDCTestApplication(t)

	response, _ := app.do(t, app.client, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录读取 OIDC 配置状态 = %d，期望 401", response.StatusCode)
	}
	response, _ = app.do(t, app.client, http.MethodPost, "/api/settings/oidc/binding", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录发起绑定状态 = %d，期望 401", response.StatusCode)
	}

	csrf := app.loginWithPassword(t)
	response, payload := app.do(t, app.client, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("登录后读取 OIDC 配置状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var settings struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("解析 OIDC 配置响应失败: %v", err)
	}
	if settings.Data["enabled"] != false || settings.Data["discovery"] != nil || settings.Data["binding"] != nil {
		t.Fatalf("默认 OIDC 配置 = %+v", settings.Data)
	}
	if settings.Data["callback_path"] != oidc.DefaultCallbackPath {
		t.Fatalf("callback_path = %v", settings.Data["callback_path"])
	}

	response, payload = app.do(t, app.client, http.MethodPost, "/api/auth/logout", "", csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("登出状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	response, _ = app.do(t, app.client, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后读取 OIDC 配置状态 = %d，期望 401", response.StatusCode)
	}
}

// TestOIDCConfigurationSaveDiscoveryAndRedaction 覆盖保存、发现、脱敏回读与 secret 合并。
func TestOIDCConfigurationSaveDiscoveryAndRedaction(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)

	data := app.configureOIDC(t, csrf, idp, nil)
	if data["client_secret"] != store.SecretMask {
		t.Fatalf("保存响应 client_secret = %v，期望脱敏", data["client_secret"])
	}
	if data["configured"] != true {
		t.Fatalf("configured = %v，期望 true", data["configured"])
	}
	if data["login_button_text"] != "使用 OIDC 登录" {
		t.Fatalf("login_button_text = %v", data["login_button_text"])
	}
	discovery, ok := data["discovery"].(map[string]any)
	if !ok {
		t.Fatalf("保存后 discovery = %#v，期望已发现端点", data["discovery"])
	}
	if discovery["issuer"] != idp.server.URL {
		t.Fatalf("discovery.issuer = %v，期望 %s", discovery["issuer"], idp.server.URL)
	}
	if discovery["authorization_endpoint"] != idp.server.URL+"/authorize" {
		t.Fatalf("discovery.authorization_endpoint = %v", discovery["authorization_endpoint"])
	}
	if data["binding"] != nil {
		t.Fatalf("未绑定时 binding = %#v，期望 null", data["binding"])
	}

	// 回读仍脱敏。
	response, payload := app.do(t, app.client, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("回读 OIDC 配置状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var settings struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("解析回读响应失败: %v", err)
	}
	if settings.Data["client_secret"] != store.SecretMask {
		t.Fatalf("回读 client_secret = %v，期望脱敏", settings.Data["client_secret"])
	}

	// 前端回传脱敏占位符时应保留库中原值，其余字段正常保存。
	app.configureOIDC(t, csrf, idp, func(document *oidcConfigDocument) {
		document.ClientSecret = store.SecretMask
		document.LoginButtonText = "SSO 登录"
	})
	setting, err := app.store.AppSetting(context.Background(), oidc.SettingConfigKey)
	if err != nil {
		t.Fatalf("读取落库 OIDC 配置失败: %v", err)
	}
	var stored oidc.Config
	if err := json.Unmarshal(setting.Value, &stored); err != nil {
		t.Fatalf("解析落库 OIDC 配置失败: %v", err)
	}
	if stored.ClientSecret != "super-secret" {
		t.Fatalf("落库 client_secret = %q，期望保留原值", stored.ClientSecret)
	}
	if stored.LoginButtonText != "SSO 登录" {
		t.Fatalf("落库 login_button_text = %q", stored.LoginButtonText)
	}

	// discover 预检接口。
	body := `{"well_known_url":"` + idp.server.URL + `"}`
	response, payload = app.do(t, app.client, http.MethodPost, "/api/settings/oidc/discover", body, csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("发现预检状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var discovered struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &discovered); err != nil {
		t.Fatalf("解析发现预检响应失败: %v", err)
	}
	if discovered.Data["token_endpoint"] != idp.server.URL+"/token" {
		t.Fatalf("发现预检 token_endpoint = %v", discovered.Data["token_endpoint"])
	}

	// 空 well_known_url 的预检应被拒绝。
	response, _ = app.do(t, app.client, http.MethodPost, "/api/settings/oidc/discover", `{"well_known_url":""}`, csrf)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 well_known_url 预检状态 = %d，期望 400", response.StatusCode)
	}
}

// TestOIDCEnabledConfigurationRequiresReachableDiscovery 验证启用时发现失败会返回 400 且不覆盖旧配置。
func TestOIDCEnabledConfigurationRequiresReachableDiscovery(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)

	unreachable := `{"enabled":true,"well_known_url":"http://127.0.0.1:1/.well-known/openid-configuration",` +
		`"client_id":"vocat-client","client_secret":"","scopes":[],"callback_urls":[],"login_button_text":""}`
	response, payload := app.do(t, app.client, http.MethodPut, "/api/settings/oidc", unreachable, csrf)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("不可达发现端点保存状态 = %d，期望 400，响应 = %s", response.StatusCode, payload)
	}

	setting, err := app.store.AppSetting(context.Background(), oidc.SettingConfigKey)
	if err != nil {
		t.Fatalf("读取落库 OIDC 配置失败: %v", err)
	}
	var stored oidc.Config
	if err := json.Unmarshal(setting.Value, &stored); err != nil {
		t.Fatalf("解析落库 OIDC 配置失败: %v", err)
	}
	if stored.WellKnownURL != idp.server.URL {
		t.Fatalf("落库 well_known_url = %q，期望保留旧值", stored.WellKnownURL)
	}

	// 未启用时发现失败不阻断保存，只把错误写入 discovery。
	disabled := `{"enabled":false,"well_known_url":"http://127.0.0.1:1/.well-known/openid-configuration",` +
		`"client_id":"vocat-client","client_secret":"","scopes":[],"callback_urls":[],"login_button_text":""}`
	response, payload = app.do(t, app.client, http.MethodPut, "/api/settings/oidc", disabled, csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("未启用且发现失败保存状态 = %d，期望 200，响应 = %s", response.StatusCode, payload)
	}
	var settings struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("解析保存响应失败: %v", err)
	}
	discovery, ok := settings.Data["discovery"].(map[string]any)
	if !ok || discovery["error"] == "" {
		t.Fatalf("未启用发现失败时 discovery = %#v，期望包含 error", settings.Data["discovery"])
	}
	if settings.Data["enabled"] != false {
		t.Fatalf("未启用保存后 enabled = %v", settings.Data["enabled"])
	}
}

// TestOIDCBindingAndLoginFlow 覆盖绑定闭环、状态视图、CSP/iframe 安全基线与 OIDC 登录闭环。
func TestOIDCBindingAndLoginFlow(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)

	// 绑定前状态：已启用、未绑定。
	response, payload := app.do(t, app.client, http.MethodGet, "/api/auth/oidc/status", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("绑定前状态接口 = %d，响应 = %s", response.StatusCode, payload)
	}
	var status struct {
		Data struct {
			Enabled bool `json:"enabled"`
			Bound   bool `json:"bound"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &status); err != nil {
		t.Fatalf("解析状态响应失败: %v", err)
	}
	if !status.Data.Enabled || status.Data.Bound {
		t.Fatalf("绑定前状态 = %+v，期望 enabled=true bound=false", status.Data)
	}

	// 绑定闭环。
	app.bindAdmin(t, idp, csrf)

	// 绑定视图。
	response, payload = app.do(t, app.client, http.MethodGet, "/api/settings/oidc/binding", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("读取绑定状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var bindingState struct {
		Data struct {
			Enabled         bool          `json:"enabled"`
			Bound           bool          `json:"bound"`
			Binding         *oidc.Binding `json:"binding"`
			LoginButtonText string        `json:"login_button_text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &bindingState); err != nil {
		t.Fatalf("解析绑定状态失败: %v", err)
	}
	if !bindingState.Data.Enabled || !bindingState.Data.Bound {
		t.Fatalf("绑定状态 = %+v，期望 enabled/bound 均为 true", bindingState.Data)
	}
	if bindingState.Data.Binding == nil {
		t.Fatal("绑定状态缺少 binding 明细")
	}
	if bindingState.Data.Binding.Issuer != idp.server.URL ||
		bindingState.Data.Binding.Subject != "oidc-subject-1" ||
		bindingState.Data.Binding.Email != "admin@example.com" {
		t.Fatalf("绑定明细 = %+v", bindingState.Data.Binding)
	}
	if bindingState.Data.LoginButtonText != "使用 OIDC 登录" {
		t.Fatalf("绑定状态按钮文案 = %q", bindingState.Data.LoginButtonText)
	}

	// 设置接口返回绑定信息。
	response, payload = app.do(t, app.client, http.MethodGet, "/api/settings/oidc", "", "")
	var settings struct {
		Data struct {
			Binding *oidc.Binding `json:"binding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("解析 OIDC 配置响应失败: %v", err)
	}
	if settings.Data.Binding == nil || settings.Data.Binding.Subject != "oidc-subject-1" {
		t.Fatalf("设置接口 binding = %+v", settings.Data.Binding)
	}

	// 状态接口：已启用 + 已绑定。
	response, payload = app.do(t, app.client, http.MethodGet, "/api/auth/oidc/status", "", "")
	if err := json.Unmarshal(payload, &status); err != nil {
		t.Fatalf("解析状态响应失败: %v", err)
	}
	if !status.Data.Enabled || !status.Data.Bound {
		t.Fatalf("绑定后状态 = %+v，期望 enabled/bound 均为 true", status.Data)
	}

	// 绑定授权在独立弹出窗口完成，因此页面 CSP 不再为 IDP 放行 frame-src；
	// 回调结果页也保持默认的 X-Frame-Options: DENY（不再被 iframe 嵌入）。
	response, _ = app.do(t, app.client, http.MethodGet, "/", "", "")
	csp := response.Header.Get("Content-Security-Policy")
	if strings.Contains(csp, "frame-src") {
		t.Fatalf("SPA CSP 不应再包含 frame-src: %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("SPA CSP 不应放行内联脚本: %q", csp)
	}
	// 回调结果页需要执行内联 postMessage 脚本，因此只对该路径放行内联脚本。
	response, _ = app.do(t, app.client, http.MethodGet, "/api/auth/oidc/callback?state=unknown", "", "")
	if xfo := response.Header.Get("X-Frame-Options"); xfo != "DENY" {
		t.Fatalf("OIDC 回调页 X-Frame-Options = %q，期望 DENY", xfo)
	}
	if callbackCSP := response.Header.Get("Content-Security-Policy"); !strings.Contains(
		callbackCSP,
		"script-src 'self' 'unsafe-inline'",
	) {
		t.Fatalf("OIDC 回调页 CSP 应放行内联脚本（结果页 postMessage）: %q", callbackCSP)
	}

	// 登出后走 OIDC 登录闭环。
	response, payload = app.do(t, app.client, http.MethodPost, "/api/auth/logout", "", csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("登出状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	browser := newBrowserClient(t)
	response, payload = app.do(
		t,
		browser,
		http.MethodGet,
		"/api/auth/oidc/login?redirect=/settings/oidc",
		"",
		"",
	)
	if response.StatusCode != http.StatusFound {
		t.Fatalf("OIDC 登录起始状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	idpURL, err := url.Parse(idp.server.URL)
	if err != nil {
		t.Fatalf("解析 IDP 地址失败: %v", err)
	}
	if location.Host != idpURL.Host {
		t.Fatalf("OIDC 登录跳转地址 = %q，期望跳转到 IDP %s", response.Header.Get("Location"), idpURL.Host)
	}
	response, payload = app.completeCallback(t, browser, idp, location)
	if response.StatusCode != http.StatusFound {
		t.Fatalf("OIDC 登录回调状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	if target := response.Header.Get("Location"); target != "/settings/oidc" {
		t.Fatalf("OIDC 登录回调跳转 = %q，期望 /settings/oidc", target)
	}
	// 回调已签发会话 Cookie，后续请求应视为已登录。
	response, payload = app.do(t, browser, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("OIDC 登录后读取配置状态 = %d，响应 = %s", response.StatusCode, payload)
	}
}

// TestOIDCLoginRejectsExternalRedirect 验证 redirect 参数只接受站内相对路径。
func TestOIDCLoginRejectsExternalRedirect(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)
	app.bindAdmin(t, idp, csrf)

	for _, target := range []string{"//evil.example.com/path", "https://evil.example.com/", "javascript:alert(1)"} {
		browser := newBrowserClient(t)
		response, payload := app.do(
			t,
			browser,
			http.MethodGet,
			"/api/auth/oidc/login?redirect="+url.QueryEscape(target),
			"",
			"",
		)
		if response.StatusCode != http.StatusFound {
			t.Fatalf("redirect=%q 起始状态 = %d，响应 = %s", target, response.StatusCode, payload)
		}
		authorization, err := url.Parse(response.Header.Get("Location"))
		if err != nil {
			t.Fatalf("解析授权地址失败: %v", err)
		}
		response, payload = app.completeCallback(t, browser, idp, authorization)
		if response.StatusCode != http.StatusFound {
			t.Fatalf("redirect=%q 回调状态 = %d，响应 = %s", target, response.StatusCode, payload)
		}
		if location := response.Header.Get("Location"); location != "/" {
			t.Fatalf("redirect=%q 回调跳转 = %q，期望退回 /", target, location)
		}
	}
}

// TestOIDCBindingRejectsDifferentIdentity 验证已绑定时不能用另一个 OIDC 身份覆盖绑定。
func TestOIDCBindingRejectsDifferentIdentity(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)
	app.bindAdmin(t, idp, csrf)

	idp.setIdentity("oidc-subject-2", "other@example.com", "Other User")
	authorization := app.startBinding(t, csrf)
	response, payload := app.completeCallback(t, app.client, idp, authorization)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("身份不匹配绑定回调状态 = %d，期望 400，响应 = %s", response.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "vocat:oidc-error") {
		t.Fatalf("身份不匹配绑定页缺少错误 postMessage: %s", payload)
	}
	if !strings.Contains(string(payload), oidc.ErrIdentityMismatch.Error()) {
		t.Fatalf("身份不匹配绑定页缺少错误说明: %s", payload)
	}

	response, payload = app.do(t, app.client, http.MethodGet, "/api/settings/oidc/binding", "", "")
	var bindingState struct {
		Data struct {
			Binding *oidc.Binding `json:"binding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &bindingState); err != nil {
		t.Fatalf("解析绑定状态失败: %v", err)
	}
	if bindingState.Data.Binding == nil || bindingState.Data.Binding.Subject != "oidc-subject-1" {
		t.Fatalf("绑定被错误覆盖: %+v", bindingState.Data.Binding)
	}

	// 未知 state 的回调应返回可 postMessage 的错误页。
	response, payload = app.do(
		t,
		newBrowserClient(t),
		http.MethodGet,
		"/api/auth/oidc/callback?state=unknown&code=test-code",
		"",
		"",
	)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知 state 回调状态 = %d，期望 400，响应 = %s", response.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "vocat:oidc-error") {
		t.Fatalf("未知 state 回调页缺少错误 postMessage: %s", payload)
	}
}

// TestOIDCUnbindAndLoginRequiresBinding 验证解绑后状态复位且 OIDC 登录被拒绝。
func TestOIDCUnbindAndLoginRequiresBinding(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)
	app.bindAdmin(t, idp, csrf)

	response, payload := app.do(t, app.client, http.MethodDelete, "/api/settings/oidc/binding", "", csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("解绑状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	var deleted struct {
		Data struct {
			Bound bool `json:"bound"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &deleted); err != nil {
		t.Fatalf("解析解绑响应失败: %v", err)
	}
	if deleted.Data.Bound {
		t.Fatal("解绑响应 bound 应为 false")
	}
	if _, err := app.store.AppSetting(context.Background(), oidc.SettingBindingKey); err == nil {
		t.Fatal("解绑后数据库仍存在 oidc.binding")
	}

	response, payload = app.do(t, app.client, http.MethodGet, "/api/settings/oidc/binding", "", "")
	var bindingState struct {
		Data struct {
			Bound   bool          `json:"bound"`
			Binding *oidc.Binding `json:"binding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &bindingState); err != nil {
		t.Fatalf("解析解绑后绑定状态失败: %v", err)
	}
	if bindingState.Data.Bound || bindingState.Data.Binding != nil {
		t.Fatalf("解绑后绑定状态 = %+v", bindingState.Data)
	}

	// 未绑定时 OIDC 登录回调应回登录页且不签发会话。
	browser := newBrowserClient(t)
	response, payload = app.do(t, browser, http.MethodGet, "/api/auth/oidc/login", "", "")
	if response.StatusCode != http.StatusFound {
		t.Fatalf("未绑定 OIDC 登录起始状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	authorization, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("解析授权地址失败: %v", err)
	}
	response, payload = app.completeCallback(t, browser, idp, authorization)
	if response.StatusCode != http.StatusFound {
		t.Fatalf("未绑定 OIDC 登录回调状态 = %d，响应 = %s", response.StatusCode, payload)
	}
	if location := response.Header.Get("Location"); location != "/login" {
		t.Fatalf("未绑定 OIDC 登录回调跳转 = %q，期望 /login", location)
	}
	response, _ = app.do(t, browser, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未绑定 OIDC 登录后仍可访问设置，状态 = %d", response.StatusCode)
	}
}

// TestOIDCBindStartRequiresAuthentication 验证公开登录入口的 bind 模式必须已登录。
func TestOIDCBindStartRequiresAuthentication(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)

	anonymous := newBrowserClient(t)
	response, payload := app.do(t, anonymous, http.MethodGet, "/api/auth/oidc/login?mode=bind", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名 bind 模式状态 = %d，期望 401，响应 = %s", response.StatusCode, payload)
	}

	// 已登录浏览器（复用会话 Cookie，但不自动跟随 302）可正常发起绑定。
	sessionClient := &http.Client{
		Jar: app.client.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, payload = app.do(t, sessionClient, http.MethodGet, "/api/auth/oidc/login?mode=bind", "", "")
	if response.StatusCode != http.StatusFound {
		t.Fatalf("已登录 bind 模式状态 = %d，响应 = %s", response.StatusCode, payload)
	}

	// 非法 mode 应返回 400。
	response, _ = app.do(t, sessionClient, http.MethodGet, "/api/auth/oidc/login?mode=other", "", "")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 mode 状态 = %d，期望 400", response.StatusCode)
	}
}

// TestOIDCLoginPopupStartUnavailableWhenDisabled 验证未启用时公开的弹窗登录发起接口返回 400，
// 并确认新路径只接受 POST、不影响原有 GET 整页跳转入口。
func TestOIDCLoginPopupStartUnavailableWhenDisabled(t *testing.T) {
	app := newOIDCTestApplication(t)
	browser := newBrowserClient(t)

	response, payload := app.do(
		t,
		browser,
		http.MethodPost,
		"/api/auth/oidc/login/start",
		`{"redirect":"/devices"}`,
		"",
	)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("未启用弹窗登录发起状态 = %d，期望 400，响应 = %s", response.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "oidc_unavailable") {
		t.Fatalf("未启用弹窗登录响应缺少错误码: %s", payload)
	}
	if !strings.Contains(string(payload), "OIDC 未启用或端点尚未就绪") {
		t.Fatalf("未启用弹窗登录响应缺少中文原因: %s", payload)
	}

	// 新路径只接受 POST。
	response, _ = app.do(t, browser, http.MethodGet, "/api/auth/oidc/login/start", "", "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 弹窗登录发起状态 = %d，期望 405", response.StatusCode)
	}
	// 原有整页跳转入口保持不变（未启用时为 503）。
	response, _ = app.do(t, browser, http.MethodGet, "/api/auth/oidc/login", "", "")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未启用整页跳转登录状态 = %d，期望 503", response.StatusCode)
	}
}

// TestOIDCLoginPopupUnboundShowsErrorPage 验证未绑定账号走弹窗登录时返回错误结果页且不签发会话。
func TestOIDCLoginPopupUnboundShowsErrorPage(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)

	browser := newBrowserClient(t)
	started := app.startLoginPopup(t, browser, "/devices")
	if started.Redirect != "/devices" {
		t.Fatalf("弹窗登录回传 redirect = %q，期望 /devices", started.Redirect)
	}
	response, payload := app.completeCallback(t, browser, idp, started.Authorization)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("未绑定弹窗登录回调状态 = %d，期望 400，响应 = %s", response.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "vocat:oidc-error") {
		t.Fatalf("未绑定弹窗登录结果页缺少错误 postMessage: %s", payload)
	}
	if !strings.Contains(string(payload), oidc.ErrNotBound.Error()) {
		t.Fatalf("未绑定弹窗登录结果页缺少中文原因: %s", payload)
	}
	if !strings.Contains(string(payload), "OIDC 登录失败") {
		t.Fatalf("未绑定弹窗登录结果页缺少失败标题: %s", payload)
	}
	if xfo := response.Header.Get("X-Frame-Options"); xfo != "DENY" {
		t.Fatalf("弹窗登录结果页 X-Frame-Options = %q，期望 DENY", xfo)
	}
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(
		csp,
		"script-src 'self' 'unsafe-inline'",
	) {
		t.Fatalf("弹窗登录结果页 CSP 应放行内联脚本: %q", csp)
	}

	// 失败页不应签发会话 Cookie。
	response, _ = app.do(t, browser, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未绑定弹窗登录后读取配置状态 = %d，期望 401", response.StatusCode)
	}
}

// TestOIDCLoginPopupBoundIssuesSession 验证已绑定账号弹窗登录返回成功结果页并签发可用的会话 Cookie。
func TestOIDCLoginPopupBoundIssuesSession(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)
	app.bindAdmin(t, idp, csrf)

	response, payload := app.do(t, app.client, http.MethodPost, "/api/auth/logout", "", csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("登出状态 = %d，响应 = %s", response.StatusCode, payload)
	}

	browser := newBrowserClient(t)
	started := app.startLoginPopup(t, browser, "/devices")
	if started.Redirect != "/devices" {
		t.Fatalf("弹窗登录回传 redirect = %q，期望 /devices", started.Redirect)
	}
	response, payload = app.completeCallback(t, browser, idp, started.Authorization)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("已绑定弹窗登录回调状态 = %d，期望 200，响应 = %s", response.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "vocat:oidc-login") {
		t.Fatalf("弹窗登录成功页缺少成功 postMessage: %s", payload)
	}
	if !strings.Contains(string(payload), "登录成功，可以关闭此窗口") {
		t.Fatalf("弹窗登录成功页缺少成功文案: %s", payload)
	}
	if location := response.Header.Get("Location"); location != "" {
		t.Fatalf("弹窗登录成功页不应 302，Location = %q", location)
	}
	if xfo := response.Header.Get("X-Frame-Options"); xfo != "DENY" {
		t.Fatalf("弹窗登录成功页 X-Frame-Options = %q，期望 DENY", xfo)
	}
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(
		csp,
		"script-src 'self' 'unsafe-inline'",
	) {
		t.Fatalf("弹窗登录成功页 CSP 应放行内联脚本: %q", csp)
	}

	// 回调已签发会话 Cookie，后续请求应视为已登录。
	response, payload = app.do(t, browser, http.MethodGet, "/api/settings/oidc", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("弹窗登录后读取配置状态 = %d，响应 = %s", response.StatusCode, payload)
	}
}

// TestOIDCLoginPopupSanitizesRedirect 验证弹窗登录的 redirect 只接受站内相对路径。
func TestOIDCLoginPopupSanitizesRedirect(t *testing.T) {
	idp := newFakeOIDCIdentityProvider(t)
	app := newOIDCTestApplication(t)
	csrf := app.loginWithPassword(t)
	app.configureOIDC(t, csrf, idp, nil)
	app.bindAdmin(t, idp, csrf)

	browser := newBrowserClient(t)
	for _, target := range []string{"//evil.example.com/path", "https://evil.example.com/", "javascript:alert(1)"} {
		started := app.startLoginPopup(t, browser, target)
		if started.Redirect != "/" {
			t.Fatalf("redirect=%q 回传 %q，期望退回 /", target, started.Redirect)
		}
		response, payload := app.completeCallback(t, browser, idp, started.Authorization)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("redirect=%q 弹窗登录回调状态 = %d，响应 = %s", target, response.StatusCode, payload)
		}
		if !strings.Contains(string(payload), "vocat:oidc-login") {
			t.Fatalf("redirect=%q 弹窗登录成功页缺少成功 postMessage: %s", target, payload)
		}
	}
}
