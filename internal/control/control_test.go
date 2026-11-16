package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sft-tui/internal/monitor"
	"sft-tui/internal/pal"
	"sync/atomic"
	"testing"
	"time"
)

func TestReloadEndpointIdentityPermissionsAndFailure(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "server.json")

	var calls atomic.Int32
	var reject atomic.Bool
	svc, err := Start(cfgPath, root, func() error {
		calls.Add(1)
		if reject.Load() {
			return errors.New("invalid replacement")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Close()

	if runtime.GOOS == "linux" {
		info, err := os.Stat(filepath.Join(root, ".sftd.sock"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("control socket permissions: %v", err)
		}
	}

	if ok, err := Reload(filepath.Join(root, "other.json"), root); err == nil || ok {
		t.Fatal("wrong config reached reload callback")
	}
	if calls.Load() != 0 {
		t.Fatal("wrong config mutated runtime")
	}
	if ok, err := Reload(cfgPath, root); err != nil || !ok {
		t.Fatalf("reload: %v", err)
	}
	reject.Store(true)
	if ok, err := Reload(cfgPath, root); err == nil || ok {
		t.Fatal("failed reload reported success")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
	reject.Store(false)
	if ok, err := Reload(cfgPath, filepath.Join(root, "offline")); err != nil || !ok {
		t.Fatalf("registered endpoint must override fallback: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if ok, err := Reload(cfgPath, filepath.Join(root, "offline")); err != nil || ok {
		t.Fatal("expected absent offline service to return (false, nil)")
	}
}

func sendRawControlRequest(t *testing.T, root string, payload []byte) response {
	t.Helper()
	conn, err := pal.DialIPC(pal.GetIPCPath(root), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply response
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestControlRejectsMalformedRequestsBeforeSideEffects(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "server.json")
	var calls atomic.Int32
	svc, err := Start(cfg, root, func() error { calls.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	valid, err := json.Marshal(request{Config: cfg, Action: "reload"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"invalid_json", []byte("{broken")},
		{"null", []byte("null")},
		{"unknown_field", []byte(fmt.Sprintf(`{"config":%q,"action":"reload","unknown":true}`, cfg))},
		{"unknown_action", []byte(fmt.Sprintf(`{"config":%q,"action":"delete"}`, cfg))},
		{"wrong_version", []byte(fmt.Sprintf(`{"config":%q,"action":"reload","version":999}`, cfg))},
		{"trailing_object", append(append([]byte(nil), valid...), []byte(`{"action":"stop"}`)...)},
		{"trailing_garbage", append(append([]byte(nil), valid...), []byte("garbage")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			reply := sendRawControlRequest(t, root, tc.payload)
			// BUG: 当前只 Decode 一次，合法对象后附加的 JSON/垃圾内容仍能触发 reload。
			if reply.OK || calls.Load() != before {
				t.Errorf("BUG: malformed request executed callback: ok=%v calls=%d->%d", reply.OK, before, calls.Load())
			}
		})
	}
}

func TestControlManagedStatusLogsAndStopActions(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "server.json")
	stops := make(chan bool, 2)
	svc, err := StartManaged(cfg, root, Handlers{
		Snapshot: func() monitor.Snapshot { return monitor.Snapshot{Version: monitor.Version, State: "running"} },
		Logs: func(q monitor.LogQuery) monitor.LogPage {
			return monitor.LogPage{Last: q.After + 1, Events: []monitor.Event{{Sequence: q.After + 1, Message: q.Text}}}
		},
		Stop: func(immediate bool) { stops <- immediate },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	snapshot, err := Status(cfg, root)
	if err != nil || snapshot == nil || snapshot.State != "running" {
		t.Fatalf("status=%+v err=%v", snapshot, err)
	}
	logs, err := QueryLogs(cfg, root, monitor.LogQuery{After: 7, Text: "needle"})
	if err != nil || len(logs.Events) != 1 || logs.Events[0].Message != "needle" || logs.Last != 8 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	for _, immediate := range []bool{false, true} {
		if ok, err := Stop(cfg, root, immediate); err != nil || !ok {
			t.Fatalf("stop=%v err=%v", ok, err)
		}
		select {
		case got := <-stops:
			if got != immediate {
				t.Errorf("stop routing=%v want=%v", got, immediate)
			}
		case <-time.After(time.Second):
			t.Fatal("stop callback not invoked")
		}
	}
}
