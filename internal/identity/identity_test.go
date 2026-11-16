package identity

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/testcert"
)

func TestDiscoverPrepareSelectAndSwitchProfiles(t *testing.T) {
	root := t.TempDir()
	certDir := filepath.Join(root, "certs")
	cfgDir := filepath.Join(root, "configs")
	srvCfg := filepath.Join(cfgDir, "server.local.json")

	if _, err := certmanage.Execute("init", certmanage.Options{
		Dir:             certDir,
		ServerConfig:    srvCfg,
		ClientConfigDir: cfgDir,
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	}); err != nil {
		t.Fatalf("certmanage init: %v", err)
	}
	if _, err := certmanage.Execute("user", certmanage.Options{
		Dir:             certDir,
		ServerConfig:    srvCfg,
		ClientConfigDir: cfgDir,
		Name:            "bob",
	}); err != nil {
		t.Fatalf("certmanage user bob: %v", err)
	}

	aliceOut := filepath.Join(root, "alice-bundle")
	bobOut := filepath.Join(root, "bob-bundle")
	if _, err := certmanage.Execute("export", certmanage.Options{
		ServerConfig:    srvCfg,
		ClientConfigDir: cfgDir,
		Name:            "alice",
		Out:             aliceOut,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := certmanage.Execute("export", certmanage.Options{
		ServerConfig:    srvCfg,
		ClientConfigDir: cfgDir,
		Name:            "bob",
		Out:             bobOut,
	}); err != nil {
		t.Fatal(err)
	}

	exeDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(exeDir, 0700); err != nil {
		t.Fatal(err)
	}

	// 自动扫描并导入激活 alice
	cfgAlice, err := Resolve(exeDir, root, aliceOut, "", true, nil)
	if err != nil {
		t.Fatalf("Resolve alice: %v", err)
	}
	// 删除原导出包后仍可从 .sft 加载已记住身份
	_ = os.RemoveAll(aliceOut)
	activeCfg, err := Active(exeDir)
	if err != nil || activeCfg.TLS.Certificate != cfgAlice.TLS.Certificate {
		t.Fatalf("Active mismatch after removing source: %v", err)
	}

	// 准备导入 bob 并通过 SelectPrepared 切换与自定义标签
	bobBundle := filepath.Join(bobOut, "client.bob.local.json")
	bobProf, err := PrepareImport(exeDir, root, bobBundle)
	if err != nil {
		t.Fatalf("PrepareImport bob: %v", err)
	}
	if _, err := SelectPrepared(exeDir, bobProf, "生产测试身份-bob"); err != nil {
		t.Fatalf("SelectPrepared: %v", err)
	}

	list, err := Profiles(exeDir)
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 stored profiles, got %d (%v)", len(list), err)
	}
	loadedBob, err := Resolve(exeDir, root, "", "", false, nil)
	if err != nil {
		t.Fatalf("Resolve saved bob: %v", err)
	}
	if _, err := config.LoadClient(loadedBob.TLS.Certificate); err == nil {
		t.Fatal("expected certificate not to parse as client JSON")
	}
}

func makeIdentityFixture(t *testing.T) (string, string, config.Client) {
	t.Helper()
	base := t.TempDir()
	certs := filepath.Join(base, "certs")
	if err := testcert.Generate(certs); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certs, "alice"); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultClient()
	cfg.TLS = config.TLSFiles{CACertificate: filepath.Join(certs, "ca.pem"), Certificate: filepath.Join(certs, "alice.pem"), PrivateKey: filepath.Join(certs, "alice-key.pem")}
	cfg.StateDir = filepath.Join(base, "state")
	path := filepath.Join(base, "client.alice.json")
	if err := config.SaveJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "app"), path, cfg
}

func TestSelectLabelValidationPreservesActiveIdentity(t *testing.T) {
	exe, path, _ := makeIdentityFixture(t)
	profile, err := PrepareImport(exe, filepath.Dir(path), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SelectPrepared(exe, profile, "original"); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(exe, ".sft", "active")
	before, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"", "   ", "bad\nlabel", "bad\x00label", strings.Repeat("界", 81)} {
		t.Run(fmt.Sprintf("%q", label), func(t *testing.T) {
			if _, err := Select(exe, profile.ID, label); err == nil {
				t.Error("invalid identity label accepted")
			}
			after, err := os.ReadFile(activePath)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("invalid selection changed active identity: %q %v", after, err)
			}
		})
	}
	if _, err := Select(exe, profile.ID, strings.Repeat("界", 80)); err != nil {
		t.Errorf("80-rune label rejected: %v", err)
	}
}

