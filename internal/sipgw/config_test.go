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
	if cfg.Transport != "udp" {
		t.Fatalf("Transport = %q, 期望 udp", cfg.Transport)
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
