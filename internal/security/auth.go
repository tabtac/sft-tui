package security

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"sync"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/protocol"
)

// Principal 描述通过 mTLS 证书指纹绑定并授权的客户端身份主体。
type Principal struct {
	UserID            string
	Disabled          bool
	Permissions       []string
	Bandwidth         config.Bandwidth
	NotAfter          time.Time
	CertFingerprint   [32]byte
	PubKeyFingerprint [32]byte
	Subject           string
	DNSNames          []string
	IPAddresses       []string
}

func (p Principal) clone() Principal {
	if p.Permissions != nil {
		p.Permissions = append([]string(nil), p.Permissions...)
	}
	if p.DNSNames != nil {
		p.DNSNames = append([]string(nil), p.DNSNames...)
	}
	if p.IPAddresses != nil {
		p.IPAddresses = append([]string(nil), p.IPAddresses...)
	}
	return p
}

func (p Principal) validAt(now time.Time) bool {
	return !p.Disabled && now.Before(p.NotAfter)
}

func (p Principal) Allows(operation string) bool {
	if !p.validAt(time.Now()) {
		return false
	}
	if p.Permissions == nil {
		return true
	}
	for _, allowed := range p.Permissions {
		if allowed == operation {
			return true
		}
	}
	return false
}

type registeredConnection struct {
	user  string
	timer *time.Timer
}

// Authorizer 维护证书指纹到授权主体的映射，支持安全热更新并按有效期踢出过期连接。
type Authorizer struct {
	mu          sync.RWMutex
	ca          *x509.CertPool
	fallback    config.Bandwidth
	certs       map[[32]byte]Principal
	users       map[string]Principal
	connections map[net.Conn]*registeredConnection
}

func NewAuthorizer(c config.Server, ca *x509.CertPool) (*Authorizer, error) {
	a := &Authorizer{
		ca:          ca,
		fallback:    c.Bandwidth,
		connections: make(map[net.Conn]*registeredConnection),
	}
	if err := a.Reload(c.AuthorizedClients); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Authorizer) Reload(entries []config.AuthorizedClient) error {
	a.mu.RLock()
	ca := a.ca
	a.mu.RUnlock()
	return a.ReloadWithCA(entries, ca)
}

func (a *Authorizer) ReloadWithCA(entries []config.AuthorizedClient, ca *x509.CertPool) error {
	certs := make(map[[32]byte]Principal)
	users := make(map[string]Principal)
	now := time.Now()

	for _, u := range entries {
		if !config.ValidUserID(u.UserID) {
			return fmt.Errorf("非法用户 ID：%q", u.UserID)
		}
		if _, ok := users[u.UserID]; ok {
			return fmt.Errorf("重复用户：%q", u.UserID)
		}

		data, err := pal.ReadSmall(u.Certificate, MaxCertFileBytes)
		if err != nil {
			return err
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return fmt.Errorf("授权文件不是证书：%s", u.Certificate)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}

		intermediates := x509.NewCertPool()
		for len(rest) > 0 {
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				return fmt.Errorf("授权证书链包含非证书 PEM 块：%s", u.Certificate)
			}
			issuer, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return err
			}
			intermediates.AddCert(issuer)
		}

		expired := !now.Before(cert.NotAfter)
		validUntil := cert.NotAfter
		if !u.Disabled {
			chains, err := verifyAuthorizationChain(cert, ca, intermediates, now)
			if err != nil {
				return err
			}
			// 连接有效期不得长于验证链中的任何上游证书
			for _, issuer := range chains[0] {
				if issuer.NotAfter.Before(validUntil) {
					validUntil = issuer.NotAfter
				}
			}
			expired = !now.Before(validUntil)
		}

		bandwidth := a.fallback
		if u.Bandwidth != nil {
			bandwidth = *u.Bandwidth
		}

		permissions := u.Permissions
		if permissions != nil {
			permissions = append([]string{}, permissions...)
			sort.Strings(permissions)
			n := 0
			for _, p := range permissions {
				if n == 0 || p != permissions[n-1] {
					permissions[n] = p
					n++
				}
			}
			permissions = permissions[:n]
		}

		var ips []string
		for _, ip := range cert.IPAddresses {
			ips = append(ips, ip.String())
		}

		certFP := CertFingerprint(cert)
		if _, ok := certs[certFP]; ok {
			return fmt.Errorf("重复授权证书指纹")
		}

		p := Principal{
			UserID:            u.UserID,
			Disabled:          u.Disabled || expired,
			Permissions:       permissions,
			Bandwidth:         bandwidth,
			NotAfter:          validUntil,
			CertFingerprint:   certFP,
			PubKeyFingerprint: PubKeyFingerprint(cert),
			Subject:           cert.Subject.String(),
			DNSNames:          append([]string(nil), cert.DNSNames...),
			IPAddresses:       ips,
		}
		certs[certFP] = p
		users[u.UserID] = p
	}

	a.mu.Lock()
	old := a.users
	oldCerts := a.certs
	var closeList []net.Conn
	changedCertUsers := make(map[string]bool)
	for fp, p := range oldCerts {
		q, ok := certs[fp]
		if !ok || q.UserID != p.UserID {
			changedCertUsers[p.UserID] = true
		}
	}
	for conn, reg := range a.connections {
		user := reg.user
		p, exists := users[user]
		if !exists || !p.validAt(now) || !reflect.DeepEqual(old[user], p) || changedCertUsers[user] {
			reg.timer.Stop()
			delete(a.connections, conn)
			closeList = append(closeList, conn)
		}
	}
	a.certs, a.users = certs, users
	a.ca = ca
	a.mu.Unlock()

	for _, conn := range closeList {
		_ = conn.Close()
	}
	return nil
}

