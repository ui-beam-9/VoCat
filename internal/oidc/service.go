// Package oidc 提供 VoCat 的 OIDC（OpenID Connect）核心能力：
// 配置解析与校验、Well-known 端点发现、授权 URL 构造、授权码兑换、
// 用户身份解析以及管理员账号绑定校验。
//
// 本包只依赖 Go 标准库，不负责持久化与 HTTP 路由：
// 配置与绑定记录由 internal/store 的 app_settings 表保存，
// HTTP 接口与 pending state 缓存由 internal/server 实现。
package oidc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// SettingConfigKey 是 app_settings 中保存整条 OIDC 配置的键。
	SettingConfigKey = "oidc.config"
	// SettingBindingKey 是 app_settings 中保存管理员绑定关系的键，未绑定时不存在。
	SettingBindingKey = "oidc.binding"
	// DefaultLoginButtonText 是登录按钮文案的默认值。
	DefaultLoginButtonText = "OIDC 登录"
	// DefaultCallbackPath 是回调接口的默认路径。
	DefaultCallbackPath = "/api/auth/oidc/callback"
)

// DefaultScopes 是未配置 scopes 时使用的默认授权范围。
var DefaultScopes = []string{"openid", "profile", "email"}

// secretMask 与 store.SecretMask 保持一致的脱敏占位符，避免本包依赖存储层。
const secretMask = "********"

// maxResponseBytes 限制从 IDP 读取的响应体大小，避免异常响应耗尽内存。
const maxResponseBytes = 1 << 20

// Config 是整条 OIDC 配置，序列化后保存在 app_settings 的 oidc.config 键中。
type Config struct {
	Enabled         bool     `json:"enabled"`
	WellKnownURL    string   `json:"well_known_url"`
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret"`
	Scopes          []string `json:"scopes"`
	CallbackURLs    []string `json:"callback_urls"`
	LoginButtonText string   `json:"login_button_text"`
}

// Metadata 是 OIDC Well-known 发现文档中本系统会用到的字段。
type Metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// Binding 是管理员账号与 OIDC 身份之间的绑定关系。
type Binding struct {
	Issuer  string    `json:"issuer"`
	Subject string    `json:"subject"`
	Email   string    `json:"email"`
	Name    string    `json:"name"`
	BoundAt time.Time `json:"bound_at"`
}

// Identity 是从 IDP 解析出的用户身份。Issuer 始终取自发现文档，不取 id_token 中未校验的值。
type Identity struct {
	Issuer  string
	Subject string
	Email   string
	Name    string
}

// TokenSet 是授权码兑换得到的令牌集合。
type TokenSet struct {
	AccessToken string
	IDToken     string
	ExpiresIn   int
}

// AuthorizationRequest 是一次 OIDC 授权请求的上下文。
type AuthorizationRequest struct {
	URL          string
	State        string
	Nonce        string
	CodeVerifier string
	RedirectURI  string
	Mode         string
}

var (
	ErrDisabled         = errors.New("OIDC 未启用")
	ErrNotConfigured    = errors.New("OIDC 配置不完整")
	ErrNotBound         = errors.New("管理员未绑定 OIDC 用户")
	ErrIdentityMismatch = errors.New("OIDC 身份与已绑定账号不一致")
)

// ParseConfig 解析并规范化 OIDC 配置。空内容按零值配置处理。
func ParseConfig(raw []byte) (Config, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return NormalizeConfig(Config{})
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析 OIDC 配置失败: %w", err)
	}
	return NormalizeConfig(cfg)
}

