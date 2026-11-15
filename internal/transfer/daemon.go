package transfer

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/monitor"
	"sft-tui/internal/ratelimit"
	"sft-tui/internal/security"
	"sft-tui/internal/storage"
)

// Daemon 集成存储引擎、mTLS 授权器、全球/单用户令牌桶限流、运行监控与配置热重载，
// 复用 Server 的 SFT/1 连接处理与传输协议逻辑，实现完整的 sftd 服务端生命周期。
type Daemon struct {
	cfg       config.Server
	server    *Server
	store     *storage.Store
	auth      *security.Authorizer
	rates     *ratelimit.Users
	runtime   *monitor.Runtime
	logger    *slog.Logger
	reloadMu  sync.Mutex
	lifecycle sync.Mutex
	listener  net.Listener
	force     chan struct{}
	forceOnce sync.Once
}

func NewDaemon(c config.Server) (*Daemon, error) {
	return NewDaemonWithLogger(c, slog.Default())
}

func NewDaemonWithLogger(c config.Server, logger *slog.Logger) (*Daemon, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	tlsCfg, auth, err := security.ServerTLS(c)
	if err != nil {
		return nil, err
	}
	store, err := storage.OpenStore(c.StorageRoot)
	if err != nil {
		return nil, err
	}
	rates := ratelimit.New(
		c.Resources.MaxStreamsPerUser,
		c.Resources.MaxNewTasksPerMinute,
		c.Resources.MaxUploadCheckpointsPerSecond,
		c.GlobalBandwidth,
	)
	rt := monitor.New()
	hooks := ServerHooks{
		AcquireStream:    rates.Acquire,
		WaitBandwidth:    rates.Wait,
		WaitCheckpoint:   rates.WaitUploadCheckpoint,
		OnStreamBegin:    rt.Begin,
		OnStreamDescribe: rt.Describe,
		OnStreamProgress: rt.Progress,
		OnStreamEnd:      rt.End,
	}
	srv, err := NewServer(c, store, auth, tlsCfg, hooks, logger)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Daemon{
		cfg:     c,
		server:  srv,
		store:   store,
		auth:    auth,
		rates:   rates,
		runtime: rt,
		logger:  logger,
		force:   make(chan struct{}),
	}, nil
}

func (d *Daemon) Close() error {
	return d.store.Close()
}

// Serve 监听连接并强制执行全局连接上限、每 IP 未认证连接上限、并发 TLS 握手上界与单用户连接上限，
// 同时运行过期任务清理与存储用量采样，直到收到停服请求或 ctx 取消。
func (d *Daemon) Serve(ctx context.Context, listener net.Listener) error {
	d.lifecycle.Lock()
	d.listener = listener
	d.runtime.Listening(listener.Addr().String())
	if d.runtime.Snapshot().State != "running" {
		_ = listener.Close()
	}
	d.lifecycle.Unlock()

	connections := make(chan struct{}, d.cfg.Resources.MaxConnections)
	handshakes := make(chan struct{}, d.cfg.Resources.MaxHandshakes)

	var mu sync.Mutex
	var wg sync.WaitGroup
	active := make(map[net.Conn]struct{})
	unauthIP := make(map[net.Conn]string)
	unauthCount := make(map[string]uint32)
	connUsers := make(map[net.Conn]string)
	userCount := make(map[string]uint32)
	decMap := func(m map[string]uint32, k string) {
		if m[k] <= 1 {
			delete(m, k)
		} else {
			m[k]--
		}
	}

	stopped := make(chan struct{})
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		interval := max(500*time.Millisecond, min(time.Minute, time.Duration(d.cfg.Resources.TransferRetentionSeconds)*time.Second/2))
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := d.store.ExpireTasks(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
					d.logger.Error("任务清理失败", "error", err)
				}
			}
		}
	}()

	usageDone := make(chan struct{})
	go func() {
		defer close(usageDone)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			sampleCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			files, bytes, err := d.store.PublishedUsage(sampleCtx)
			cancel()
			d.runtime.Storage(files, bytes, err)
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	defer func() {
		close(stopped)
		<-maintenanceDone
		<-usageDone
	}()

	go func() {
		select {
		case <-ctx.Done():
			d.Stop(false)
		case <-stopped:
		}
	}()

	defer func() {
		_ = listener.Close()
		drained := d.runtime.Drain(false)
		select {
		case <-drained:
		case <-d.force:
		}
		mu.Lock()
		for conn := range active {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()

	gate := &connGate{
		acquireHandshake: func() (func(), bool) {
			select {
			case handshakes <- struct{}{}:
				var once sync.Once
				return func() { once.Do(func() { <-handshakes }) }, true
			default:
				return nil, false
			}
		},
		onAuthenticate: func(raw net.Conn, user string) (uint64, bool) {
			mu.Lock()
			if ip, ok := unauthIP[raw]; ok {
				delete(unauthIP, raw)
				decMap(unauthCount, ip)
			}
			if userCount[user] >= d.cfg.Resources.MaxConnectionsPerUser {
				mu.Unlock()
				d.logger.Warn("拒绝连接：单用户连接已达上限", "user", user, "remote", raw.RemoteAddr())
				return 0, false
			}
			remote := ""
			if addr := raw.RemoteAddr(); addr != nil {
				remote = addr.String()
			}
			cid, ok := d.runtime.Connect(user, remote)
			if !ok {
				mu.Unlock()
				return 0, false
			}
			userCount[user]++
			connUsers[raw] = user
			mu.Unlock()
			return cid, true
		},
		onDisconnect: func(_ net.Conn, cid uint64) {
			d.runtime.Disconnect(cid)
		},
	}

	for {
		raw, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			d.Stop(true)
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			d.logger.Debug("拒绝连接：全局并发连接已满", "remote", raw.RemoteAddr())
			_ = raw.Close()
			continue
		}
		ip, _, err := net.SplitHostPort(raw.RemoteAddr().String())
		if err != nil {
			ip = raw.RemoteAddr().String()
		}
		mu.Lock()
		if unauthCount[ip] >= d.cfg.Resources.MaxUnauthenticatedPerIP {
			mu.Unlock()
			d.logger.Debug("拒绝连接：来源 IP 未认证连接已满", "remote", raw.RemoteAddr())
			_ = raw.Close()
			<-connections
			continue
		}
		unauthCount[ip]++
		unauthIP[raw] = ip
		active[raw] = struct{}{}
		mu.Unlock()

		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer func() {
				_ = c.Close()
				mu.Lock()
				delete(active, c)
				if prevIP, ok := unauthIP[c]; ok {
					delete(unauthIP, c)
					decMap(unauthCount, prevIP)
				}
				if u, ok := connUsers[c]; ok {
					delete(connUsers, c)
					decMap(userCount, u)
				}
				mu.Unlock()
				<-connections
			}()
			d.server.handleConn(ctx, c, gate)
		}(raw)
	}
}

