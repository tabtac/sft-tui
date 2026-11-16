// Package control 复用 PAL 跨平台私有 IPC（Unix Socket / Windows 命名管道），
// 为 sftd 提供本地控制面（热重载、探测、状态快照、日志检索、停服排水）。
package control

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"sft-tui/internal/monitor"
	"sft-tui/internal/pal"
)

const maxResponseBytes = 2 * 1024 * 1024

type request struct {
	Config  string           `json:"config"`
	Action  string           `json:"action,omitempty"`
	Version int              `json:"version,omitempty"`
	Query   monitor.LogQuery `json:"query,omitempty"`
}

type response struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Snapshot *monitor.Snapshot `json:"snapshot,omitempty"`
	Logs     *monitor.LogPage  `json:"logs,omitempty"`
}

type location struct {
	Config  string `json:"config"`
	Storage string `json:"storage"`
}

type Service struct {
	listener net.Listener
	wg       sync.WaitGroup
}

type Handlers struct {
	Reload   func() error
	Snapshot func() monitor.Snapshot
	Logs     func(monitor.LogQuery) monitor.LogPage
	Stop     func(immediate bool)
}

func sameConfigPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func locationPath(configPath string) (hintPath, absConfig string, err error) {
	absConfig, err = filepath.Abs(configPath)
	if err != nil {
		return "", "", err
	}
	key := absConfig
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	digest := sha256.Sum256([]byte(key))
	hintPath = filepath.Join(filepath.Dir(absConfig), fmt.Sprintf(".sftd-runtime-%x.json", digest[:12]))
	return hintPath, absConfig, nil
}

func remember(absConfig, storageRoot string) error {
	hintPath, _, err := locationPath(absConfig)
	if err != nil {
		return err
	}
	absRoot, err := filepath.Abs(storageRoot)
	if err != nil {
		return err
	}
	if err := pal.EnsurePrivateDir(filepath.Dir(hintPath)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(location{Config: absConfig, Storage: absRoot}, "", "  ")
	if err != nil {
		return err
	}
	return pal.StageAndReplace(hintPath, append(data, '\n'))
}

// Start 仅注册重载回调，便于测试或轻量使用。
func Start(configPath, storageRoot string, reload func() error) (*Service, error) {
	return StartManaged(configPath, storageRoot, Handlers{Reload: reload})
}

// StartManaged 监听与 storageRoot 绑定的私有本地 IPC 端点并记录配置文件位置提示。
func StartManaged(configPath, storageRoot string, handlers Handlers) (*Service, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	if err := pal.EnsurePrivateDir(storageRoot); err != nil {
		return nil, err
	}
	listener, err := pal.ListenIPC(pal.GetIPCPath(storageRoot))
	if err != nil {
		return nil, err
	}
	if err := remember(absConfig, storageRoot); err != nil {
		_ = listener.Close()
		return nil, err
	}
	s := &Service{listener: listener}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		slots := make(chan struct{}, 4)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = conn.Close()
				continue
			}
			s.wg.Add(1)
			go func(c net.Conn) {
				defer s.wg.Done()
				defer func() {
					<-slots
					_ = c.Close()
				}()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				var req request
				line, err := bufio.NewReader(io.LimitReader(c, 4097)).ReadBytes('\n')
				if err == io.EOF && len(line) > 0 {
					err = nil
				}
				if err == nil && len(line) > 4096 {
					err = fmt.Errorf("控制请求过大")
				}
				if err == nil {
					trimmed := bytes.TrimSpace(line)
					if !bytes.HasPrefix(trimmed, []byte("{")) {
						err = fmt.Errorf("控制请求必须为 JSON 对象")
					} else {
						dec := json.NewDecoder(bytes.NewReader(trimmed))
						dec.DisallowUnknownFields()
						if err = dec.Decode(&req); err == nil {
							var extra any
							if extraErr := dec.Decode(&extra); extraErr != io.EOF {
								err = fmt.Errorf("控制请求只能包含单个 JSON 对象")
							}
						}
					}
				}
				if err == nil && !sameConfigPath(req.Config, absConfig) {
					err = fmt.Errorf("运行中的服务端使用另一配置文件：%s", absConfig)
				}
				r := response{}
				if err == nil && req.Version != 0 && req.Version != monitor.Version {
					err = fmt.Errorf("不支持的管理接口版本：%d", req.Version)
				}
				if err == nil {
					switch req.Action {
					case "", "reload":
						if handlers.Reload == nil {
							err = fmt.Errorf("重载不可用")
						} else {
							err = handlers.Reload()
						}
					case "probe":
					case "status":
						if handlers.Snapshot == nil {
							err = fmt.Errorf("状态查询不可用")
						} else {
							snap := handlers.Snapshot()
							r.Snapshot = &snap
						}
					case "logs":
						if handlers.Logs == nil {
							err = fmt.Errorf("日志查询不可用")
						} else {
							logs := handlers.Logs(req.Query)
							r.Logs = &logs
						}
					case "drain", "stop":
						if handlers.Stop == nil {
							err = fmt.Errorf("停服控制不可用")
						} else {
							handlers.Stop(req.Action == "stop")
						}
					default:
						err = fmt.Errorf("不支持的管理操作")
					}
				}
				r.OK = err == nil
				if err != nil {
					r.Error = err.Error()
				}
				data, encErr := json.Marshal(r)
				for encErr == nil && len(data) > maxResponseBytes && r.Logs != nil && len(r.Logs.Events) > 1 {
					r.Logs.Events = r.Logs.Events[len(r.Logs.Events)/2:]
					r.Logs.Lost = true
					data, encErr = json.Marshal(r)
				}
				if encErr != nil || len(data) > maxResponseBytes {
					data, _ = json.Marshal(response{Error: "管理响应超过大小限制"})
				}
				data = append(data, '\n')
				_, _ = c.Write(data)
			}(conn)
		}
	}()
	return s, nil
}