// NormalizeConfig 去空白、补默认值，并校验 URL 与启用时的必填项。
func NormalizeConfig(cfg Config) (Config, error) {
	cfg.WellKnownURL = strings.TrimSpace(cfg.WellKnownURL)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	cfg.ClientSecret = strings.TrimSpace(cfg.ClientSecret)
	cfg.LoginButtonText = strings.TrimSpace(cfg.LoginButtonText)
	cfg.Scopes = normalizeStrings(cfg.Scopes)
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = append([]string(nil), DefaultScopes...)
	}
	if cfg.LoginButtonText == "" {
		cfg.LoginButtonText = DefaultLoginButtonText
	}

	// 回调地址只保留能被解析且 scheme 为 http/https 的项，非法项直接丢弃。
	callbackURLs := make([]string, 0, len(cfg.CallbackURLs))
	seenCallbacks := make(map[string]struct{}, len(cfg.CallbackURLs))
	for _, raw := range cfg.CallbackURLs {
		candidate := strings.TrimSpace(raw)
		if candidate == "" {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Host == "" {
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			continue
		}
		if _, duplicate := seenCallbacks[candidate]; duplicate {
			continue
		}
		seenCallbacks[candidate] = struct{}{}
		callbackURLs = append(callbackURLs, candidate)
	}
	cfg.CallbackURLs = callbackURLs

	if cfg.WellKnownURL != "" {
		parsed, err := url.Parse(cfg.WellKnownURL)
		if err != nil {
			return Config{}, fmt.Errorf("OIDC well-known 地址格式不合法: %w", err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, fmt.Errorf("%w: well-known 地址必须以 http:// 或 https:// 开头", ErrNotConfigured)
		}
		if parsed.Host == "" {
			return Config{}, fmt.Errorf("%w: well-known 地址缺少主机名", ErrNotConfigured)
		}
	}

	if cfg.Enabled {
		if cfg.WellKnownURL == "" {
			return Config{}, fmt.Errorf("%w: 启用时必须填写 well-known 地址", ErrNotConfigured)
		}
		if cfg.ClientID == "" {
			return Config{}, fmt.Errorf("%w: 启用时必须填写 client_id", ErrNotConfigured)
		}
	}
	return cfg, nil
}

// MarshalConfig 将配置规范化后序列化为 JSON，调用方无需再次归一化。
func MarshalConfig(cfg Config) ([]byte, error) {
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("序列化 OIDC 配置失败: %w", err)
	}
	return encoded, nil
}

// ParseBinding 解析绑定关系 JSON。
func ParseBinding(raw []byte) (Binding, error) {
	var binding Binding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return Binding{}, fmt.Errorf("解析 OIDC 绑定信息失败: %w", err)
	}
	binding.Issuer = strings.TrimSpace(binding.Issuer)
	binding.Subject = strings.TrimSpace(binding.Subject)
	binding.Email = strings.TrimSpace(binding.Email)
	binding.Name = strings.TrimSpace(binding.Name)
	return binding, nil
}

// MarshalBinding 将绑定关系序列化为 JSON。
func MarshalBinding(binding Binding) ([]byte, error) {
	encoded, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("序列化 OIDC 绑定信息失败: %w", err)
	}
	return encoded, nil
}

// RedactConfig 将 client_secret 替换为脱敏占位符，用于返回前端。
func RedactConfig(cfg Config) Config {
	if cfg.ClientSecret != "" {
		cfg.ClientSecret = secretMask
	}
	return cfg
}

// MergeConfigSecret 在前端回传的 client_secret 为空或为脱敏占位符时，沿用数据库中的旧值。
func MergeConfigSecret(incoming, current Config) Config {
	if incoming.ClientSecret == "" || incoming.ClientSecret == secretMask {
		incoming.ClientSecret = current.ClientSecret
	}
	return incoming
}

// DiscoveryURL 由用户填写的 well-known 地址推导出发现文档 URL：
// 允许直接填写发现文档 URL，也允许只填 issuer（此时自动补 /.well-known/openid-configuration）。
func DiscoveryURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: well-known 地址不能为空", ErrNotConfigured)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("解析 OIDC well-known 地址失败: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: well-known 地址必须以 http:// 或 https:// 开头", ErrNotConfigured)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%w: well-known 地址缺少主机名", ErrNotConfigured)
	}
	parsed.Fragment = ""
	discoverySuffix := "/.well-known/openid-configuration"
	trimmedPath := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(trimmedPath, discoverySuffix) {
		parsed.Path = trimmedPath
		return parsed.String(), nil
	}
	parsed.Path = trimmedPath + discoverySuffix
	parsed.RawQuery = ""
	return parsed.String(), nil
}

