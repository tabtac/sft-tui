package certmanage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
)

type fileChange struct {
	path      string
	data      []byte
	mode      os.FileMode
	exclusive bool
}

type stagedSnapshot struct {
	fileChange
	old     []byte
	oldMode os.FileMode
	exists  bool
	temp    string
}

// applyChanges 先将所有变更落盘到临时文件并做冲突检查，再按序发布并在失败时回滚。
func applyChanges(changes []fileChange) (result error) {
	var prepared []stagedSnapshot
	defer func() {
		for _, s := range prepared {
			if s.temp != "" {
				_ = os.Remove(s.temp)
			}
		}
	}()
	seen := make(map[string]bool)
	for _, c := range changes {
		abs, err := filepath.Abs(c.path)
		if err != nil {
			return err
		}
		if runtime.GOOS == "windows" {
			abs = strings.ToLower(abs)
		}
		if seen[abs] {
			return fmt.Errorf("输出路径重复：%s", c.path)
		}
		seen[abs] = true

		if err := pal.EnsurePrivateDir(filepath.Dir(c.path)); err != nil {
			return err
		}
		s := stagedSnapshot{fileChange: c}
		info, err := os.Lstat(c.path)
		if err == nil {
			if c.exclusive {
				return fmt.Errorf("目标已存在：%s", c.path)
			}
			if !info.Mode().IsRegular() || info.Size() > config.MaxConfigBytes {
				return fmt.Errorf("目标必须是最多 64 KiB 的普通文件：%s", c.path)
			}
			s.exists, s.oldMode = true, info.Mode().Perm()
			s.old, err = os.ReadFile(c.path)
			if err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		s.temp, err = pal.StageTempFile(c.path, c.data, c.mode)
		if err != nil {
			return err
		}
		prepared = append(prepared, s)
	}

	committed := 0
	defer func() {
		if result == nil {
			return
		}
		for i := committed - 1; i >= 0; i-- {
			s := prepared[i]
			current, err := os.ReadFile(s.path)
			if err != nil || !bytes.Equal(current, s.data) {
				result = errors.Join(result, fmt.Errorf("回滚时目标发生变化，请检查 %s", s.path))
				continue
			}
			if s.exists {
				tmp, e := pal.StageTempFile(s.path, s.old, s.oldMode)
				if e == nil {
					e = pal.AtomicReplace(tmp, s.path)
					_ = os.Remove(tmp)
				}
				err = e
			} else {
				err = os.Remove(s.path)
				if err == nil {
					err = pal.SyncParentDir(s.path)
				}
			}
			result = errors.Join(result, err)
		}
	}()

	for i := range prepared {
		s := &prepared[i]
		if s.exists {
			current, err := os.ReadFile(s.path)
			if err != nil || !bytes.Equal(current, s.old) {
				return fmt.Errorf("目标在操作中发生变化：%s", s.path)
			}
			if err := pal.AtomicReplace(s.temp, s.path); err != nil {
				return err
			}
		} else {
			if err := os.Link(s.temp, s.path); err != nil {
				return err
			}
			_ = os.Remove(s.temp)
			if err := pal.SyncParentDir(s.path); err != nil {
				return err
			}
		}
		committed++
		s.temp = ""
	}
	return nil
}