func verifyAuthorizationChain(cert *x509.Certificate, roots, intermediates *x509.CertPool, now time.Time) ([][]*x509.Certificate, error) {
	verifyTime := now
	for {
		chains, err := cert.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			CurrentTime:   verifyTime,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err == nil {
			return chains, nil
		}
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &invalid) || invalid.Reason != x509.Expired || invalid.Cert == nil || !verifyTime.After(invalid.Cert.NotAfter) {
			return nil, err
		}
		verifyTime = invalid.Cert.NotAfter
	}
}

// Certificate 验证对端客户端证书指纹及有效期（并校验公钥指纹与主体匹配）。
func (a *Authorizer) Certificate(cert *x509.Certificate) (Principal, error) {
	a.mu.RLock()
	p, ok := a.certs[CertFingerprint(cert)]
	a.mu.RUnlock()
	if !ok || !p.validAt(time.Now()) || p.PubKeyFingerprint != PubKeyFingerprint(cert) {
		return p.clone(), &protocol.RemoteError{Code: protocol.CodeUnauthorized, Message: "客户端证书未授权或用户已禁用"}
	}
	return p.clone(), nil
}

// Check 检查指定用户是否具备操作权限。
func (a *Authorizer) Check(user, operation string) (Principal, error) {
	a.mu.RLock()
	p, ok := a.users[user]
	a.mu.RUnlock()
	if !ok || !p.Allows(operation) {
		return p.clone(), &protocol.RemoteError{Code: protocol.CodeUnauthorized, Message: "用户没有此操作权限"}
	}
	return p.clone(), nil
}

// CheckBandwidth 零分配检查指定用户是否具备操作权限并返回当前有效带宽策略，适用于传输热路径。
func (a *Authorizer) CheckBandwidth(user, operation string) (config.Bandwidth, error) {
	a.mu.RLock()
	p, ok := a.users[user]
	allowed := ok && p.Allows(operation)
	bw := p.Bandwidth
	a.mu.RUnlock()
	if !allowed {
		return config.Bandwidth{}, &protocol.RemoteError{Code: protocol.CodeUnauthorized, Message: "用户没有此操作权限"}
	}
	return bw, nil
}

// Register 注册已建立 mTLS 连接的到期定时器，并在证书过期时主动断连。
func (a *Authorizer) Register(cert *x509.Certificate, conn net.Conn) (Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	p, ok := a.certs[CertFingerprint(cert)]
	if !ok || !p.validAt(time.Now()) || p.PubKeyFingerprint != PubKeyFingerprint(cert) {
		return p.clone(), fmt.Errorf("用户已撤销或证书不匹配")
	}
	if old := a.connections[conn]; old != nil {
		old.timer.Stop()
	}
	reg := &registeredConnection{user: p.UserID}
	a.connections[conn] = reg
	reg.timer = time.AfterFunc(time.Until(p.NotAfter), func() {
		a.mu.Lock()
		current := a.connections[conn] == reg
		if current {
			delete(a.connections, conn)
		}
		a.mu.Unlock()
		if current {
			_ = conn.Close()
		}
	})
	return p.clone(), nil
}

func (a *Authorizer) Unregister(conn net.Conn) {
	a.mu.Lock()
	if reg := a.connections[conn]; reg != nil {
		reg.timer.Stop()
	}
	delete(a.connections, conn)
	a.mu.Unlock()
}
