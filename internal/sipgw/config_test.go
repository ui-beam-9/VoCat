package sipgw

import "testing"

// TestConfigNormalize 验证默认值与去重、清空无效项。
func TestConfigNormalize(t *testing.T) {
	cfg := Config{
		Users: []User{
			{Username: "linphone", Password: "p1"},
			{Username: "LinPhone", Password: "p2"}, // 重复（大小写不敏感）
			{Username: "  ", Password: "p3"},       // 无效
			{Username: "second", Password: "p4", DeviceID: "d2"},
		},
	}.Normalize()

	if cfg.Listen != DefaultListen {
		t.Fatalf("Listen = %q, 期望默认 %q", cfg.Listen, DefaultListen)
	}
	if cfg.Realm != DefaultRealm {
		t.Fatalf("Realm = %q, 期望默认 %q", cfg.Realm, DefaultRealm)
	}
	if cfg.Transport != "auto" {
		t.Fatalf("Transport = %q, 期望 auto", cfg.Transport)
	}
	if len(cfg.Users) != 2 {
		t.Fatalf("有效用户数 = %d, 期望 2", len(cfg.Users))
	}
}

// TestConfigValidate 覆盖地址、公网 IP 与密码校验。
func TestConfigValidate(t *testing.T) {
	valid := Config{Listen: "0.0.0.0:5060", Users: []User{{Username: "u", Password: "p"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("有效配置不应报错: %v", err)
	}

	badListen := Config{Listen: "not-an-address"}
	if err := badListen.Validate(); err == nil {
		t.Fatal("非法监听地址应报错")
	}

	badIP := Config{Listen: "0.0.0.0:5060", ExternalIP: "999.1.1.1"}
	if err := badIP.Validate(); err == nil {
		t.Fatal("非法 external_ip 应报错")
	}

	noPassword := Config{Listen: "0.0.0.0:5060", Users: []User{{Username: "u"}}}
	if err := noPassword.Validate(); err == nil {
		t.Fatal("缺少密码应报错")
	}
}

// TestConfigBackend 覆盖 backend 的归一化与取值校验。
func TestConfigBackend(t *testing.T) {
	// 空值归一化为 auto。
	if got := (Config{}).Normalize().Backend; got != BackendAuto {
		t.Fatalf("空 backend 应归一化为 auto，得到 %q", got)
	}
	// 大小写与空白清洗。
	if got := (Config{Backend: " IMS "}).Normalize().Backend; got != BackendIMS {
		t.Fatalf("backend 清洗失败，得到 %q", got)
	}
	// 合法取值通过校验。
	for _, backend := range []string{BackendAuto, BackendIMS, BackendCS} {
		cfg := Config{Listen: "0.0.0.0:5060", Backend: backend}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("backend=%q 应通过校验: %v", backend, err)
		}
	}
	// 非法取值被拒绝。
	bad := Config{Listen: "0.0.0.0:5060", Backend: "pstn"}
	if err := bad.Validate(); err == nil {
		t.Fatal("非法 backend 应报错")
	}
}