func (s *Service) Close() error {
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

// Locate 检查提示文件，仅当探测确认端点由同一配置文件托管时才采用提示中的 storageRoot。
func Locate(configPath, fallback string) (string, error) {
	hintPath, absConfig, err := locationPath(configPath)
	if err != nil {
		return "", err
	}
	data, err := pal.ReadSmall(hintPath, 4096)
	if os.IsNotExist(err) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	var hint location
	if err = json.Unmarshal(data, &hint); err != nil {
		return "", err
	}
	if !sameConfigPath(hint.Config, absConfig) || !filepath.IsAbs(hint.Storage) {
		return "", fmt.Errorf("运行端点记录与配置不匹配")
	}
	_, running, err := call(configPath, hint.Storage, request{Action: "probe"}, 500*time.Millisecond)
	if err != nil {
		return "", err
	}
	if running {
		return hint.Storage, nil
	}
	return fallback, nil
}

func Reload(configPath, storageRoot string) (bool, error) {
	root, err := Locate(configPath, storageRoot)
	if err != nil {
		return false, err
	}
	return exchange(configPath, root, "", 5*time.Second)
}

func Probe(configPath, storageRoot string) (bool, error) {
	return exchange(configPath, storageRoot, "probe", 500*time.Millisecond)
}

func Status(configPath, storageRoot string) (*monitor.Snapshot, error) {
	root, err := Locate(configPath, storageRoot)
	if err != nil {
		return nil, err
	}
	reply, running, err := call(configPath, root, request{Action: "status", Version: monitor.Version}, 2*time.Second)
	if err != nil || !running {
		return nil, err
	}
	if reply.Snapshot == nil || reply.Snapshot.Version != monitor.Version {
		return nil, fmt.Errorf("无效的管理状态响应")
	}
	return reply.Snapshot, nil
}

func QueryLogs(configPath, storageRoot string, q monitor.LogQuery) (monitor.LogPage, error) {
	root, err := Locate(configPath, storageRoot)
	if err != nil {
		return monitor.LogPage{}, err
	}
	reply, running, err := call(configPath, root, request{Action: "logs", Version: monitor.Version, Query: q}, 2*time.Second)
	if err != nil {
		return monitor.LogPage{}, err
	}
	if !running || reply.Logs == nil {
		return monitor.LogPage{}, fmt.Errorf("日志管理端点不可用")
	}
	return *reply.Logs, nil
}

func Stop(configPath, storageRoot string, immediate bool) (bool, error) {
	root, err := Locate(configPath, storageRoot)
	if err != nil {
		return false, err
	}
	action := "drain"
	if immediate {
		action = "stop"
	}
	return exchange(configPath, root, action, 2*time.Second)
}

func exchange(configPath, storageRoot, action string, timeout time.Duration) (bool, error) {
	_, running, err := call(configPath, storageRoot, request{Action: action}, timeout)
	return running, err
}

func call(configPath, storageRoot string, req request, timeout time.Duration) (response, bool, error) {
	conn, err := pal.DialIPC(pal.GetIPCPath(storageRoot), timeout)
	if err != nil {
		if pal.IsIPCNotFound(err) {
			return response{}, false, nil
		}
		return response{}, false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return response{}, false, err
	}
	req.Config = absConfig
	payload, err := json.Marshal(req)
	if err != nil {
		return response{}, false, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return response{}, false, err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxResponseBytes+1)).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return response{}, false, err
	}
	if len(line) > maxResponseBytes {
		return response{}, false, fmt.Errorf("管理响应超过大小限制")
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return response{}, false, err
	}
	if !resp.OK {
		return response{}, false, fmt.Errorf("%s", resp.Error)
	}
	return resp, true, nil
}
