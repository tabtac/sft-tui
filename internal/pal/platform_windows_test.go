//go:build windows

package pal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func reviewAssertPrivateDACL(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	sid, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatal("BUG: NULL DACL grants unrestricted access")
	}
	seenUser := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		allowedSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if allowedSID == sid {
			seenUser = true
		}
		if allowedSID != sid && allowedSID != "S-1-5-18" && ace.Mask != 0 {
			t.Errorf("BUG: private resource grants SID %s access mask %#x", allowedSID, ace.Mask)
		}
	}
	if !seenUser {
		t.Error("private resource does not grant current user access")
	}
}

func TestReviewWindowsPrivateDirectoryACL(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private")
			if existing {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := EnsurePrivateDir(path); err != nil {
				t.Fatal(err)
			}
			sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			reviewAssertPrivateDACL(t, sd)
		})
	}
}

func TestReviewWindowsExistingSubdirectoryACLIsTightened(t *testing.T) {
	base := t.TempDir()
	root, err := OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := filepath.Join(base, "sub")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	// 预设 Everyone 完全控制。单纯 Chmod(0700) 不能移除 Windows DACL 中该 ACE。
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.EnsureSecureSubdir("sub"); err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	// BUG: EnsureSecureSubdir 当前仅 Chmod，未应用私有 DACL。
	reviewAssertPrivateDACL(t, after)
}

func TestReviewWindowsNamedPipeHasPrivateACL(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	listener, err := ListenIPC(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	path, err := windows.UTF16PtrFromString(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	// BUG: ListenIPC 未传入 SecurityDescriptor，默认管道 ACL 不保证仅当前 SID 与 SYSTEM。
	reviewAssertPrivateDACL(t, sd)
}

func TestReviewWindowsPipePathCanonicalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance")
	if GetIPCPath(path) != GetIPCPath(strings.ToUpper(path)) {
		t.Error("Windows case aliases produced different pipe names")
	}
}

func TestReviewWindowsLockFileRejectsDirectory(t *testing.T) {
	f, err := OpenLockFile(t.TempDir())
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Error("directory accepted as a lock file")
	}
}

func TestReviewWindowsRootRejectsDevicesAndAlternateStreams(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"NUL", "CON", "CON.txt", "COM1", "LPT1.txt", "file:stream", "file::$DATA", `C:relative`} {
		t.Run(name, func(t *testing.T) {
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0600)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Errorf("unsafe Windows path accepted: %q", name)
			}
		})
	}
}

func TestReviewWindowsReplaceCanRetryAfterOwnReaderCloses(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source")
	dst := filepath.Join(base, "destination")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := AtomicReplace(src, dst); err == nil {
		t.Error("replacement unexpectedly succeeded while non-delete-sharing reader was open")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "old" {
		t.Errorf("failed replacement damaged destination: %q %v", got, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := AtomicReplace(src, dst); err != nil {
		t.Fatalf("retry after reader close: %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "new" {
		t.Errorf("retry content: %q %v", got, err)
	}
}

func TestWindowsNamedPipeRapidListenAcceptCloseNoDeadlock(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 300; i++ {
		endpoint := GetNamedIPCPath(base, "probe")
		ln, err := ListenIPC(endpoint)
		if err != nil {
			t.Fatalf("iter %d ListenIPC: %v", i, err)
		}
		acceptDone := make(chan struct{})
		go func() {
			defer close(acceptDone)
			conn, err := ln.Accept()
			if err == nil && conn != nil {
				_ = conn.Close()
			}
		}()

		closeDone := make(chan error, 1)
		go func() {
			closeDone <- ln.Close()
		}()

		select {
		case err := <-closeDone:
			if err != nil {
				t.Fatalf("iter %d Close error: %v", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("iter %d Close deadlocked without clients", i)
		}
		select {
		case <-acceptDone:
		case <-time.After(3 * time.Second):
			t.Fatalf("iter %d Accept did not exit after Close", i)
		}
	}
}