func (d *Daemon) Reload(c config.Server) error { return d.reload(c, false) }

// ApplySaved 热更新证书、授权用户列表与全局限速配置，保留需重启生效的字段。
func (d *Daemon) ApplySaved(c config.Server) error { return d.reload(c, true) }

func (d *Daemon) reload(c config.Server, allowPending bool) error {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()
	if err := c.Validate(); err != nil {
		return err
	}
	if !allowPending && config.RestartRequired(d.cfg, c) {
		return errors.New("热更新仅支持 TLS 证书、authorized_clients 与 global_bandwidth；其余配置需重启")
	}
	merged := config.MergeHot(d.cfg, c)
	tlsCfg, ca, err := security.PrepareServerTLS(merged)
	if err != nil {
		return err
	}
	if err := d.auth.ReloadWithCA(merged.AuthorizedClients, ca); err != nil {
		return err
	}
	security.BindAuthorizer(tlsCfg, d.auth)
	d.rates.SetGlobal(merged.GlobalBandwidth)
	d.server.UpdateTLS(tlsCfg)
	d.cfg.TLS = merged.TLS
	d.cfg.AuthorizedClients = merged.AuthorizedClients
	d.cfg.GlobalBandwidth = merged.GlobalBandwidth
	return nil
}

func (d *Daemon) Snapshot() monitor.Snapshot {
	snap := d.runtime.Snapshot()
	d.reloadMu.Lock()
	snap.Config = d.cfg
	snap.Config.AuthorizedClients = slices.Clone(d.cfg.AuthorizedClients)
	for i := range snap.Config.AuthorizedClients {
		u := &snap.Config.AuthorizedClients[i]
		u.Permissions = slices.Clone(u.Permissions)
		if u.Bandwidth != nil {
			b := *u.Bandwidth
			u.Bandwidth = &b
		}
	}
	if d.cfg.GlobalBandwidth != nil {
		gb := *d.cfg.GlobalBandwidth
		snap.Config.GlobalBandwidth = &gb
	}
	d.reloadMu.Unlock()
	return snap
}

func (d *Daemon) Stop(immediate bool) {
	d.lifecycle.Lock()
	d.runtime.Drain(immediate)
	if immediate {
		d.forceOnce.Do(func() { close(d.force) })
	}
	if d.listener != nil {
		_ = d.listener.Close()
	}
	d.lifecycle.Unlock()
	d.logger.Info("收到服务停止指令", "immediate", immediate)
}
