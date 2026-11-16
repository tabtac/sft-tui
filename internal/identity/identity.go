// Package identity 管理客户端便携证书包的扫描发现、校验导入、多身份切换与标签存储。
package identity

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/prompt"
	"sft-tui/internal/security"
)

type Profile struct {
	ID          string
	Label       string
	Path        string
	User        string
	Fingerprint string
	Expires     time.Time
	Config      config.Client
}

type material struct {
	cfg           config.Client
	ca, cert, key []byte
	leaf          *x509.Certificate
}

// Resolve 解析并载入当前客户端配置；指定 explicitConfig（非 init）时直接读取该文件。
func Resolve(executableDir, workingDir, sourceDir, explicitConfig string, initialize bool, out io.Writer) (config.Client, error) {
	return ResolveInteractive(executableDir, workingDir, sourceDir, explicitConfig, initialize, nil, out)
}

// ResolveInteractive 与 Resolve 相同，但在发现多个候选身份且提供交互输入 in 时允许用户编号选择。
func ResolveInteractive(executableDir, workingDir, sourceDir, explicitConfig string, initialize bool, in io.Reader, out io.Writer) (config.Client, error) {
	if explicitConfig != "" && sourceDir != "" {
		return config.Client{}, fmt.Errorf("-config 与 -dir 不能同时使用")
	}
	if explicitConfig != "" && !initialize {
		c, err := config.LoadClient(explicitConfig)
		if err != nil {
			return c, err
		}
		return resolveLegacyPaths(c, explicitConfig, workingDir), nil
	}
	home := filepath.Join(executableDir, ".sft")
	if !initialize && sourceDir == "" {
		cfg, err := loadActive(home)
		if !os.IsNotExist(err) {
			return cfg, err
		}
	}

	target := explicitConfig
	if target == "" {
		candidates, err := Discover(executableDir, workingDir, sourceDir)
		if err != nil {
			return config.Client{}, fmt.Errorf("自动扫描身份包失败：%w", err)
		}
		if len(candidates) == 0 && sourceDir == "" {
			saved, err := Profiles(executableDir)
			if err != nil {
				return config.Client{}, err
			}
			for _, p := range saved {
				candidates = append(candidates, p.Path)
			}
		}
		if len(candidates) == 0 {
			return config.Client{}, fmt.Errorf("未找到客户端配置；请将导出包放到 sft 同目录，或通过 -dir / -config 指定")
		}
		if len(candidates) > 1 {
			if in != nil && out != nil {
				labels := make([]string, len(candidates))
				for i, p := range candidates {
					if prof, e := Preview(p, workingDir); e == nil {
						labels[i] = prof.Label + "  (" + p + ")"
					} else {
						labels[i] = p
					}
				}
				idx, err := prompt.New(in, out).Choose("发现多个客户端身份，请选择要激活的配置", labels)
				if err != nil {
					return config.Client{}, err
				}
				target = candidates[idx]
			} else {
				return config.Client{}, fmt.Errorf("发现多个可用身份配置（%d 个），请通过 -config / -dir 指定，或在 TUI 界面按 i 选择", len(candidates))
			}
		} else {
			target = candidates[0]
		}
	}

	c, err := importSnapshot(home, target, workingDir, true)
	if err != nil {
		return c, err
	}
	if out != nil {
		fmt.Fprintln(out, "客户端身份已导入并激活："+target)
	}
	return c, nil
}

