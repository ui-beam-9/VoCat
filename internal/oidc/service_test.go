package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalizeConfig(t *testing.T) {
	t.Run("空配置补默认值", func(t *testing.T) {
		cfg, err := NormalizeConfig(Config{})
		if err != nil {
			t.Fatalf("NormalizeConfig() error = %v", err)
		}
		if !reflect.DeepEqual(cfg.Scopes, DefaultScopes) {
			t.Fatalf("Scopes = %v, want %v", cfg.Scopes, DefaultScopes)
		}
		if cfg.LoginButtonText != DefaultLoginButtonText {
			t.Fatalf("LoginButtonText = %q, want %q", cfg.LoginButtonText, DefaultLoginButtonText)
		}
		if len(cfg.CallbackURLs) != 0 {
			t.Fatalf("CallbackURLs = %v, want empty", cfg.CallbackURLs)
		}
	})

	t.Run("去空白并过滤非法回调地址", func(t *testing.T) {
		cfg, err := NormalizeConfig(Config{
			Scopes: []string{" openid ", "", "profile", "openid"},
			CallbackURLs: []string{
				" https://vocat.example.com/api/auth/oidc/callback ",
				"https://vocat.example.com/api/auth/oidc/callback",
				"ftp://vocat.example.com/callback",
				"not-a-url",
				"",
				"http://vocat.local/api/auth/oidc/callback",
			},
			LoginButtonText: "  统一登录  ",
			ClientSecret:    "  secret  ",
		})
		if err != nil {
			t.Fatalf("NormalizeConfig() error = %v", err)
		}
		wantScopes := []string{"openid", "profile"}
		if !reflect.DeepEqual(cfg.Scopes, wantScopes) {
			t.Fatalf("Scopes = %v, want %v", cfg.Scopes, wantScopes)
		}
		wantCallbacks := []string{
			"https://vocat.example.com/api/auth/oidc/callback",
			"http://vocat.local/api/auth/oidc/callback",
		}
		if !reflect.DeepEqual(cfg.CallbackURLs, wantCallbacks) {
			t.Fatalf("CallbackURLs = %v, want %v", cfg.CallbackURLs, wantCallbacks)
		}
		if cfg.LoginButtonText != "统一登录" {
			t.Fatalf("LoginButtonText = %q", cfg.LoginButtonText)
		}
		if cfg.ClientSecret != "secret" {
			t.Fatalf("ClientSecret = %q", cfg.ClientSecret)
		}
	})

	t.Run("启用时必填项缺失", func(t *testing.T) {
		for name, cfg := range map[string]Config{
			"缺少 well-known 地址": {Enabled: true, ClientID: "vocat"},
			"缺少 client_id":     {Enabled: true, WellKnownURL: "https://idp.example.com"},
			"两者都缺":             {Enabled: true},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := NormalizeConfig(cfg); !errors.Is(err, ErrNotConfigured) {
					t.Fatalf("NormalizeConfig() error = %v, want ErrNotConfigured", err)
				}
			})
		}
	})

	t.Run("启用且配置完整", func(t *testing.T) {
		if _, err := NormalizeConfig(Config{
			Enabled:      true,
			WellKnownURL: "https://idp.example.com/realms/vocat",
			ClientID:     "vocat",
		}); err != nil {
			t.Fatalf("NormalizeConfig() error = %v", err)
		}
	})

	t.Run("禁用时允许配置不完整", func(t *testing.T) {
		if _, err := NormalizeConfig(Config{}); err != nil {
			t.Fatalf("NormalizeConfig() error = %v", err)
		}
	})

	t.Run("非法 well-known 地址", func(t *testing.T) {
		for name, rawURL := range map[string]string{
			"缺少 scheme": "idp.example.com/realms/vocat",
			"非 HTTP":    "ftp://idp.example.com/realms/vocat",
			"缺少主机名":     "https:///realms/vocat",
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := NormalizeConfig(Config{WellKnownURL: rawURL}); err == nil {
					t.Fatalf("NormalizeConfig() 接受了非法地址 %q", rawURL)
				}
			})
		}
	})
}