func TestIdentityIndexRejectsTraversalAndMalformedHex(t *testing.T) {
	for _, id := range []string{"", "../escape", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("G", 64), strings.Repeat("A", 64)} {
		t.Run(id, func(t *testing.T) {
			if _, err := profilePath(t.TempDir(), id); err == nil {
				t.Errorf("invalid profile ID accepted: %q", id)
			}
		})
	}
}

func TestSelectHandlesNullLabelFileWithoutPanic(t *testing.T) {
	exe, path, _ := makeIdentityFixture(t)
	profile, err := PrepareImport(exe, filepath.Dir(path), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exe, ".sft", "profile-labels.json"), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	// BUG: json.Unmarshal(null, &map) 得到 nil map，Select 当前直接赋值会 panic。
	defer func() {
		if value := recover(); value != nil {
			t.Errorf("BUG: malformed label metadata panicked: %v", value)
		}
	}()
	_, _ = SelectPrepared(exe, profile, "alice") // 可以返回校验错误，也可以初始化空标签表。
}

func TestSelectPreparedRevalidatesPrivateMaterial(t *testing.T) {
	exe, path, _ := makeIdentityFixture(t)
	profile, err := PrepareImport(exe, filepath.Dir(path), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile.Config.TLS.PrivateKey, []byte("invalid private key"), 0600); err != nil {
		t.Fatal(err)
	}
	// BUG: SelectPrepared 只比较配置路径，预览后的证书/私钥替换没有重新验证。
	if _, err := SelectPrepared(exe, profile, "alice"); err == nil {
		t.Error("BUG: identity with damaged private key was activated")
	}
	if _, err := os.Stat(filepath.Join(exe, ".sft", "active")); !os.IsNotExist(err) {
		t.Errorf("invalid identity changed active pointer: %v", err)
	}
}

func TestInvalidImportPreservesExistingActiveProfile(t *testing.T) {
	for _, damage := range []string{"key_mismatch", "untrusted_ca", "wrong_usage"} {
		t.Run(damage, func(t *testing.T) {
			exe, path, cfg := makeIdentityFixture(t)
			active, err := Resolve(exe, filepath.Dir(path), "", path, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			certDir := filepath.Dir(cfg.TLS.Certificate)
			switch damage {
			case "key_mismatch":
				cfg.TLS.PrivateKey = filepath.Join(certDir, "server-key.pem")
			case "wrong_usage":
				cfg.TLS.Certificate = filepath.Join(certDir, "server.pem")
				cfg.TLS.PrivateKey = filepath.Join(certDir, "server-key.pem")
			case "untrusted_ca":
				other := filepath.Join(t.TempDir(), "other")
				if err := testcert.Generate(other); err != nil {
					t.Fatal(err)
				}
				cfg.TLS.CACertificate = filepath.Join(other, "ca.pem")
			}
			if err := config.SaveJSON(path, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := PrepareImport(exe, filepath.Dir(path), path); err == nil {
				t.Error("invalid certificate material imported")
			}
			after, err := Active(exe)
			if err != nil || after != active {
				t.Errorf("failed import changed remembered identity: %v", err)
			}
		})
	}
}

func TestDiscoverDeduplicatesAndSkipsHiddenBundles(t *testing.T) {
	_, path, cfg := makeIdentityFixture(t)
	base := filepath.Dir(path)
	hidden := filepath.Join(base, ".hidden", "client.hidden.json")
	if err := os.MkdirAll(filepath.Dir(hidden), 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveJSON(hidden, cfg); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveJSON(filepath.Join(base, "client.example.json"), cfg); err != nil {
		t.Fatal(err)
	}
	paths, err := Discover(base, base, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != path {
		t.Errorf("discovery duplicates or includes hidden/example profiles: %q", paths)
	}
}
