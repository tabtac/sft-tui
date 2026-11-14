package pal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSecureJoinRejectsEscapes(t *testing.T) {
	for _, path := range []string{
		"..", "../a", "a/../../b", `..\a`, `a\..\..\b`,
		"/etc/passwd", `\Windows`, `\\server\share\a`,
		"C:/a", `C:\a`, "c:relative", `\\?\C:\a`, `\\.\pipe\a`,
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := SecureJoin(t.TempDir(), path); !errors.Is(err, ErrPathTraversal) {
				t.Errorf("expected ErrPathTraversal for %q, got %v", path, err)
			}
		})
	}
}

func TestReviewSecureJoinAcceptsContainedPaths(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"a", "a"}, {"a/b", "a/b"}, {`a\b`, "a/b"},
		{"a/../b", "b"}, {"中文/文件", "中文/文件"},
		{"with spaces/a", "with spaces/a"}, {"..hidden", "..hidden"},
		{"..hidden/a", "..hidden/a"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			got, err := SecureJoin(root, tc.path)
			// BUG: HasPrefix("..") 错把 ..hidden 等合法目录名当作父目录逃逸。
			if err != nil {
				t.Fatalf("BUG: contained path rejected: %v", err)
			}
			want := filepath.Join(root, filepath.FromSlash(tc.want))
			if got != want {
				t.Errorf("got=%q want=%q", got, want)
			}
		})
	}
}

func TestReviewStageParentUsesNativePathRules(t *testing.T) {
	for _, path := range []string{"a", "a/b", filepath.Join(string(filepath.Separator), "a"), filepath.Join("a", "b", "c")} {
		t.Run(path, func(t *testing.T) {
			// BUG: 手写 filepathDir 对 /a 返回空串，StageAndReplace 会在系统临时目录创建临时文件。
			if got, want := filepathDir(path), filepath.Dir(path); got != want {
				t.Errorf("BUG: temporary file parent=%q want=%q", got, want)
			}
		})
	}
}

func TestReviewRootRejectsExternalSymlink(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, path := range []string{"escape/secret", "../outside", filepath.Join(outside, "secret")} {
		f, err := root.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
		if f != nil {
			f.Close()
		}
		if err == nil {
			t.Errorf("root escaped through %q", path)
		}
	}
	if data, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(data) != "safe" {
		t.Errorf("outside file modified: %q err=%v", data, err)
	}
}

func TestReviewSecureSubdirRejectsSymlinkAndFile(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			root, err := OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			path := filepath.Join(base, "sub")
			if kind == "file" {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if err := root.EnsureSecureSubdir("sub"); err == nil {
				t.Error("irregular private subdirectory accepted")
			}
		})
	}
}

func TestReviewRootPathIsAbsolute(t *testing.T) {
	// t.Chdir 会自动恢复目录，此测试及其父测试均不使用 t.Parallel。
	t.Chdir(t.TempDir())
	root, err := OpenRoot("relative")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// BUG: Path 的注释承诺绝对路径，当前原样保留输入，SQLite 后续可能随工作目录变化。
	if !filepath.IsAbs(root.Path()) {
		t.Errorf("BUG: Root.Path returned relative path %q", root.Path())
	}
}
