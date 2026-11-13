package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewSQLiteDurabilitySettingsSurviveReconnect(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	for _, reconnect := range []bool{false, true} {
		if reconnect {
			// 强制关闭空闲连接，验证 DSN 对新连接也设置了同步参数。
			s.db.SetMaxIdleConns(0)
		}
		for _, pragma := range []struct {
			name string
			want int
		}{
			{"synchronous", 2}, {"foreign_keys", 1}, {"busy_timeout", 5000},
		} {
			var got int
			if err := s.db.QueryRow("PRAGMA " + pragma.name).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != pragma.want && !(pragma.name == "synchronous" && got == 3) {
				t.Errorf("reconnect=%v %s=%d want=%d", reconnect, pragma.name, got, pragma.want)
			}
		}
		var mode string
		if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(mode, "wal") {
			t.Errorf("journal_mode=%q", mode)
		}
	}
}

func TestReviewTaskSchemaRejectsImpossibleStates(t *testing.T) {
	// BUG: 当前 schema 缺少 CHECK，可能把越界偏移、非法方向/状态及坏 ID/摘要持久化。
	for _, tc := range []struct {
		name, assignment string
		value            any
	}{
		{"negative_size", "file_size=?", -1},
		{"negative_offset", "offset=?", -1},
		{"past_size", "offset=?", 3},
		{"direction_zero", "direction=?", 0},
		{"direction_unknown", "direction=?", 99},
		{"status_unknown", "status=?", "UNKNOWN"},
		{"short_digest", "file_sha256=?", "00"},
		{"nonhex_digest", "file_sha256=?", strings.Repeat("z", 64)},
		{"short_request_id", "request_id=?", "00"},
		{"invalid_object_id", "object_id=?", "../other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("AB"), 1))
			if _, err := s.db.Exec("UPDATE tasks SET "+tc.assignment+" WHERE id=?", tc.value, IDToHex(task.ID)); err == nil {
				t.Errorf("BUG: database accepted %s", tc.name)
			}
		})
	}
}

func TestReviewTaskDecoderRejectsCorruptRecords(t *testing.T) {
	for _, tc := range []struct {
		name, column string
		value        any
	}{
		{"request_id", "request_id", "nothex"},
		{"digest_short", "file_sha256", "00"},
		{"digest_nonhex", "file_sha256", strings.Repeat("z", 64)},
		{"status", "status", "UNKNOWN"},
		{"direction", "direction", 99},
		{"object_id", "object_id", "../outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("AB"), 1))
			// 绕过 CHECK 构造历史/损坏记录，测试读取边界，兼容将来加强 schema 后继续运行。
			reviewExec(t, s, "PRAGMA ignore_check_constraints=ON")
			reviewExec(t, s, "UPDATE tasks SET "+tc.column+"=? WHERE id=?", tc.value, IDToHex(task.ID))
			if _, err := s.GetTask(context.Background(), task.ID); err == nil {
				t.Error("BUG: corrupt metadata silently decoded into a usable task")
			}
		})
	}
}

func TestReviewPublishedDigestCorruptionIsReported(t *testing.T) {
	for _, corrupt := range []string{"00", strings.Repeat("z", 64)} {
		t.Run(corrupt[:2], func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			reviewPublish(t, s, "alice", "a", []byte("A"), 1)
			reviewExec(t, s, "PRAGMA ignore_check_constraints=ON")
			reviewExec(t, s, "UPDATE published_files SET file_sha256=?", corrupt)
			if _, _, err := s.ListPublishedFiles(context.Background(), "alice", "", 12); err == nil {
				t.Error("BUG: LIST silently pads/zeros corrupt SHA-256")
			}
			f, _, err := s.OpenPublishedFile(context.Background(), "alice", "a")
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Error("BUG: download silently pads/zeros corrupt SHA-256")
			}
		})
	}
}

func TestReviewDatabaseRequestKeyIsUnique(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	reviewCreateTask(t, s, "alice", reviewRequest("a", nil, 1))
	// BUG: idx_tasks_user_req 是普通索引，未约束幂等键唯一性。
	_, err := s.db.Exec(`INSERT INTO tasks (id,user_id,request_id,direction,file_name,file_size,file_sha256,object_id,offset,status,created_at,updated_at,expires_at) SELECT ?,user_id,request_id,direction,file_name,file_size,file_sha256,?,offset,status,created_at,updated_at,expires_at FROM tasks`, strings.Repeat("a", 32), strings.Repeat("b", 32))
	if err == nil {
		t.Error("BUG: duplicate (user_id, request_id) persisted")
	} else if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Errorf("duplicate fixture failed for a reason other than key uniqueness: %v", err)
	}
}

func TestReviewStorePathsWithSpacesUnicodeAndURICharacters(t *testing.T) {
	for _, name := range []string{"with spaces", "中文存储", "percent%20literal", "hash#literal"} {
		t.Run(name, func(t *testing.T) {
			s := reviewOpenStore(t, filepath.Join(t.TempDir(), name))
			reviewPublish(t, s, "alice", "a", []byte("A"), 1)
		})
	}
}

func TestReviewIDEncodingBoundaries(t *testing.T) {
	for _, value := range [][16]byte{{}, {0xff, 0, 1}, {15: 0xff}} {
		encoded := IDToHex(value)
		decoded, err := HexToID(encoded)
		if err != nil || decoded != value || len(encoded) != 32 {
			t.Errorf("roundtrip: %x %v", value, err)
		}
		upper, err := HexToID(strings.ToUpper(encoded))
		if err != nil || upper != value {
			t.Errorf("upper hex: %v", err)
		}
	}
	for _, value := range []string{"", "0", "00", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("a", 34), strings.Repeat("g", 32), " " + strings.Repeat("0", 32)} {
		t.Run(fmt.Sprintf("invalid_%q", value), func(t *testing.T) {
			if _, err := HexToID(value); err == nil {
				t.Errorf("invalid ID accepted: %q", value)
			}
		})
	}
}
