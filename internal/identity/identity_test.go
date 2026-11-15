package identity

import (
	"os"
	"path/filepath"
	"testing"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
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
