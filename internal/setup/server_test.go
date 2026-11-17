package setup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/security"
)

func serverFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	o := certmanage.Options{
		Dir:              filepath.Join(dir, "certs"),
		ServerConfig:     filepath.Join(dir, "server.local.json"),
		ClientConfigDir:  dir,
		Name:             "alice",
		Days:             30,
		CADays:           365,
		Hosts:            "localhost,127.0.0.1",
		CAName:           "Example CA",
		ServerCommonName: "Example transfer",
	}
	if _, err := certmanage.Execute("init", o); err != nil {
		t.Fatal(err)
	}
	return dir, o.ServerConfig
}

func TestServerInitFindsMatchingCertificatesWithoutExampleConfig(t *testing.T) {
	dir, existing := serverFixture(t)
	old, _ := config.LoadServer(existing)
	ca, _ := security.ReadCertificate(old.TLS.CACertificate)
	leaf, _ := security.ReadCertificate(old.TLS.Certificate)
	if ca.Subject.CommonName != "Example CA" || leaf.Subject.CommonName != "Example transfer" {
		t.Fatal("custom certificate names were lost")
	}
	matches, err := ScanServerTLS(dir)
	if err != nil || len(matches) != 1 || matches[0] != old.TLS {
		t.Fatalf("matched wrong server certificates: %+v %v", matches, err)
	}
	path := filepath.Join(dir, "new-config", "server.json")
	if err := Server(strings.NewReader(strings.Repeat("\n", 12)), io.Discard, path, dir); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadServerDraft(path)
	if err != nil || c.TLS != old.TLS || len(c.AuthorizedClients) != 0 || !filepath.IsAbs(c.StorageRoot) {
		t.Fatalf("incorrect initialized configuration: %+v %v", c, err)
	}
	if _, err := config.LoadServer(path); err == nil {
		t.Fatal("new server silently authorized discovered client certificates")
	}
	if _, _, err := security.PrepareServerTLS(c); err != nil {
		t.Fatal(err)
	}
}

func TestServerInitPreservesUsersAndDoesNotWriteOnEOF(t *testing.T) {
	dir, path := serverFixture(t)
	before, _ := os.ReadFile(path)
	if err := Server(strings.NewReader("\n\n"), io.Discard, path, dir); err == nil {
		t.Fatal("interrupted setup succeeded")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("interrupted setup changed existing configuration")
	}
	old, _ := config.LoadServer(path)
	if err := Server(strings.NewReader(strings.Repeat("\n", 12)), io.Discard, path, dir); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadServer(path)
	if err != nil || len(c.AuthorizedClients) != 1 || c.AuthorizedClients[0].Certificate != old.AuthorizedClients[0].Certificate || c.Resources != old.Resources {
		t.Fatalf("existing settings were lost: %+v %v", c, err)
	}

	// 开启高级参数输入 y 后可完整配置 Limits/Timeouts/Resources
	advInput := strings.Repeat("\n", 10) + "y\n" + strings.Repeat("\n", 30)
	if err := Server(strings.NewReader(advInput), io.Discard, path, dir); err != nil {
		t.Fatal(err)
	}
}

func TestServerInitSupportsUnitsGlobalBandwidthWithoutDuplicatePrompts(t *testing.T) {
	dir, path := serverFixture(t)
	// 5 项路径/监听/存储采用默认，接下来 5 项支持单位输入：默认上传、下载、突发、单文件、用户配额，随后进入高级配置并启用全局总带宽
	inputs := strings.Join([]string{
		"", "", "", "", "", // CA, cert, key, listen, storage
		"10 MiB/s", "20 MiB/s", "2 MiB", "5 GiB", "10 GiB", // 5 项带宽/存储配额
		"y",                         // 进入高级配置
		"y",                         // 启用全局总带宽
		"50 MiB/s", "60 MiB/s", "4 MiB", // 全局总带宽 3 项
	}, "\n") + "\n" + strings.Repeat("\n", 25)

	var out bytes.Buffer
	if err := Server(strings.NewReader(inputs), &out, path, dir); err != nil {
		t.Fatalf("Server setup with units & global bandwidth failed: %v\nout=%s", err, out.String())
	}
	c, err := config.LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bandwidth.UploadBytesPerSecond != 10<<20 || c.Bandwidth.DownloadBytesPerSecond != 20<<20 || c.Bandwidth.BurstBytes != 2<<20 {
		t.Fatalf("unexpected default bandwidth parsed: %+v", c.Bandwidth)
	}
	if c.Resources.MaxFileBytes != 5<<30 || c.Resources.UserStorageBytes != 10<<30 {
		t.Fatalf("unexpected resource sizes parsed: %+v", c.Resources)
	}
	if c.GlobalBandwidth == nil || c.GlobalBandwidth.UploadBytesPerSecond != 50<<20 || c.GlobalBandwidth.DownloadBytesPerSecond != 60<<20 || c.GlobalBandwidth.BurstBytes != 4<<20 {
		t.Fatalf("unexpected global bandwidth parsed: %+v", c.GlobalBandwidth)
	}
	// 确认提示文本中没有再次重复出现默认带宽与文件上限 JSON 字段名
	outStr := out.String()
	for _, dup := range []string{"bandwidth.upload_bytes_per_second", "resources.max_file_bytes", "resources.user_storage_bytes"} {
		if strings.Contains(outStr, dup) {
			t.Errorf("setup wizard prompted duplicate field %q", dup)
		}
	}
}

func TestParseBytesExactRationalBoundaries(t *testing.T) {
	if got, err := ParseBytes("10 MiB/s"); err != nil || got != 10<<20 {
		t.Fatalf("ParseBytes(10 MiB/s) = %d, %v", got, err)
	}
	if _, err := ParseBytes("0.5 B"); err == nil {
		t.Fatal("accepted fractional byte")
	}
}