func TestParseMarshalConfigRoundTrip(t *testing.T) {
	incoming := Config{
		Enabled:         true,
		WellKnownURL:    " https://idp.example.com/realms/vocat ",
		ClientID:        " vocat ",
		ClientSecret:    " top-secret ",
		Scopes:          []string{" openid ", "profile", "", "email"},
		CallbackURLs:    []string{"https://vocat.example.com/api/auth/oidc/callback"},
		LoginButtonText: "统一登录",
	}
	want, err := NormalizeConfig(incoming)
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	raw, err := MarshalConfig(incoming)
	if err != nil {
		t.Fatalf("MarshalConfig() error = %v", err)
	}
	got, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseConfig(MarshalConfig()) = %+v, want %+v", got, want)
	}

	defaults, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig(nil) error = %v", err)
	}
	if !reflect.DeepEqual(defaults.Scopes, DefaultScopes) {
		t.Fatalf("ParseConfig(nil) Scopes = %v, want %v", defaults.Scopes, DefaultScopes)
	}
	if _, err := ParseConfig([]byte("{not json")); err == nil {
		t.Fatal("ParseConfig() 接受了非法 JSON")
	}
}

func TestRedactAndMergeConfigSecret(t *testing.T) {
	cfg := Config{ClientID: "vocat", ClientSecret: "top-secret"}
	redacted := RedactConfig(cfg)
	if redacted.ClientSecret != secretMask {
		t.Fatalf("RedactConfig() ClientSecret = %q, want %q", redacted.ClientSecret, secretMask)
	}
	if cfg.ClientSecret != "top-secret" {
		t.Fatal("RedactConfig() 修改了原配置")
	}
	if empty := RedactConfig(Config{}); empty.ClientSecret != "" {
		t.Fatalf("RedactConfig() 为空 secret 填入了脱敏值: %q", empty.ClientSecret)
	}

	for name, incomingSecret := range map[string]string{
		"空字符串":  "",
		"脱敏占位符": secretMask,
	} {
		t.Run(name, func(t *testing.T) {
			merged := MergeConfigSecret(Config{ClientID: "vocat", ClientSecret: incomingSecret}, cfg)
			if merged.ClientSecret != "top-secret" {
				t.Fatalf("MergeConfigSecret() ClientSecret = %q, want top-secret", merged.ClientSecret)
			}
			if merged.ClientID != "vocat" {
				t.Fatalf("MergeConfigSecret() ClientID = %q", merged.ClientID)
			}
		})
	}
	merged := MergeConfigSecret(Config{ClientSecret: "new-secret"}, cfg)
	if merged.ClientSecret != "new-secret" {
		t.Fatalf("MergeConfigSecret() 未使用新 secret: %q", merged.ClientSecret)
	}
}

func TestDiscoveryURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "仅主机名",
			input: "https://idp.example.com",
			want:  "https://idp.example.com/.well-known/openid-configuration",
		},
		{
			name:  "主机名带结尾斜杠",
			input: "https://idp.example.com/",
			want:  "https://idp.example.com/.well-known/openid-configuration",
		},
		{
			name:  "带 issuer 路径",
			input: "https://idp.example.com/realms/vocat",
			want:  "https://idp.example.com/realms/vocat/.well-known/openid-configuration",
		},
		{
			name:  "路径带结尾斜杠与空白",
			input: " https://idp.example.com/realms/vocat/ ",
			want:  "https://idp.example.com/realms/vocat/.well-known/openid-configuration",
		},
		{
			name:  "已经是发现文档地址",
			input: "https://idp.example.com/.well-known/openid-configuration",
			want:  "https://idp.example.com/.well-known/openid-configuration",
		},
		{
			name:  "带主机端口的发现文档地址",
			input: "http://127.0.0.1:8443/realms/vocat/.well-known/openid-configuration",
			want:  "http://127.0.0.1:8443/realms/vocat/.well-known/openid-configuration",
		},
		{name: "空字符串", input: "   ", wantErr: true},
		{name: "缺少 scheme", input: "idp.example.com/realms/vocat", wantErr: true},
		{name: "非 HTTP scheme", input: "ftp://idp.example.com", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DiscoveryURL(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("DiscoveryURL(%q) 未返回错误，got %q", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DiscoveryURL(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("DiscoveryURL(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	t.Run("解析发现文档", func(t *testing.T) {
		var gotPath, gotAccept string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAccept = r.Header.Get("Accept")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"issuer": "https://idp.example.com",
				"authorization_endpoint": "https://idp.example.com/oauth2/authorize",
				"token_endpoint": "https://idp.example.com/oauth2/token",
				"userinfo_endpoint": "https://idp.example.com/oauth2/userinfo",
				"end_session_endpoint": "https://idp.example.com/oauth2/logout",
				"jwks_uri": "https://idp.example.com/oauth2/jwks",
				"scopes_supported": ["openid", " profile ", "email"]
			}`))
		}))
		defer server.Close()

		md, err := Discover(context.Background(), server.Client(), server.URL+"/realms/vocat")
		if err != nil {
			t.Fatalf("Discover() error = %v", err)
		}
		if gotPath != "/realms/vocat/.well-known/openid-configuration" {
			t.Fatalf("请求路径 = %q", gotPath)
		}
		if gotAccept != "application/json" {
			t.Fatalf("Accept = %q", gotAccept)
		}
		if md.Issuer != "https://idp.example.com" ||
			md.AuthorizationEndpoint != "https://idp.example.com/oauth2/authorize" ||
			md.TokenEndpoint != "https://idp.example.com/oauth2/token" ||
			md.UserinfoEndpoint != "https://idp.example.com/oauth2/userinfo" ||
			md.EndSessionEndpoint != "https://idp.example.com/oauth2/logout" ||
			md.JWKSURI != "https://idp.example.com/oauth2/jwks" {
			t.Fatalf("Metadata = %+v", md)
		}
		wantScopes := []string{"openid", "profile", "email"}
		if !reflect.DeepEqual(md.ScopesSupported, wantScopes) {
			t.Fatalf("ScopesSupported = %v, want %v", md.ScopesSupported, wantScopes)
		}
	})

	t.Run("异常响应", func(t *testing.T) {
		for name, handler := range map[string]http.HandlerFunc{
			"非 200": func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			"非法 JSON": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("not json"))
			},
			"缺少 issuer": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{
					"authorization_endpoint": "https://idp.example.com/auth",
					"token_endpoint": "https://idp.example.com/token"
				}`))
			},
			"缺少 authorization_endpoint": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{
					"issuer": "https://idp.example.com",
					"token_endpoint": "https://idp.example.com/token"
				}`))
			},
			"缺少 token_endpoint": func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{
					"issuer": "https://idp.example.com",
					"authorization_endpoint": "https://idp.example.com/auth"
				}`))
			},
		} {
			t.Run(name, func(t *testing.T) {
				server := httptest.NewServer(handler)
				defer server.Close()
				if _, err := Discover(context.Background(), server.Client(), server.URL); err == nil {
					t.Fatal("Discover() 未返回错误")
				}
			})
		}
	})
}

