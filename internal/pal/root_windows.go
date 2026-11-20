//go:build windows

package pal

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CurrentUserSID 获取当前进程的真实用户 SID
func CurrentUserSID() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", fmt.Errorf("open current process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query token user: %w", err)
	}
	return user.User.Sid.String(), nil
}

// PrivateSecurityDescriptor 生成仅授予当前用户与 SYSTEM 完全控制权的不可继承 DACL
func PrivateSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := CurrentUserSID()
	if err != nil {
		return nil, err
	}
	// D:P 代表受保护 DACL (禁用继承)，仅允许 SYSTEM(SY) 与当前用户(sid)
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + sid + ")"
	return windows.SecurityDescriptorFromString(sddl)
}

// applyPrivateDACL 为目录句柄应用仅当前用户与 SYSTEM 可访问的受保护 DACL
func applyPrivateDACL(h windows.Handle) error {
	sd, err := PrivateSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

// EnsurePrivateDir 在 Windows 下确保私有目录安全：
// 1. 若已存在，强制校验不是重解析点/符号链接，并应用私有安全描述符。
// 2. 若不存在，逐级以私有安全描述符创建缺失祖先及目标目录。
func EnsurePrivateDir(path string) error {
	path = filepath.Clean(path)
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}

	opened, err := openAndHardenPrivateDir(p, path)
	if opened || err != nil {
		return err
	}

	sd, err := PrivateSecurityDescriptor()
	if err != nil {
		return err
	}
	secAttr := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}

	if parent := filepath.Dir(path); parent != path {
		if err := createMissingPrivateParents(parent, secAttr); err != nil {
			return err
		}
	}

	if err := windows.CreateDirectory(p, secAttr); err != nil {
		if os.IsExist(err) {
			opened, openErr := openAndHardenPrivateDir(p, path)
			if openErr != nil {
				return openErr
			}
			if opened {
				return nil
			}
		}
		return &os.PathError{Op: "CreateDirectory", Path: path, Err: err}
	}
	return nil
}

func openAndHardenPrivateDir(p *uint16, path string) (bool, error) {
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, &os.PathError{Op: "CreateFile", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return false, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return false, fmt.Errorf("security violation: %q is not a directory", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, fmt.Errorf("security violation: %q is a reparse point/symlink", path)
	}
	return true, applyPrivateDACL(h)
}

func createMissingPrivateParents(dir string, secAttr *windows.SecurityAttributes) error {
	dir = filepath.Clean(dir)
	if _, err := os.Lstat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := createMissingPrivateParents(parent, secAttr); err != nil {
			return err
		}
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(p, secAttr); err != nil && !os.IsExist(err) {
		return &os.PathError{Op: "CreateDirectory", Path: dir, Err: err}
	}
	return nil
}

// secureSubdir 对根目录句柄内的已有子目录收紧私有 DACL（同时验证句柄指向同目录、非重解析点）
func (r *Root) secureSubdir(sub string) error {
	pinned, err := r.root.Open(sub)
	if err != nil {
		return err
	}
	defer pinned.Close()

	var pinnedInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(pinned.Fd()), &pinnedInfo); err != nil {
		return err
	}

	targetPath, err := SecureJoin(r.path, sub)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}

	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return &os.PathError{Op: "open subdir", Path: targetPath, Err: err}
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("security violation: sub-path %q is a reparse point or non-directory", sub)
	}
	if info.VolumeSerialNumber != pinnedInfo.VolumeSerialNumber ||
		info.FileIndexHigh != pinnedInfo.FileIndexHigh ||
		info.FileIndexLow != pinnedInfo.FileIndexLow {
		return fmt.Errorf("security violation: sub-path %q changed during hardening", sub)
	}
	return applyPrivateDACL(h)
}

// SyncRootSubdir Windows 目录不支持 fsync，由 NTFS 日志保障元数据一致性
func (r *Root) SyncRootSubdir(sub string) error {
	return nil
}
