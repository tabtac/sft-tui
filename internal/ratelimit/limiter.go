// Package ratelimit 实现基于令牌桶算法的全局与单用户带宽限速、流并发及操作频率限制。
package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
)

type allowance struct {
	upload      *rate.Limiter
	download    *rate.Limiter
	tasks       *rate.Limiter
	checkpoints *rate.Limiter
	policy      config.Bandwidth
	streams     uint32
}

type globalLimiters struct {
	configured bool
	policy     config.Bandwidth
	upload     *rate.Limiter
	download   *rate.Limiter
}

// Users 管理单用户及全局共享令牌桶，支持热更新动态调整速率与突发配额。
type Users struct {
	mu                   sync.Mutex
	users                map[string]*allowance
	global               globalLimiters
	maxStreams           uint32
	tasksPerMinute       uint32
	checkpointsPerSecond uint32
}

func New(maxStreams, tasksPerMinute, checkpointsPerSecond uint32, global ...*config.Bandwidth) *Users {
	u := &Users{
		users:                make(map[string]*allowance),
		maxStreams:           maxStreams,
		tasksPerMinute:       tasksPerMinute,
		checkpointsPerSecond: checkpointsPerSecond,
	}
	if len(global) > 0 && global[0] != nil {
		u.SetGlobal(global[0])
	}
	return u
}

// SetGlobal 配置或更新全局共享上下行总带宽令牌桶；传入 nil 表示取消全局总上限。
func (u *Users) SetGlobal(b *config.Bandwidth) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if b == nil {
		u.global = globalLimiters{}
		return
	}
	if !u.global.configured {
		u.global = globalLimiters{
			configured: true,
			policy:     *b,
			upload:     rate.NewLimiter(rate.Limit(b.UploadBytesPerSecond), int(b.BurstBytes)),
			download:   rate.NewLimiter(rate.Limit(b.DownloadBytesPerSecond), int(b.BurstBytes)),
		}
		return
	}
	if u.global.policy != *b {
		syncBandwidthLimiters(u.global.upload, u.global.download, *b)
		u.global.policy = *b
	}
}

func syncBandwidthLimiters(up, down *rate.Limiter, b config.Bandwidth) {
	up.SetLimit(rate.Limit(b.UploadBytesPerSecond))
	down.SetLimit(rate.Limit(b.DownloadBytesPerSecond))
	up.SetBurst(int(b.BurstBytes))
	down.SetBurst(int(b.BurstBytes))
}

func (u *Users) getLocked(user string, b config.Bandwidth) *allowance {
	a := u.users[user]
	if a == nil {
		a = &allowance{
			upload:      rate.NewLimiter(rate.Limit(b.UploadBytesPerSecond), int(b.BurstBytes)),
			download:    rate.NewLimiter(rate.Limit(b.DownloadBytesPerSecond), int(b.BurstBytes)),
			tasks:       rate.NewLimiter(rate.Limit(float64(u.tasksPerMinute)/60), int(u.tasksPerMinute)),
			checkpoints: rate.NewLimiter(rate.Limit(u.checkpointsPerSecond), 1),
			policy:      b,
		}
		u.users[user] = a
		return a
	}
	if a.policy != b {
		syncBandwidthLimiters(a.upload, a.download, b)
		a.policy = b
	}
	return a
}

// Acquire 检查单用户最大并发流数及每分钟新建任务频率，并返回幂等的释放函数。
func (u *Users) Acquire(user string, b config.Bandwidth) (func(), error) {
	return u.acquire(user, b, true)
}

// AcquireList 仅检查并占用单用户最大并发流数，不消耗每分钟新建任务频率配额。
func (u *Users) AcquireList(user string, b config.Bandwidth) (func(), error) {
	return u.acquire(user, b, false)
}

func (u *Users) acquire(user string, b config.Bandwidth, consumeTaskQuota bool) (func(), error) {
	u.mu.Lock()
	a := u.getLocked(user, b)
	if a.streams >= u.maxStreams {
		u.mu.Unlock()
		return nil, &protocol.RemoteError{Code: protocol.CodeQuotaExceeded, Message: "用户活跃流超限"}
	}
	if consumeTaskQuota && !a.tasks.Allow() {
		u.mu.Unlock()
		return nil, &protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "用户任务初始化频率超限"}
	}
	a.streams++
	u.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			u.mu.Lock()
			if a.streams > 0 {
				a.streams--
			}
			u.mu.Unlock()
		})
	}, nil
}

// Wait 为指定方向的数据传输消耗 n 字节令牌（同时受单用户与全局总带宽约束）。
func (u *Users) Wait(ctx context.Context, user string, b config.Bandwidth, direction protocol.Direction, n int) error {
	if n <= 0 {
		return nil
	}
	u.mu.Lock()
	a := u.getLocked(user, b)
	userLimiter := a.upload
	var globalLimiter *rate.Limiter
	if u.global.configured {
		globalLimiter = u.global.upload
	}
	if direction == protocol.DirectionDownload {
		userLimiter = a.download
		if u.global.configured {
			globalLimiter = u.global.download
		}
	}
	u.mu.Unlock()

	if err := waitChunked(ctx, userLimiter, n); err != nil {
		return err
	}
	if globalLimiter != nil {
		return waitChunked(ctx, globalLimiter, n)
	}
	return nil
}

func waitChunked(ctx context.Context, lim *rate.Limiter, n int) error {
	for rem := n; rem > 0; {
		burst := lim.Burst()
		if burst <= 0 {
			return lim.WaitN(ctx, rem)
		}
		step := min(rem, burst)
		if err := lim.WaitN(ctx, step); err != nil {
			if ctx.Err() == nil && lim.Burst() < step {
				continue
			}
			return err
		}
		rem -= step
	}
	return nil
}

// WaitUploadCheckpoint 独立限制每用户持久化断点更新频率，在策略变更与重连间保持连续。
func (u *Users) WaitUploadCheckpoint(ctx context.Context, user string, b config.Bandwidth) error {
	u.mu.Lock()
	limiter := u.getLocked(user, b).checkpoints
	u.mu.Unlock()
	return limiter.WaitN(ctx, 1)
}