// Discover 请求发现文档并返回其中的端点信息。超时由调用方传入的 client 控制。
func Discover(ctx context.Context, client *http.Client, wellKnownURL string) (Metadata, error) {
	discoveryURL, err := DiscoveryURL(wellKnownURL)
	if err != nil {
		return Metadata{}, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("构造 OIDC 发现请求失败: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return Metadata{}, fmt.Errorf("请求 OIDC 发现端点失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Metadata{}, fmt.Errorf("OIDC 发现端点返回状态码 %d", response.StatusCode)
	}

	var metadata Metadata
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, fmt.Errorf("解析 OIDC 发现文档失败: %w", err)
	}
	metadata.Issuer = strings.TrimSpace(metadata.Issuer)
	metadata.AuthorizationEndpoint = strings.TrimSpace(metadata.AuthorizationEndpoint)
	metadata.TokenEndpoint = strings.TrimSpace(metadata.TokenEndpoint)
	metadata.UserinfoEndpoint = strings.TrimSpace(metadata.UserinfoEndpoint)
	metadata.EndSessionEndpoint = strings.TrimSpace(metadata.EndSessionEndpoint)
	metadata.JWKSURI = strings.TrimSpace(metadata.JWKSURI)
	metadata.ScopesSupported = normalizeStrings(metadata.ScopesSupported)

	if metadata.Issuer == "" {
		return Metadata{}, fmt.Errorf("%w: 发现文档缺少 issuer", ErrNotConfigured)
	}
	if metadata.AuthorizationEndpoint == "" {
		return Metadata{}, fmt.Errorf("%w: 发现文档缺少 authorization_endpoint", ErrNotConfigured)
	}
	if metadata.TokenEndpoint == "" {
		return Metadata{}, fmt.Errorf("%w: 发现文档缺少 token_endpoint", ErrNotConfigured)
	}
	return metadata, nil
}

