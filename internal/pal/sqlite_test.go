package pal

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteDSN(t *testing.T) {
	const query = "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"

	t.Run("windows_paths", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			dbPath  string
			want    string
			wantErr bool
		}{
			{
				name:   "drive_plain",
				dbPath: `C:\Users\runner\metadata.db`,
				want:   "file:///C:/Users/runner/metadata.db?" + query,
			},
			{
				name:   "drive_lowercase_and_forward_slashes",
				dbPath: `d:/data/queue-id.sqlite3`,
				want:   "file:///d:/data/queue-id.sqlite3?" + query,
			},
			{
				name:   "drive_unicode_space_and_uri_reserved_chars",
				dbPath: `C:\测试 目录\alice #1\pct%20name\q?mark\metadata.db`,
				want:   "file:///C:/%E6%B5%8B%E8%AF%95%20%E7%9B%AE%E5%BD%95/alice%20%231/pct%2520name/q%3Fmark/metadata.db?" + query,
			},
			{
				name:   "unc_path",
				dbPath: `\\fileserver\share\dir space\queue.sqlite3`,
				want:   "file:////fileserver/share/dir%20space/queue.sqlite3?" + query,
			},
			{
				name:   "extended_length_path",
				dbPath: `\\?\C:\long path\metadata.db`,
				want:   "file:///%5C%5C%3F%5CC:%5Clong%20path%5Cmetadata.db?" + query,
			},
			{
				name:    "relative_drive_path_rejected",
				dbPath:  `C:relative.db`,
				wantErr: true,
			},
			{
				name:    "empty_rejected",
				dbPath:  "   ",
				wantErr: true,
			},
			{
				name:    "nul_byte_rejected",
				dbPath:  "C:\\bad\x00.db",
				wantErr: true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := sqliteDSNWithSep(tc.dbPath, query, '\\', func(p string) (string, error) {
					return p, nil
				})
				if tc.wantErr {
					if err == nil {
						t.Fatalf("expected error for %q, got %q", tc.dbPath, got)
					}
					return
				}
				if err != nil {
					t.Fatalf("sqliteDSNWithSep(%q) error: %v", tc.dbPath, err)
				}
				if got != tc.want {
					t.Fatalf("sqliteDSNWithSep(%q)\n got: %q\nwant: %q", tc.dbPath, got, tc.want)
				}
				parsed, err := url.Parse(got)
				if err != nil {
					t.Fatalf("url.Parse(%q): %v", got, err)
				}
				if parsed.Scheme != "file" || parsed.Host != "" {
					t.Fatalf("unexpected URI authority host=%q scheme=%q for %q", parsed.Host, parsed.Scheme, got)
				}
			})
		}
	})

	t.Run("native_paths", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "测试 空格", "sharp#dir", "pct%20dir", "metadata.db")
		dsn, err := SQLiteDSN(dbPath, query)
		if err != nil {
			t.Fatalf("SQLiteDSN(%q) error: %v", dbPath, err)
		}
		if !strings.HasPrefix(dsn, "file:///") {
			t.Fatalf("expected triple-slash file URI, got %q", dsn)
		}
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", dsn, err)
		}
		if parsed.Host != "" {
			t.Fatalf("expected empty URI authority host, got %q in %q", parsed.Host, dsn)
		}
		if parsed.RawQuery != query {
			t.Fatalf("query mismatch: got %q want %q", parsed.RawQuery, query)
		}

		t.Chdir(dir)
		relDSN, err := SQLiteDSN(filepath.Join("rel", "sub.db"), query)
		if err != nil {
			t.Fatalf("SQLiteDSN(relative) error: %v", err)
		}
		if !strings.HasPrefix(relDSN, "file:///") {
			t.Fatalf("expected relative path resolved to file:/// URI, got %q", relDSN)
		}
	})
}
