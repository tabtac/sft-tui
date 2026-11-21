package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"sft-tui/internal/pal"
	"sft-tui/internal/protocol"
)

func sameCleanDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil {
		absA = filepath.Clean(a)
	}
	if errB != nil {
		absB = filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(absA, absB)
	}
	return absA == absB
}

// MigrateUnfinished 将旧状态目录或旧身份键下的未完成队列记录安全迁移到目标队列。
// 若源队列文件不存在则返回 (0, nil)；若源或目标队列被另一实例锁定则返回错误。
func MigrateUnfinished(srcDir, srcIdentity, dstDir, dstIdentity string) (int, error) {
	if srcDir == "" || srcIdentity == "" || dstDir == "" || dstIdentity == "" {
		return 0, nil
	}
	if sameCleanDir(srcDir, dstDir) && srcIdentity == dstIdentity {
		return 0, nil
	}
	srcDB := filepath.Join(srcDir, "queue-"+srcIdentity+".sqlite3")
	if _, err := os.Lstat(srcDB); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	dst, err := OpenStore(dstDir, dstIdentity)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	return dst.MigrateUnfinishedFrom(srcDir, srcIdentity, dstIdentity)
}

// MigrateUnfinishedFrom 在已持有目标 Store 独占锁的情况下，从源状态目录/身份键迁移未完成任务与历史记录。
func (s *Store) MigrateUnfinishedFrom(srcDir, srcIdentity, dstIdentity string) (int, error) {
	if srcDir == "" || srcIdentity == "" || dstIdentity == "" {
		return 0, nil
	}
	srcDB := filepath.Join(srcDir, "queue-"+srcIdentity+".sqlite3")
	if _, err := os.Lstat(srcDB); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}

	src, err := OpenStore(srcDir, srcIdentity)
	if err != nil {
		return 0, fmt.Errorf("迁移旧队列失败（源队列不可打开或正被占用）：%w", err)
	}
	defer src.Close()

	srcTasks, err := src.Load(false)
	if err != nil {
		return 0, fmt.Errorf("读取源队列任务失败：%w", err)
	}
	srcHistory, err := src.Load(true)
	if err != nil {
		return 0, fmt.Errorf("读取源队列历史失败：%w", err)
	}
	if len(srcTasks) == 0 && len(srcHistory) == 0 {
		return 0, nil
	}

	dstTasks, err := s.Load(false)
	if err != nil {
		return 0, fmt.Errorf("读取目标队列任务失败：%w", err)
	}
	dstHistory, err := s.Load(true)
	if err != nil {
		return 0, fmt.Errorf("读取目标队列历史失败：%w", err)
	}

	dstByID := make(map[string]bool, len(dstTasks))
	dstUnfinishedTargets := make(map[string]bool, len(dstTasks))
	for _, t := range dstTasks {
		dstByID[t.ID] = true
		if !t.Terminal() {
			dstUnfinishedTargets[TargetKey(t.Direction, t.Remote, t.Path)] = true
		}
	}

	var toWrite []Task
	var migratedIDs []string
	for _, t := range srcTasks {
		if t.Identity != srcIdentity || protocol.ValidateName(t.Remote) != nil || !filepath.IsAbs(t.Path) {
			return 0, fmt.Errorf("源队列任务记录无效或身份不匹配")
		}
		if t.Terminal() {
			continue
		}
		if dstByID[t.ID] {
			migratedIDs = append(migratedIDs, t.ID)
			continue
		}
		tk := TargetKey(t.Direction, t.Remote, t.Path)
		if dstUnfinishedTargets[tk] {
			// 目标队列已有同目标未完成任务，保留目标任务且不覆盖，也暂不删除源记录
			continue
		}
		if t.State.Active() {
			if t.WantCancel {
				t.State = Pending
				t.Error = "上次取消尚未完成，请重试取消"
			} else {
				t.State = Paused
				t.Error = "已从原状态目录迁移，请选择继续"
			}
		}
		if srcIdentity != dstIdentity {
			newKey, err := StateKeyForIdentity(dstIdentity, t.Direction, t.Remote, t.Path)
			if err != nil {
				return 0, err
			}
			if t.Direction == protocol.DirectionDownload && t.TempName != "" {
				newTemp := ".sft-" + newKey + ".part"
				if t.TempName != newTemp {
					migrateDownloadTempFile(filepath.Dir(t.Path), t.TempName, newTemp)
					t.TempName = newTemp
				}
			}
			t.Identity = dstIdentity
			t.StateKey = newKey
		}
		toWrite = append(toWrite, t)
		migratedIDs = append(migratedIDs, t.ID)
		dstByID[t.ID] = true
		dstUnfinishedTargets[tk] = true
	}

	if len(toWrite) > 0 {
		if err := s.Write(toWrite, false); err != nil {
			return 0, fmt.Errorf("写入迁移任务失败：%w", err)
		}
	}

	if len(dstHistory) == 0 && len(srcHistory) > 0 {
		var hist []Task
		for _, h := range srcHistory {
			if h.Identity != srcIdentity || protocol.ValidateName(h.Remote) != nil || !filepath.IsAbs(h.Path) {
				continue
			}
			if srcIdentity != dstIdentity {
				if newKey, err := StateKeyForIdentity(dstIdentity, h.Direction, h.Remote, h.Path); err == nil {
					h.Identity = dstIdentity
					h.StateKey = newKey
				}
			}
			hist = append(hist, h)
		}
		if len(hist) > 0 {
			_ = s.Write(hist, true)
		}
	}

	if len(migratedIDs) > 0 {
		tx, err := src.db.Begin()
		if err == nil {
			ok := true
			for _, id := range migratedIDs {
				if _, err := tx.Exec("DELETE FROM tasks WHERE id=?", id); err != nil {
					ok = false
					break
				}
			}
			if ok {
				_ = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
	}

	return len(toWrite), nil
}

func migrateDownloadTempFile(parent, oldName, newName string) {
	oldPath := filepath.Join(parent, oldName)
	newPath := filepath.Join(parent, newName)
	if err := pal.CheckRegularOrAbsent(oldPath); err != nil {
		return
	}
	if _, err := os.Lstat(oldPath); err != nil {
		return
	}
	if _, err := os.Lstat(newPath); err == nil {
		return
	}
	_ = os.Rename(oldPath, newPath)
}
