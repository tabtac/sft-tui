package transfer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
)

// QueueRunner 为 queue.Manager 提供可自动重连的共享传输客户端驱动，实现 queue.TaskRunner 契约。
type QueueRunner struct {
	cfg     config.Client
	manager *queue.Manager

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	client   *Client
	dialing  chan struct{}
	dialErr  error
	status   string
	closed   bool
	dialWG   sync.WaitGroup
}

func NewQueueRunner(cfg config.Client, m *queue.Manager) *QueueRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &QueueRunner{
		cfg:     cfg,
		manager: m,
		ctx:     ctx,
		cancel:  cancel,
		status:  "尚未连接",
	}
}

func (r *QueueRunner) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	c := r.client
	r.client = nil
	r.mu.Unlock()

	r.cancel()
	if c != nil {
		_ = c.Close()
	}
	r.dialWG.Wait()
	return nil
}

func (r *QueueRunner) Status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil && !r.client.Connected() && r.dialing == nil {
		return "连接已断开，下次操作将重连"
	}
	return r.status
}

func (r *QueueRunner) ResetDialError() {
	r.mu.Lock()
	r.dialErr = nil
	r.mu.Unlock()
}

func (r *QueueRunner) GetClient(ctx context.Context) (*Client, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.closed || r.ctx.Err() != nil {
			err := r.ctx.Err()
			r.mu.Unlock()
			if err == nil {
				err = context.Canceled
			}
			return nil, err
		}
		if r.client != nil && r.client.Connected() {
			c := r.client
			r.mu.Unlock()
			return c, nil
		}
		if r.dialErr != nil {
			e := r.dialErr
			r.mu.Unlock()
			return nil, e
		}
		wait := r.dialing
		if wait == nil {
			wait = make(chan struct{})
			r.dialing = wait
			r.dialWG.Add(1)
			go r.dial(wait)
		}
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
}

func (r *QueueRunner) dial(wait chan struct{}) {
	defer r.dialWG.Done()
	var c *Client
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		r.mu.Lock()
		if attempt == 0 {
			r.status = "正在连接"
		} else {
			r.status = fmt.Sprintf("重连 %d/3", attempt)
		}
		r.mu.Unlock()

		if attempt > 0 {
			timer := time.NewTimer(time.Second * time.Duration(1<<(attempt-1)))
			select {
			case <-r.ctx.Done():
				timer.Stop()
				err = r.ctx.Err()
			case <-timer.C:
			}
			if r.ctx.Err() != nil {
				break
			}
		}

		c, err = Dial(r.ctx, r.cfg)
		if err == nil && r.manager != nil && c.Identity() != r.manager.Identity() {
			_ = c.Close()
			c = nil
			err = fmt.Errorf("身份证书与当前队列不匹配")
		}
		if err == nil || !IsTransient(err) {
			break
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if (r.closed || r.ctx.Err() != nil) && c != nil {
		_ = c.Close()
		c = nil
		err = r.ctx.Err()
		if err == nil {
			err = context.Canceled
		}
	}
	r.client = c
	r.dialErr = err
	r.dialing = nil
	if err != nil {
		r.status = "连接失败：" + err.Error()
	} else {
		r.status = "已连接"
	}
	close(wait)
}

func (r *QueueRunner) List(ctx context.Context) ([]protocol.FileEntry, error) {
	r.ResetDialError()
	c, err := r.GetClient(ctx)
	if err != nil {
		return nil, err
	}
	var entries []protocol.FileEntry
	err = c.ListFiles(ctx, func(f protocol.FileEntry) error {
		if len(entries) >= 100000 {
			return fmt.Errorf("远端文件数量超过显示上限")
		}
		entries = append(entries, f)
		return nil
	})
	return entries, err
}

func preDialControlErr(err error, task queue.Task, op *queue.Operation) error {
	if op != nil && op.Mode() != 0 && errors.Is(err, context.Canceled) {
		if op.Mode() == protocol.CancelPause {
			return ErrPaused
		}
		if op.Mode() == protocol.CancelAbort && !task.HasCheckpoint() {
			return ErrCancelled
		}
	}
	return err
}

// Run 满足 queue.TaskRunner 签名，支持瞬态断连重连、断点提示以及统一 SQLite 持久化。
func (r *QueueRunner) Run(ctx context.Context, task queue.Task, op *queue.Operation, progress func(transferred, total uint64, phase queue.State)) error {
	r.ResetDialError()
	update := func(fn func(*queue.Task)) error {
		if r.manager == nil {
			fn(&task)
			return nil
		}
		return r.manager.UpdateTask(task.ID, fn)
	}

	const maxRateWait = 65 * time.Second
	var rateStart time.Time
	rateAttempt := 0

	for attempt := 0; attempt < 4; {
		if op != nil && op.Mode() != 0 {
			if mapped := preDialControlErr(context.Canceled, task, op); mapped != context.Canceled {
				return mapped
			}
		}
		if err := ctx.Err(); err != nil {
			return preDialControlErr(err, task, op)
		}
		c, err := r.GetClient(ctx)
		if err != nil {
			return preDialControlErr(err, task, op)
		}

		// 从 manager 重新读取最新检查点字段以防止重试覆盖
		if r.manager != nil {
			if latest := r.manager.Task(task.ID); latest != nil {
				task = *latest
			}
		}

		_, err = c.Execute(ctx, task, op, progress, update)
		if err == nil {
			return nil
		}
		if (op != nil && op.Mode() != 0) || r.ctx.Err() != nil {
			return err
		}

		// 服务端临时限流：保持健康连接，按有界退避可中断等待后重试
		if IsRateLimited(err) {
			if rateStart.IsZero() {
				rateStart = time.Now()
			}
			if time.Since(rateStart) >= maxRateWait {
				return err
			}
			delay := 250 * time.Millisecond * time.Duration(1<<min(rateAttempt, 3))
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
			rateAttempt++
			if r.manager != nil {
				_ = r.manager.UpdateTask(task.ID, func(cur *queue.Task) {
					cur.State = queue.Waiting
					cur.Error = "服务端限流，等待配额后自动重试"
				})
			}
			var opDone <-chan struct{}
			if op != nil {
				opDone = op.Context().Done()
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
				continue
			case <-ctx.Done():
				timer.Stop()
				return preDialControlErr(ctx.Err(), task, op)
			case <-r.ctx.Done():
				timer.Stop()
				return preDialControlErr(r.ctx.Err(), task, op)
			case <-opDone:
				timer.Stop()
				return preDialControlErr(context.Canceled, task, op)
			}
		}

		if (!IsTransient(err) && !isReconnectableRemote(err)) || attempt == 3 {
			var remote *protocol.RemoteError
			if task.HasCheckpoint() && errors.As(err, &remote) &&
				(remote.Code == protocol.CodeTransferExpired || remote.Code == protocol.CodeNotFound) {
				return fmt.Errorf("服务端断点已过期或任务不存在，请取消后重新传输：%w", err)
			}
			return err
		}
		attempt++

		r.mu.Lock()
		if r.client == c {
			r.client = nil
		}
		r.mu.Unlock()
		// 若连接仅处于 GOAWAY 排空状态，保留旧连接让其上已有活跃流自然完成，仅由后续重试建立新连接。
		if !c.Draining() {
			_ = c.Close()
		}

		if r.manager != nil {
			_ = r.manager.UpdateTask(task.ID, func(cur *queue.Task) {
				cur.State = queue.Reconnecting
				cur.Error = "连接中断，正在恢复断点"
			})
		}
	}
	return nil
}