// Discover 扫描指定目录、工作目录或可执行文件旁的有效客户端配置文件（depth <= 2）。
func Discover(executableDir, workingDir, source string) ([]string, error) {
	roots := []string{workingDir, executableDir}
	if source != "" {
		info, err := os.Stat(source)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			abs, err := filepath.Abs(source)
			return []string{abs}, err
		}
		roots = []string{source}
	}

	seen := make(map[string]bool)
	var out []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		paths, err := discoverCandidates(root)
		if err != nil {
			if source != "" {
				return nil, err
			}
			continue
		}
		for _, p := range paths {
			abs, err := filepath.Abs(p)
			if err == nil && !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func discoverCandidates(root string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("扫描路径必须是目录：%s", absRoot)
	}

	var valid []string
	visited := 0
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		visited++
		if visited > 10000 {
			return fmt.Errorf("目录项超过 10000，请指定更精确的目录：%s", absRoot)
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != absRoot && (strings.Count(rel, string(filepath.Separator)) >= 2 ||
				strings.HasPrefix(entry.Name(), ".") ||
				entry.Name() == "node_modules" || entry.Name() == "var" || entry.Name() == "state") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if strings.HasPrefix(name, "client") && strings.HasSuffix(name, ".json") && !strings.Contains(name, ".example.") {
			if _, err := config.LoadClient(path); err == nil {
				valid = append(valid, path)
			}
		}
		return nil
	})
	return valid, err
}

// Preview 读取并校验证书与配置，不修改本地已记住身份。
func Preview(path, workingDir string) (Profile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Profile{}, err
	}
	m, err := readMaterial(abs, workingDir)
	if err != nil {
		return Profile{}, err
	}
	return Profile{
		Path:        abs,
		User:        m.leaf.Subject.CommonName,
		Label:       m.leaf.Subject.CommonName + "@" + m.cfg.ServerAddress,
		Fingerprint: security.CertFingerprintHex(m.leaf),
		Expires:     m.leaf.NotAfter,
		Config:      m.cfg,
	}, nil
}

// PrepareImport 将身份包安全导入到 .sft/profiles/<id> 中供切换或预览使用，但不立即切换 active。
func PrepareImport(executableDir, workingDir, path string) (Profile, error) {
	home := filepath.Join(executableDir, ".sft")
	c, err := importSnapshot(home, path, workingDir, false)
	if err != nil {
		return Profile{}, err
	}
	id := filepath.Base(filepath.Dir(c.TLS.Certificate))
	pPath, err := profilePath(home, id)
	if err != nil {
		return Profile{}, err
	}
	p, err := Preview(pPath, workingDir)
	if err != nil {
		return Profile{}, err
	}
	p.ID = id
	p.Config = c
	return p, nil
}

// Profiles 列出所有已安全导入到 .sft/profiles/ 的身份配置。
func Profiles(executableDir string) ([]Profile, error) {
	home := filepath.Join(executableDir, ".sft")
	entries, err := os.ReadDir(filepath.Join(home, "profiles"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names, err := readLabels(home)
	if err != nil {
		return nil, err
	}
	var result []Profile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pPath, err := profilePath(home, e.Name())
		if err != nil {
			continue
		}
		c, err := loadProfile(pPath)
		if err != nil {
			continue
		}
		cert, err := security.ReadCertificate(c.TLS.Certificate)
		if err != nil {
			continue
		}
		p := Profile{
			ID:          e.Name(),
			Path:        pPath,
			Config:      c,
			Label:       names[e.Name()],
			User:        cert.Subject.CommonName,
			Expires:     cert.NotAfter,
			Fingerprint: security.CertFingerprintHex(cert),
		}
		if p.Label == "" {
			p.Label = p.User + "@" + c.ServerAddress
		}
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Label < result[j].Label })
	return result, nil
}

func Select(executableDir, id, label string) (config.Client, error) {
	return selectExpected(executableDir, id, label, nil)
}

func SelectPrepared(executableDir string, p Profile, label string) (config.Client, error) {
	return selectExpected(executableDir, p.ID, label, &p.Config)
}

func Active(executableDir string) (config.Client, error) {
	return loadActive(filepath.Join(executableDir, ".sft"))
}