// EndpointOrigin 返回 authorization_endpoint 的 scheme://host，供 CSP frame-src 使用。
// 端点无法解析时返回空字符串，调用方应退回 "self"。
func (m Metadata) EndpointOrigin() string {
	parsed, err := url.Parse(strings.TrimSpace(m.AuthorizationEndpoint))
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// SelectCallbackURL 从配置的多回调地址中选出与当前请求主机匹配的地址；
// 没有配置或配置均非法时，按请求主机与时序推导出回调地址。
func SelectCallbackURL(callbackURLs []string, requestHost string, secure bool) string {
	requestHost = strings.TrimSpace(requestHost)
	valid := make([]string, 0, len(callbackURLs))
	for _, raw := range callbackURLs {
		candidate := strings.TrimSpace(raw)
		if candidate == "" {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Host == "" {
			continue
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			continue
		}
		valid = append(valid, candidate)
	}
	if len(valid) > 0 {
		for _, candidate := range valid {
			parsed, err := url.Parse(candidate)
			if err != nil {
				continue
			}
			if strings.EqualFold(parsed.Host, requestHost) {
				return candidate
			}
		}
		return valid[0]
	}
	if requestHost == "" {
		return DefaultCallbackPath
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	return scheme + "://" + requestHost + DefaultCallbackPath
}

// NewAuthorizationRequest 构造授权 URL，并生成 state、nonce 与 PKCE S256 参数。
// redirectPath 由调用方保存到服务端 pending state，不参与授权 URL 构造；
// mode 仅允许 login 或 bind，传空值时按 login 处理。
func NewAuthorizationRequest(
	md Metadata,
	cfg Config,
	callbackURL string,
	redirectPath string,
	mode string,
) (AuthorizationRequest, error) {
	_ = redirectPath

	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = "login"
	}
	if mode != "login" && mode != "bind" {
		return AuthorizationRequest{}, fmt.Errorf("OIDC 授权模式 %q 不受支持，仅允许 login 或 bind", mode)
	}

	authorizationEndpoint := strings.TrimSpace(md.AuthorizationEndpoint)
	if authorizationEndpoint == "" {
		return AuthorizationRequest{}, fmt.Errorf("%w: 缺少 authorization_endpoint", ErrNotConfigured)
	}
	callbackURL = strings.TrimSpace(callbackURL)
	if callbackURL == "" {
		return AuthorizationRequest{}, fmt.Errorf("%w: 缺少回调地址", ErrNotConfigured)
	}
	clientID := strings.TrimSpace(cfg.ClientID)
	if clientID == "" {
		return AuthorizationRequest{}, fmt.Errorf("%w: 缺少 client_id", ErrNotConfigured)
	}
	scopes := normalizeStrings(cfg.Scopes)
	if len(scopes) == 0 {
		scopes = append([]string(nil), DefaultScopes...)
	}

	state, err := randomURLToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	nonce, err := randomURLToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	codeVerifier, err := randomURLToken()
	if err != nil {
		return AuthorizationRequest{}, err
	}
	codeChallenge := challengeS256(codeVerifier)

	parsed, err := url.Parse(authorizationEndpoint)
	if err != nil {
		return AuthorizationRequest{}, fmt.Errorf("解析 authorization_endpoint 失败: %w", err)
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return AuthorizationRequest{}, fmt.Errorf("%w: authorization_endpoint 不是合法的 HTTP 地址", ErrNotConfigured)
	}
	query := parsed.Query()
	query.Set("client_id", clientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", callbackURL)
	query.Set("scope", strings.Join(scopes, " "))
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", codeChallenge)
	query.Set("code_challenge_method", "S256")
	parsed.RawQuery = query.Encode()

	return AuthorizationRequest{
		URL:          parsed.String(),
		State:        state,
		Nonce:        nonce,
		CodeVerifier: codeVerifier,
		RedirectURI:  callbackURL,
		Mode:         mode,
	}, nil
}

// Exchange 使用授权码向令牌端点换取令牌（client_secret_post，secret 为空时不附带）。
func Exchange(
	ctx context.Context,
	client *http.Client,
	md Metadata,
	cfg Config,
	code string,
	redirectURI string,
	codeVerifier string,
) (TokenSet, error) {
	tokenEndpoint := strings.TrimSpace(md.TokenEndpoint)
	if tokenEndpoint == "" {
		return TokenSet{}, fmt.Errorf("%w: 缺少 token_endpoint", ErrNotConfigured)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return TokenSet{}, errors.New("OIDC 授权码不能为空")
	}
	if client == nil {
		client = http.DefaultClient
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", strings.TrimSpace(cfg.ClientID))
	form.Set("code_verifier", codeVerifier)
	if secret := strings.TrimSpace(cfg.ClientSecret); secret != "" {
		form.Set("client_secret", secret)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenEndpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return TokenSet{}, fmt.Errorf("构造 OIDC 令牌请求失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return TokenSet{}, fmt.Errorf("请求 OIDC 令牌端点失败: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return TokenSet{}, fmt.Errorf("读取 OIDC 令牌响应失败: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return TokenSet{}, fmt.Errorf(
			"OIDC 令牌端点返回状态码 %d: %s",
			response.StatusCode,
			tokenErrorDetail(body),
		)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return TokenSet{}, fmt.Errorf("解析 OIDC 令牌响应失败: %w", err)
	}
	return TokenSet{
		AccessToken: payload.AccessToken,
		IDToken:     payload.IDToken,
		ExpiresIn:   payload.ExpiresIn,
	}, nil
}

// ResolveIdentity 解析用户身份：优先 userinfo，取不到 sub 时回退到 id_token payload。
// id_token 不做签名校验，仅作为兜底；Issuer 始终取自发现文档。
func ResolveIdentity(
	ctx context.Context,
	client *http.Client,
	md Metadata,
	tokens TokenSet,
) (Identity, error) {
	identity := Identity{Issuer: strings.TrimSpace(md.Issuer)}

	userinfoEndpoint := strings.TrimSpace(md.UserinfoEndpoint)
	accessToken := strings.TrimSpace(tokens.AccessToken)
	var userinfoErr error
	if userinfoEndpoint != "" && accessToken != "" {
		claims, err := fetchUserinfo(ctx, client, userinfoEndpoint, accessToken)
		if err != nil {
			userinfoErr = err
		} else {
			identity.Subject = claimString(claims, "sub")
			identity.Email = claimString(claims, "email")
			identity.Name = claimString(claims, "name", "preferred_username")
		}
	}

	if identity.Subject == "" {
		claims, err := decodeIDTokenClaims(tokens.IDToken)
		if err != nil {
			if userinfoErr != nil {
				return Identity{}, fmt.Errorf(
					"OIDC 用户信息端点获取失败（%v），且无法解析 id_token: %w",
					userinfoErr,
					err,
				)
			}
			return Identity{}, err
		}
		identity.Subject = claimString(claims, "sub")
		if identity.Email == "" {
			identity.Email = claimString(claims, "email")
		}
		if identity.Name == "" {
			identity.Name = claimString(claims, "name", "preferred_username")
		}
	}

	if identity.Subject == "" {
		return Identity{}, errors.New("OIDC 用户信息中缺少 sub 标识")
	}
	return identity, nil
}

// VerifyBinding 校验解析出的身份与已绑定账号是否一致。
func VerifyBinding(identity Identity, binding Binding) error {
	if strings.TrimSpace(binding.Subject) == "" {
		return ErrNotBound
	}
	if !strings.EqualFold(
		strings.TrimSpace(identity.Issuer),
		strings.TrimSpace(binding.Issuer),
	) {
		return fmt.Errorf("%w: issuer 不一致", ErrIdentityMismatch)
	}
	if strings.TrimSpace(identity.Subject) != strings.TrimSpace(binding.Subject) {
		return fmt.Errorf("%w: subject 不一致", ErrIdentityMismatch)
	}
	return nil
}

// fetchUserinfo 使用 access_token 请求用户信息端点。
func fetchUserinfo(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	accessToken string,
) (map[string]any, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造 userinfo 请求失败: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 userinfo 端点失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo 端点返回状态码 %d", response.StatusCode)
	}
	var claims map[string]any
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err := decoder.Decode(&claims); err != nil {
		return nil, fmt.Errorf("解析 userinfo 响应失败: %w", err)
	}
	return claims, nil
}

// decodeIDTokenClaims 仅 base64url 解码 id_token 的 payload 段，不做签名校验。
func decodeIDTokenClaims(idToken string) (map[string]any, error) {
	idToken = strings.TrimSpace(idToken)
	if idToken == "" {
		return nil, errors.New("id_token 为空")
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil, errors.New("id_token 格式不合法")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// 兼容带填充的标准 base64url 编码。
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("解码 id_token payload 失败: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("解析 id_token payload 失败: %w", err)
	}
	return claims, nil
}

// claimString 依次尝试多个声明名，返回第一个非空字符串值。
func claimString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := claims[key]
		if !ok {
			continue
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// tokenErrorDetail 从令牌端点的错误响应中提取可读的错误说明。
func tokenErrorDetail(body []byte) string {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		switch {
		case payload.Error != "" && payload.ErrorDescription != "":
			return payload.Error + ": " + payload.ErrorDescription
		case payload.Error != "":
			return payload.Error
		case payload.ErrorDescription != "":
			return payload.ErrorDescription
		}
	}
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return "无响应内容"
	}
	if len(detail) > 200 {
		detail = detail[:200] + "..."
	}
	return detail
}

// normalizeStrings 去空白、丢弃空项并按首次出现顺序去重。
func normalizeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

// randomURLToken 生成 32 字节随机数的 base64url 编码（43 个字符）。
func randomURLToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成 OIDC 随机参数失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// challengeS256 计算 PKCE S256 challenge：base64url(sha256(verifier))。
func challengeS256(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