func TestEndpointOrigin(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "带端口与查询参数",
			endpoint: "https://idp.example.com:8443/oauth2/authorize?client_id=vocat",
			want:     "https://idp.example.com:8443",
		},
		{
			name:     "带 issuer 路径",
			endpoint: "https://idp.example.com/realms/vocat/protocol/openid-connect/auth",
			want:     "https://idp.example.com",
		},
		{name: "空端点", endpoint: "", want: ""},
		{name: "非 HTTP 端点", endpoint: "javascript:alert(1)", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := Metadata{AuthorizationEndpoint: test.endpoint}
			if got := metadata.EndpointOrigin(); got != test.want {
				t.Fatalf("EndpointOrigin() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSelectCallbackURL(t *testing.T) {
	tests := []struct {
		name        string
		callbacks   []string
		requestHost string
		secure      bool
		want        string
	}{
		{
			name: "优先匹配请求主机",
			callbacks: []string{
				"https://other.example.com/api/auth/oidc/callback",
				"https://vocat.example.com/api/auth/oidc/callback",
			},
			requestHost: "vocat.example.com",
			want:        "https://vocat.example.com/api/auth/oidc/callback",
		},
		{
			name: "主机匹配不区分大小写",
			callbacks: []string{
				"https://VOCAT.example.com/api/auth/oidc/callback",
			},
			requestHost: "vocat.example.com",
			want:        "https://VOCAT.example.com/api/auth/oidc/callback",
		},
		{
			name:        "无匹配主机时使用第一个合法地址",
			callbacks:   []string{"https://other.example.com/cb", "https://vocat.example.com/cb"},
			requestHost: "unknown.example.com",
			want:        "https://other.example.com/cb",
		},
		{
			name:        "未配置时按请求推导 HTTPS",
			requestHost: "vocat.example.com",
			secure:      true,
			want:        "https://vocat.example.com/api/auth/oidc/callback",
		},
		{
			name:        "未配置时按请求推导 HTTP",
			requestHost: "127.0.0.1:8080",
			want:        "http://127.0.0.1:8080/api/auth/oidc/callback",
		},
		{
			name:        "配置全部非法时按请求推导",
			callbacks:   []string{"ftp://vocat.example.com/cb", "not-a-url"},
			requestHost: "vocat.example.com",
			secure:      true,
			want:        "https://vocat.example.com/api/auth/oidc/callback",
		},
		{
			name: "请求主机为空时返回默认路径",
			want: DefaultCallbackPath,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SelectCallbackURL(test.callbacks, test.requestHost, test.secure)
			if got != test.want {
				t.Fatalf("SelectCallbackURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNewAuthorizationRequest(t *testing.T) {
	md := Metadata{
		Issuer:                "https://idp.example.com",
		AuthorizationEndpoint: "https://idp.example.com/oauth2/authorize?tenant=vocat",
	}
	cfg := Config{ClientID: "vocat", Scopes: []string{"openid", "profile", "email"}}
	callbackURL := "https://vocat.example.com/api/auth/oidc/callback"

	request, err := NewAuthorizationRequest(md, cfg, callbackURL, "/devices", "bind")
	if err != nil {
		t.Fatalf("NewAuthorizationRequest() error = %v", err)
	}
	if request.Mode != "bind" {
		t.Fatalf("Mode = %q, want bind", request.Mode)
	}
	if request.RedirectURI != callbackURL {
		t.Fatalf("RedirectURI = %q, want %q", request.RedirectURI, callbackURL)
	}

	authorizationURL, err := url.Parse(request.URL)
	if err != nil {
		t.Fatalf("解析授权 URL 失败: %v", err)
	}
	if authorizationURL.Host != "idp.example.com" || authorizationURL.Path != "/oauth2/authorize" {
		t.Fatalf("授权 URL = %q", request.URL)
	}
	query := authorizationURL.Query()
	checks := map[string]string{
		"client_id":             "vocat",
		"response_type":         "code",
		"redirect_uri":          callbackURL,
		"scope":                 "openid profile email",
		"state":                 request.State,
		"nonce":                 request.Nonce,
		"code_challenge":        challengeS256(request.CodeVerifier),
		"code_challenge_method": "S256",
		"tenant":                "vocat",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Fatalf("授权 URL 参数 %s = %q, want %q", key, got, want)
		}
	}

	// state/nonce/code_verifier 均为 32 字节随机数的 base64url 编码，长度 43。
	for name, value := range map[string]string{
		"state":         request.State,
		"nonce":         request.Nonce,
		"code_verifier": request.CodeVerifier,
	} {
		if len(value) != 43 {
			t.Fatalf("%s 长度 = %d, want 43", name, len(value))
		}
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			t.Fatalf("%s 不是合法 base64url: %v", name, err)
		}
		if len(decoded) != 32 {
			t.Fatalf("%s 解码长度 = %d, want 32", name, len(decoded))
		}
	}
	digest := sha256.Sum256([]byte(request.CodeVerifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if query.Get("code_challenge") != wantChallenge {
		t.Fatal("code_challenge 与 code_verifier 的 SHA-256 不匹配")
	}

	t.Run("模式缺省为 login", func(t *testing.T) {
		got, err := NewAuthorizationRequest(md, cfg, callbackURL, "/", "")
		if err != nil {
			t.Fatalf("NewAuthorizationRequest() error = %v", err)
		}
		if got.Mode != "login" {
			t.Fatalf("Mode = %q, want login", got.Mode)
		}
	})

	t.Run("非法模式", func(t *testing.T) {
		if _, err := NewAuthorizationRequest(md, cfg, callbackURL, "/", "refresh"); err == nil {
			t.Fatal("NewAuthorizationRequest() 接受了非法模式")
		}
	})

	t.Run("缺少授权端点", func(t *testing.T) {
		if _, err := NewAuthorizationRequest(Metadata{}, cfg, callbackURL, "/", "login"); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("error = %v, want ErrNotConfigured", err)
		}
	})

	t.Run("缺少回调地址", func(t *testing.T) {
		if _, err := NewAuthorizationRequest(md, cfg, "", "/", "login"); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("error = %v, want ErrNotConfigured", err)
		}
	})

	t.Run("缺少 client_id", func(t *testing.T) {
		if _, err := NewAuthorizationRequest(md, Config{}, callbackURL, "/", "login"); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("error = %v, want ErrNotConfigured", err)
		}
	})

	t.Run("非 HTTP 授权端点", func(t *testing.T) {
		if _, err := NewAuthorizationRequest(
			Metadata{AuthorizationEndpoint: "javascript:alert(1)"},
			cfg,
			callbackURL,
			"/",
			"login",
		); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("error = %v, want ErrNotConfigured", err)
		}
	})
}

func TestExchange(t *testing.T) {
	t.Run("成功兑换", func(t *testing.T) {
		var gotMethod, gotContentType string
		var gotForm url.Values
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotContentType = r.Header.Get("Content-Type")
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm() error = %v", err)
				return
			}
			gotForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-token","id_token":"id-token","expires_in":3600}`))
		}))
		defer server.Close()

		md := Metadata{TokenEndpoint: server.URL + "/oauth2/token"}
		cfg := Config{ClientID: "vocat", ClientSecret: "top-secret"}
		tokens, err := Exchange(
			context.Background(),
			server.Client(),
			md,
			cfg,
			"the-code",
			"https://vocat.example.com/api/auth/oidc/callback",
			"the-verifier",
		)
		if err != nil {
			t.Fatalf("Exchange() error = %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Fatalf("Method = %q, want POST", gotMethod)
		}
		if gotContentType != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q", gotContentType)
		}
		wantForm := map[string]string{
			"grant_type":    "authorization_code",
			"code":          "the-code",
			"redirect_uri":  "https://vocat.example.com/api/auth/oidc/callback",
			"client_id":     "vocat",
			"code_verifier": "the-verifier",
			"client_secret": "top-secret",
		}
		for key, want := range wantForm {
			if got := gotForm.Get(key); got != want {
				t.Fatalf("表单参数 %s = %q, want %q", key, got, want)
			}
		}
		if tokens.AccessToken != "access-token" || tokens.IDToken != "id-token" || tokens.ExpiresIn != 3600 {
			t.Fatalf("TokenSet = %+v", tokens)
		}
	})

	t.Run("公共客户端不附带 client_secret", func(t *testing.T) {
		var gotSecret string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			gotSecret = r.PostForm.Get("client_secret")
			_, _ = w.Write([]byte(`{"access_token":"access-token"}`))
		}))
		defer server.Close()

		tokens, err := Exchange(
			context.Background(),
			server.Client(),
			Metadata{TokenEndpoint: server.URL},
			Config{ClientID: "vocat"},
			"the-code",
			"https://vocat.example.com/callback",
			"the-verifier",
		)
		if err != nil {
			t.Fatalf("Exchange() error = %v", err)
		}
		if gotSecret != "" {
			t.Fatalf("client_secret = %q, want empty", gotSecret)
		}
		if tokens.AccessToken != "access-token" {
			t.Fatalf("AccessToken = %q", tokens.AccessToken)
		}
	})

	t.Run("令牌端点返回错误", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"授权码已过期"}`))
		}))
		defer server.Close()

		_, err := Exchange(
			context.Background(),
			server.Client(),
			Metadata{TokenEndpoint: server.URL},
			Config{ClientID: "vocat"},
			"expired-code",
			"https://vocat.example.com/callback",
			"the-verifier",
		)
		if err == nil {
			t.Fatal("Exchange() 未返回错误")
		}
		if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "授权码已过期") {
			t.Fatalf("错误信息缺少 IDP 返回的详情: %v", err)
		}
	})

	t.Run("缺少 token_endpoint", func(t *testing.T) {
		if _, err := Exchange(
			context.Background(),
			http.DefaultClient,
			Metadata{},
			Config{ClientID: "vocat"},
			"the-code",
			"",
			"",
		); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("error = %v, want ErrNotConfigured", err)
		}
	})
}

func TestResolveIdentity(t *testing.T) {
	idToken := encodeIDToken(t, map[string]any{
		"iss":   "https://attacker.example.com",
		"sub":   "id-token-subject",
		"email": "fallback@example.com",
		"name":  "兜底用户名",
	})

	t.Run("优先使用 userinfo", func(t *testing.T) {
		var gotAuthorization, gotAccept string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuthorization = r.Header.Get("Authorization")
			gotAccept = r.Header.Get("Accept")
			_, _ = w.Write([]byte(`{
				"sub": "userinfo-subject",
				"email": "admin@example.com",
				"preferred_username": "admin"
			}`))
		}))
		defer server.Close()

		md := Metadata{Issuer: "https://idp.example.com", UserinfoEndpoint: server.URL}
		identity, err := ResolveIdentity(
			context.Background(),
			server.Client(),
			md,
			TokenSet{AccessToken: "access-token", IDToken: idToken},
		)
		if err != nil {
			t.Fatalf("ResolveIdentity() error = %v", err)
		}
		if gotAuthorization != "Bearer access-token" {
			t.Fatalf("Authorization = %q", gotAuthorization)
		}
		if gotAccept != "application/json" {
			t.Fatalf("Accept = %q", gotAccept)
		}
		want := Identity{
			Issuer:  "https://idp.example.com",
			Subject: "userinfo-subject",
			Email:   "admin@example.com",
			Name:    "admin",
		}
		if identity != want {
			t.Fatalf("Identity = %+v, want %+v", identity, want)
		}
	})

	t.Run("userinfo 失败时回退 id_token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		md := Metadata{Issuer: "https://idp.example.com", UserinfoEndpoint: server.URL}
		identity, err := ResolveIdentity(
			context.Background(),
			server.Client(),
			md,
			TokenSet{AccessToken: "access-token", IDToken: idToken},
		)
		if err != nil {
			t.Fatalf("ResolveIdentity() error = %v", err)
		}
		want := Identity{
			Issuer:  "https://idp.example.com",
			Subject: "id-token-subject",
			Email:   "fallback@example.com",
			Name:    "兜底用户名",
		}
		if identity != want {
			t.Fatalf("Identity = %+v, want %+v", identity, want)
		}
	})

	t.Run("没有 userinfo 端点时使用 id_token", func(t *testing.T) {
		identity, err := ResolveIdentity(
			context.Background(),
			http.DefaultClient,
			Metadata{Issuer: "https://idp.example.com"},
			TokenSet{IDToken: idToken},
		)
		if err != nil {
			t.Fatalf("ResolveIdentity() error = %v", err)
		}
		if identity.Subject != "id-token-subject" || identity.Issuer != "https://idp.example.com" {
			t.Fatalf("Identity = %+v", identity)
		}
	})

	t.Run("userinfo 无 sub 时回退 id_token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"email":"nobody@example.com"}`))
		}))
		defer server.Close()

		identity, err := ResolveIdentity(
			context.Background(),
			server.Client(),
			Metadata{Issuer: "https://idp.example.com", UserinfoEndpoint: server.URL},
			TokenSet{AccessToken: "access-token", IDToken: idToken},
		)
		if err != nil {
			t.Fatalf("ResolveIdentity() error = %v", err)
		}
		if identity.Subject != "id-token-subject" {
			t.Fatalf("Subject = %q", identity.Subject)
		}
	})

	t.Run("userinfo 失败且无 id_token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := ResolveIdentity(
			context.Background(),
			server.Client(),
			Metadata{Issuer: "https://idp.example.com", UserinfoEndpoint: server.URL},
			TokenSet{AccessToken: "access-token"},
		)
		if err == nil {
			t.Fatal("ResolveIdentity() 未返回错误")
		}
		if !strings.Contains(err.Error(), "id_token") {
			t.Fatalf("错误信息 = %v", err)
		}
	})

	t.Run("始终取不到 sub", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"email":"nobody@example.com"}`))
		}))
		defer server.Close()

		_, err := ResolveIdentity(
			context.Background(),
			server.Client(),
			Metadata{Issuer: "https://idp.example.com", UserinfoEndpoint: server.URL},
			TokenSet{AccessToken: "access-token", IDToken: encodeIDToken(t, map[string]any{"email": "x@example.com"})},
		)
		if err == nil {
			t.Fatal("ResolveIdentity() 未返回错误")
		}
		if !strings.Contains(err.Error(), "sub") {
			t.Fatalf("错误信息 = %v", err)
		}
	})
}

func TestVerifyBinding(t *testing.T) {
	binding := Binding{
		Issuer:  "https://IDP.example.com",
		Subject: "subject-1",
		Email:   "admin@example.com",
	}
	identity := Identity{
		Issuer:  "https://idp.example.com",
		Subject: "subject-1",
		Email:   "admin@example.com",
	}
	if err := VerifyBinding(identity, binding); err != nil {
		t.Fatalf("VerifyBinding() error = %v", err)
	}

	if err := VerifyBinding(identity, Binding{}); !errors.Is(err, ErrNotBound) {
		t.Fatalf("未绑定时 error = %v, want ErrNotBound", err)
	}

	mismatchIssuer := identity
	mismatchIssuer.Issuer = "https://other.example.com"
	if err := VerifyBinding(mismatchIssuer, binding); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("issuer 不一致时 error = %v, want ErrIdentityMismatch", err)
	}

	mismatchSubject := identity
	mismatchSubject.Subject = "subject-2"
	if err := VerifyBinding(mismatchSubject, binding); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("subject 不一致时 error = %v, want ErrIdentityMismatch", err)
	}

	emptySubject := identity
	emptySubject.Subject = ""
	if err := VerifyBinding(emptySubject, binding); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("subject 为空时 error = %v, want ErrIdentityMismatch", err)
	}
}

func TestParseMarshalBinding(t *testing.T) {
	binding := Binding{
		Issuer:  " https://idp.example.com ",
		Subject: " subject-1 ",
		Email:   "admin@example.com",
		Name:    "管理员",
		BoundAt: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
	}
	raw, err := MarshalBinding(binding)
	if err != nil {
		t.Fatalf("MarshalBinding() error = %v", err)
	}
	got, err := ParseBinding(raw)
	if err != nil {
		t.Fatalf("ParseBinding() error = %v", err)
	}
	if got.Issuer != "https://idp.example.com" ||
		got.Subject != "subject-1" ||
		got.Email != binding.Email ||
		got.Name != binding.Name ||
		!got.BoundAt.Equal(binding.BoundAt) {
		t.Fatalf("ParseBinding() = %+v", got)
	}
	if _, err := ParseBinding([]byte("{bad json")); err == nil {
		t.Fatal("ParseBinding() 接受了非法 JSON")
	}
}

// encodeIDToken 构造仅用于兜底解析测试的无签名 id_token。
func encodeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("序列化 id_token claims 失败: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