func selectExpected(executableDir, id, label string, expected *config.Client) (config.Client, error) {
	if strings.TrimSpace(label) == "" || len([]rune(label)) > 80 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return config.Client{}, fmt.Errorf("身份标签须为 1–80 个字符且不能包含控制字符")
	}
	home := filepath.Join(executableDir, ".sft")
	pPath, err := profilePath(home, id)
	if err != nil {
		return config.Client{}, err
	}
	c, err := loadProfile(pPath)
	if err != nil {
		return c, err
	}
	var out config.Client
	err = withIdentityLock(home, func() error {
		c, err := loadProfile(pPath)
		if err != nil {
			return err
		}
		if _, err := readMaterial(pPath, filepath.Dir(pPath)); err != nil {
			return err
		}
		if expected != nil && c != *expected {
			return fmt.Errorf("身份配置在预览后发生变化，请重新选择")
		}
		names, err := readLabels(home)
		if err != nil {
			return err
		}
		names[id] = strings.TrimSpace(label)
		labelData, err := json.MarshalIndent(names, "", "  ")
		if err != nil {
			return err
		}
		if err := pal.StageAndReplaceMode(filepath.Join(home, "profile-labels.json"), append(labelData, '\n'), 0600, false); err != nil {
			return err
		}
		if err := pal.StageAndReplaceMode(filepath.Join(home, "active"), []byte(id+"\n"), 0600, false); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

func withIdentityLock(home string, fn func() error) error {
	if err := pal.EnsurePrivateDir(home); err != nil {
		return fmt.Errorf("无法在程序旁保存身份（请确认目录可写）：%w", err)
	}
	lock, err := pal.OpenLockFile(filepath.Join(home, ".identity.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := pal.LockFile(lock); err != nil {
		return fmt.Errorf("另一个身份操作正在进行：%w", err)
	}
	defer func() { _ = pal.UnlockFile(lock) }()
	return fn()
}

func profilePath(home, id string) (string, error) {
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(id) != id {
		return "", fmt.Errorf("保存的身份索引无效，请重新导入")
	}
	return filepath.Join(home, "profiles", id, "client.local.json"), nil
}

func readLabels(home string) (map[string]string, error) {
	data, err := pal.ReadSmall(filepath.Join(home, "profile-labels.json"), config.MaxConfigBytes)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

func loadActive(home string) (config.Client, error) {
	data, err := pal.ReadSmall(filepath.Join(home, "active"), 256)
	if err != nil {
		return config.Client{}, err
	}
	pPath, err := profilePath(home, strings.TrimSpace(string(data)))
	if err != nil {
		return config.Client{}, err
	}
	c, err := loadProfile(pPath)
	if os.IsNotExist(err) {
		return c, fmt.Errorf("保存的身份材料缺失，请重新导入：%v", err)
	}
	return c, err
}

func loadProfile(path string) (config.Client, error) {
	c, err := config.LoadClient(path)
	if err != nil {
		return c, err
	}
	dir := filepath.Dir(path)
	for _, field := range []*string{&c.TLS.CACertificate, &c.TLS.Certificate, &c.TLS.PrivateKey, &c.StateDir} {
		if !filepath.IsAbs(*field) {
			*field = filepath.Join(dir, *field)
		}
	}
	return c, nil
}

func resolveLegacyPaths(c config.Client, configPath, workingDir string) config.Client {
	cfgDir := filepath.Dir(configPath)
	resolve := func(name string) string {
		if filepath.IsAbs(name) {
			return name
		}
		local := filepath.Join(cfgDir, name)
		if _, err := os.Stat(local); err == nil {
			return local
		}
		return filepath.Join(workingDir, name)
	}
	c.TLS.CACertificate = resolve(c.TLS.CACertificate)
	c.TLS.Certificate = resolve(c.TLS.Certificate)
	c.TLS.PrivateKey = resolve(c.TLS.PrivateKey)
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(workingDir, c.StateDir)
	}
	return c
}

func readMaterial(path, workingDir string) (material, error) {
	c, err := config.LoadClient(path)
	if err != nil {
		return material{}, err
	}
	c = resolveLegacyPaths(c, path, workingDir)
	ca, err := pal.ReadSmall(c.TLS.CACertificate, security.MaxCertFileBytes)
	if err != nil {
		return material{}, err
	}
	cert, err := pal.ReadSmall(c.TLS.Certificate, security.MaxCertFileBytes)
	if err != nil {
		return material{}, err
	}
	key, err := pal.ReadSmall(c.TLS.PrivateKey, security.MaxCertFileBytes)
	if err != nil {
		return material{}, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return material{}, fmt.Errorf("用户证书与私钥不匹配：%w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return material{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return material{}, fmt.Errorf("CA 文件不含有效证书")
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		inter, err := x509.ParseCertificate(der)
		if err != nil {
			return material{}, err
		}
		intermediates.AddCert(inter)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil || leaf.IsCA {
		return material{}, fmt.Errorf("用户证书不受 CA 信任、用途错误或已过期：%v", err)
	}
	return material{cfg: c, ca: ca, cert: cert, key: key, leaf: leaf}, nil
}

func importSnapshot(home, path, workingDir string, selectActive bool) (config.Client, error) {
	m, err := readMaterial(path, workingDir)
	if err != nil {
		return config.Client{}, err
	}
	c, ca, cert, key, leaf := m.cfg, m.ca, m.cert, m.key, m.leaf
	leafID := security.CertFingerprintHex(leaf)
	c.TLS = config.TLSFiles{CACertificate: "ca.pem", Certificate: "cert.pem", PrivateKey: "key.pem"}
	c.StateDir = filepath.Join("..", "..", "state", leafID)

	canonical, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	h := sha256.New()
	h.Write(canonical)
	h.Write(ca)
	h.Write(cert)
	id := hex.EncodeToString(h.Sum(nil))
	pPath, err := profilePath(home, id)
	if err != nil {
		return c, err
	}

	err = withIdentityLock(home, func() error {
		profilesDir := filepath.Join(home, "profiles")
		if err := pal.EnsurePrivateDir(profilesDir); err != nil {
			return err
		}
		targetDir := filepath.Dir(pPath)
		if _, statErr := os.Lstat(targetDir); os.IsNotExist(statErr) {
			staging, err := os.MkdirTemp(profilesDir, ".import-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(staging)
			if err := pal.EnsurePrivateDir(staging); err != nil {
				return err
			}
			for _, f := range []struct {
				name string
				data []byte
			}{{"ca.pem", ca}, {"cert.pem", cert}, {"key.pem", key}} {
				if err := pal.StageAndReplaceMode(filepath.Join(staging, f.name), f.data, 0600, true); err != nil {
					return err
				}
			}
			cfgBytes, err := json.MarshalIndent(c, "", "  ")
			if err != nil {
				return err
			}
			if err := pal.StageAndReplaceMode(filepath.Join(staging, "client.local.json"), append(cfgBytes, '\n'), 0600, true); err != nil {
				return err
			}
			if err := os.Rename(staging, targetDir); err != nil {
				return err
			}
			_ = pal.SyncParentDir(targetDir)
		} else if statErr != nil {
			return statErr
		} else {
			for _, f := range []struct {
				name string
				data []byte
			}{{"ca.pem", ca}, {"cert.pem", cert}, {"key.pem", key}} {
				stored, err := pal.ReadSmall(filepath.Join(targetDir, f.name), security.MaxCertFileBytes)
				if err != nil || !bytes.Equal(stored, f.data) {
					return fmt.Errorf("已有身份材料被修改或缺失：%s", pPath)
				}
			}
			stored, err := config.LoadClient(pPath)
			if err != nil || stored != c {
				return fmt.Errorf("已有身份配置被修改或缺失：%s", pPath)
			}
		}

		if selectActive {
			return pal.StageAndReplaceMode(filepath.Join(home, "active"), []byte(id+"\n"), 0600, false)
		}
		return nil
	})
	if err != nil {
		return config.Client{}, err
	}
	return loadProfile(pPath)
}
