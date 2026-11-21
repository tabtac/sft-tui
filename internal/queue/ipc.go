package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"sft-tui/internal/pal"
)

const maxQueueIPCBytes = 64 * 1024

type ipcRequest struct {
	Op     string `json:"op"`
	Prefix string `json:"prefix,omitempty"`
}

type ipcResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Tasks []Task `json:"tasks,omitempty"`
	Task  *Task  `json:"task,omitempty"`
}

// IPCPath 返回该状态目录与身份对应的本地队列控制 IPC 端点路径。
func IPCPath(stateDir, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return pal.GetNamedIPCPath(stateDir, "q-"+hex.EncodeToString(sum[:8]))
}

func (m *Manager) startIPC() {
	ep := IPCPath(m.cfg.StateDir, m.identity)
	ln, err := pal.ListenIPC(ep)
	if err != nil {
		return
	}
	m.ipcLn = ln
	m.ipcEndpoint = ep
	m.ipcWG.Add(1)
	go m.serveIPC(ln)
}

func (m *Manager) stopIPC() {
	if m.ipcLn != nil {
		_ = m.ipcLn.Close()
		m.ipcLn = nil
	}
	m.ipcWG.Wait()
	if m.ipcEndpoint != "" && runtime.GOOS != "windows" {
		_ = os.Remove(m.ipcEndpoint)
	}
	m.ipcEndpoint = ""
}

func (m *Manager) serveIPC(ln net.Listener) {
	defer m.ipcWG.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		m.ipcWG.Add(1)
		go func(c net.Conn) {
			defer m.ipcWG.Done()
			defer c.Close()
			m.handleIPCConn(c)
		}(conn)
	}
}

func (m *Manager) handleIPCConn(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(18 * time.Second))
	var req ipcRequest
	dec := json.NewDecoder(io.LimitReader(c, maxQueueIPCBytes))
	if err := dec.Decode(&req); err != nil {
		_ = json.NewEncoder(c).Encode(ipcResponse{Error: "无法解析控制请求：" + err.Error()})
		return
	}
	resp := m.dispatchIPC(req)
	_ = json.NewEncoder(c).Encode(resp)
}

func (m *Manager) findUnfinishedByPrefix(prefix string) (Task, error) {
	var matched []Task
	for _, t := range m.Snapshot(false) {
		if !t.Terminal() && strings.HasPrefix(t.ID, prefix) {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		return Task{}, fmt.Errorf("未找到匹配的未完成任务：%s", prefix)
	}
	if len(matched) > 1 {
		return Task{}, fmt.Errorf("任务前缀不唯一（匹配 %d 项）：%s", len(matched), prefix)
	}
	return matched[0], nil
}

func (m *Manager) dispatchIPC(req ipcRequest) ipcResponse {
	switch req.Op {
	case "status":
		var unfinished []Task
		for _, t := range m.Snapshot(false) {
			if !t.Terminal() {
				unfinished = append(unfinished, t)
			}
		}
		return ipcResponse{OK: true, Tasks: unfinished}

	case "pause":
		t, err := m.findUnfinishedByPrefix(req.Prefix)
		if err != nil {
			return ipcResponse{Error: err.Error()}
		}
		if err := m.Pause(t.ID); err != nil {
			return ipcResponse{Error: err.Error()}
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			cur := m.Task(t.ID)
			if cur == nil || cur.State != Pausing {
				if cur != nil {
					t = *cur
				}
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		return ipcResponse{OK: true, Task: &t}

	case "abort":
		t, err := m.findUnfinishedByPrefix(req.Prefix)
		if err != nil {
			return ipcResponse{Error: err.Error()}
		}
		if err := m.Cancel(t.ID); err != nil {
			return ipcResponse{Error: err.Error()}
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			cur := m.Task(t.ID)
			if cur == nil || cur.State == Cancelled {
				return ipcResponse{OK: true, Task: &t}
			}
			if cur.State == Pending && cur.Error != "" {
				return ipcResponse{Error: "取消失败：" + cur.Error}
			}
			time.Sleep(25 * time.Millisecond)
		}
		return ipcResponse{Error: "取消等待超时"}

	default:
		return ipcResponse{Error: "不支持的控制操作：" + req.Op}
	}
}

func callRemoteIPC(stateDir, identity string, req ipcRequest, timeout time.Duration) (ipcResponse, error) {
	ep := IPCPath(stateDir, identity)
	conn, err := pal.DialIPC(ep, 2*time.Second)
	if err != nil {
		return ipcResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return ipcResponse{}, err
	}
	var resp ipcResponse
	if err := json.NewDecoder(io.LimitReader(conn, 4*1024*1024)).Decode(&resp); err != nil {
		return ipcResponse{}, err
	}
	return resp, nil
}

// RemoteStatus 通过本地 IPC 向正在运行的客户端实例查询未完成队列状态。
func RemoteStatus(stateDir, identity string) ([]Task, error) {
	resp, err := callRemoteIPC(stateDir, identity, ipcRequest{Op: "status"}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Tasks, nil
}

// RemotePause 通过本地 IPC 向正在运行的客户端实例发送暂停指令。
func RemotePause(stateDir, identity, prefix string) (Task, error) {
	resp, err := callRemoteIPC(stateDir, identity, ipcRequest{Op: "pause", Prefix: prefix}, 8*time.Second)
	if err != nil {
		return Task{}, err
	}
	if !resp.OK || resp.Task == nil {
		return Task{}, fmt.Errorf("%s", resp.Error)
	}
	return *resp.Task, nil
}

// RemoteAbort 通过本地 IPC 向正在运行的客户端实例发送取消并清理指令。
func RemoteAbort(stateDir, identity, prefix string) (Task, error) {
	resp, err := callRemoteIPC(stateDir, identity, ipcRequest{Op: "abort", Prefix: prefix}, 18*time.Second)
	if err != nil {
		return Task{}, err
	}
	if !resp.OK || resp.Task == nil {
		return Task{}, fmt.Errorf("%s", resp.Error)
	}
	return *resp.Task, nil
}
