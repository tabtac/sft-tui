package pal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CopyFile 安全复制文件内容并落盘同步
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	inInfo, err := in.Stat()
	if err != nil {
		return err
	}

	if dstInfo, err := os.Stat(dst); err == nil {
		if os.SameFile(inInfo, dstInfo) {
			return fmt.Errorf("source and destination point to the same file: %s", src)
		}
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

// StageAndReplace 写入临时数据并原子发布到最终目标
func StageAndReplace(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepathDir(dst), ".stage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return AtomicReplace(tmpName, dst)
}

func filepathDir(p string) string {
	return filepath.Dir(p)
}
