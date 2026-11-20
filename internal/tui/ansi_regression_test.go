package tui

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"sft-tui/internal/config"
	"sft-tui/internal/monitor"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
)

func TestRegressionANSITruncationPreservesStyledText(t *testing.T) {
	const plain = "界面ABC"
	for _, style := range []struct {
		name, open string
	}{
		{"foreground", "\x1b[38;2;56;189;248m"},
		{"background", "\x1b[48;2;30;40;59m"},
		{"combined", "\x1b[1;38;2;56;189;248;48;2;30;40;59m"},
	} {
		for _, tc := range []struct {
			width int
			want  string
		}{
			{0, ""},
			{3, "界…"},
			{7, plain},
			{20, plain},
		} {
			t.Run(fmt.Sprintf("%s/width=%d", style.name, tc.width), func(t *testing.T) {
				// Literal SGR sequences keep this regression effective even
				// when go test runs without a terminal or color environment.
				input := style.open + plain + "\x1b[0m"
				got := truncCell(input, tc.width)
				if visible := ansi.Strip(got); visible != tc.want {
					t.Errorf("ANSI truncation changed visible text: got %q, want %q", visible, tc.want)
				}
				if gotWidth := lipgloss.Width(got); gotWidth > tc.width {
					t.Errorf("SGR bytes consumed cell budget: width %d exceeds %d", gotWidth, tc.width)
				}
				if tc.want != "" && !strings.Contains(got, style.open) {
					t.Error("truncation destroyed the original color/style sequence")
				}
			})
		}
	}
}

func TestRegressionANSIPaddingUsesVisibleCellWidth(t *testing.T) {
	const open = "\x1b[1;38;2;56;189;248m"
	const plain = "文A" // Three terminal cells, regardless of SGR byte length.
	input := open + plain + "\x1b[0m"
	for _, width := range []int{3, 8} {
		for _, right := range []bool{false, true} {
			t.Run(fmt.Sprintf("width=%d/right=%v", width, right), func(t *testing.T) {
				padding := strings.Repeat(" ", width-3)
				got, want := padLeftCell(input, width), padding+plain
				if right {
					got, want = padRightCell(input, width), plain+padding
				}
				if visible := ansi.Strip(got); visible != want {
					t.Errorf("ANSI padding changed visible text: got %q, want %q", visible, want)
				}
				if gotWidth := lipgloss.Width(got); gotWidth != width {
					t.Errorf("padding used escape bytes as cells: got width %d, want %d", gotWidth, width)
				}
				if !strings.Contains(got, open) {
					t.Error("padding destroyed the original color/style sequence")
				}
			})
		}
	}
}

// SGR fragments must disappear after valid ANSI sequences are stripped. A
// missing ESC instead leaves visible text such as [38;2;56;189;248m or [0m.
var regressionVisibleSGR = regexp.MustCompile(`\[(?:[0-9]+;)*[0-9]+m`)

func TestRegressionANSITrueColorInterfaceBarsDoNotLeakSGR(t *testing.T) {
	// Use a fresh renderer instead of mutating the caller's color settings.
	// This test must stay sequential while replacing Lip Gloss's default.
	previous := lipgloss.DefaultRenderer()
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	lipgloss.SetDefaultRenderer(renderer)
	t.Cleanup(func() { lipgloss.SetDefaultRenderer(previous) })

	for _, dark := range []bool{false, true} {
		renderer.SetHasDarkBackground(dark)
		for _, width := range []int{80, 120} {
			t.Run(fmt.Sprintf("dark=%v/width=%d", dark, width), func(t *testing.T) {
				th := makeTheme()
				if probe := th.badgeApp.Render("probe"); !strings.Contains(probe, "\x1b[") {
					t.Fatal("fixture must emit real ANSI styles even without a terminal")
				}
				server := &serverModel{
					th: th, width: width, height: 32,
					cfg: config.DefaultServer(), runtimeState: "服务未运行（离线）",
				}
				client := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
				client.th = th
				client.width, client.height = width, 32
				client.identityLabel = "alice"
				for _, bar := range []struct {
					name, view, label string
				}{
					{"server_header", server.renderHeader(), "SFTD-UI"},
					{"server_tabs", server.renderTabBar(), "1 概览"},
					{"server_footer", server.renderFooter(), "[s]"},
					{"client_header", client.renderHeader(), "SFT-TUI"},
					{"client_subheader", client.renderSubHeader(), "目录:"},
					{"client_footer", client.renderFooter(), "本地:"},
				} {
					t.Run(bar.name, func(t *testing.T) {
						visible := ansi.Strip(bar.view)
						if fragment := regressionVisibleSGR.FindString(visible); fragment != "" {
							t.Errorf("raw color code leaked into interface text: %q", fragment)
						}
						if !strings.Contains(visible, bar.label) {
							t.Errorf("color codes displaced expected label %q: %q", bar.label, visible)
						}
						if h := lipgloss.Height(bar.view); h != 1 {
							t.Errorf("interface bar wrapped to %d lines", h)
						}
						if w := lipgloss.Width(bar.view); w > width {
							t.Errorf("interface bar exceeds terminal width: %d > %d", w, width)
						}
					})
				}
				// 动态对比度：深浅色终端背景下 footerBar 应各自输出高对比度前景色。
				darkFooter := ""
				renderer.SetHasDarkBackground(true)
				darkFooter = th.footerBar.Render("footer")
				renderer.SetHasDarkBackground(false)
				lightFooter := th.footerBar.Render("footer")
				renderer.SetHasDarkBackground(dark)
				if darkFooter == lightFooter {
					t.Errorf("expected footerBar AdaptiveColor to change between dark and light backgrounds")
				}
			})
		}
	}
}

func TestRegressionANSIUntrustedInlineTextStillRejectsControls(t *testing.T) {
	// Preserving trusted renderer styles must not allow control sequences
	// from filenames, server errors or logs to reach the terminal unchanged.
	got := sanitizeInline("file\x1b[2Jname\r\n\t\x7f")
	if strings.ContainsAny(got, "\x1b\r\n\t\x7f") {
		t.Errorf("untrusted inline text retained terminal controls: %q", got)
	}
	if !strings.Contains(got, "file") || !strings.Contains(got, "name") {
		t.Error("sanitization removed the readable filename")
	}
}

func TestRegressionANSITrueColorFullViewsAndUntrustedPayloads(t *testing.T) {
	previous := lipgloss.DefaultRenderer()
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	lipgloss.SetDefaultRenderer(renderer)
	t.Cleanup(func() { lipgloss.SetDefaultRenderer(previous) })

	const evilEsc = "\x1b[2J"
	const evilOSC = "\x1b]0;PWN\x07"
	const evilC1 = "\u009b2K"

	checkRenderedView := func(t *testing.T, name, view string, maxW, maxH int) {
		t.Helper()
		if !strings.Contains(view, "\x1b[") {
			t.Fatalf("%s expected TrueColor ANSI styling in rendered output", name)
		}
		for _, badSeq := range []string{evilEsc, evilOSC, evilC1, "\u009b", "\r", "\x07"} {
			if strings.Contains(view, badSeq) {
				t.Errorf("%s leaked untrusted control payload %q", name, badSeq)
			}
		}
		visible := ansi.Strip(view)
		if frag := regressionVisibleSGR.FindString(visible); frag != "" {
			t.Errorf("%s leaked stripped/corrupted SGR fragment %q in:\n%s", name, frag, visible)
		}
		if h := lipgloss.Height(view); h > maxH {
			t.Errorf("%s height %d exceeded %d", name, h, maxH)
		}
		for i, ln := range strings.Split(visible, "\n") {
			if w := lipgloss.Width(ln); w > maxW {
				t.Errorf("%s line %d width %d exceeded %d: %q", name, i+1, w, maxW, ln)
			}
		}
	}

	cfg := config.DefaultServer()
	cfg.Listen = "127.0.0.1:7443"
	cfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice" + evilEsc, Certificate: "/tmp/alice" + evilOSC + ".crt"},
		{UserID: "bob" + evilC1, Disabled: true},
	}
	rt := cfg
	cfg.GlobalBandwidth = &config.Bandwidth{UploadBytesPerSecond: 2_000_000, DownloadBytesPerSecond: 3_000_000, BurstBytes: 256_000}
	cfg.Limits.MaxStreams = 16

	srv := &serverModel{
		th: makeTheme(), width: 100, height: 30,
		loaded: true, cfg: cfg,
		certs: []certRow{
			{Name: "ca" + evilEsc, Role: "ca", State: "有效", Expiry: "2028-01-01", Fingerprint: "ABCDEF1234567890"},
		},
		runtimeState: "运行中" + evilEsc,
		statusToast:  "提示" + evilEsc,
		runtime: &monitor.Snapshot{
			State:       "running",
			Listen:      rt.Listen,
			Started:     time.Now().Add(-time.Minute),
			OnlineUsers: 1,
			Config:      rt,
			Peers:       []monitor.Connection{{ID: 1, User: "alice" + evilEsc, Remote: "127.0.0.1:50000" + evilC1, Since: time.Now()}},
			Tasks:       []monitor.Task{{ID: 7, User: "alice" + evilEsc, Direction: "upload", File: "doc" + evilOSC + ".bin", Size: 1024, Offset: 512, State: "transferring"}},
		},
		logEvents: []monitor.Event{
			{Sequence: 1, Time: time.Now(), Level: "WARN" + evilC1, Message: "slow" + evilEsc, Detail: "pipe" + evilOSC},
			{Sequence: 2, Time: time.Now(), Level: "ERROR", Message: "deny\r\n" + evilEsc, Detail: "bad"},
		},
	}
	srv.cursors[4] = 3
	for page := 0; page <= 5; page++ {
		srv.page = page
		checkRenderedView(t, fmt.Sprintf("server_page_%d", page), srv.View(), 100, 30)
	}
	srv.page = 5
	srv.logEvents = nil
	srv.logFallback = "2026 WARN raw" + evilEsc + "line" + evilOSC + "\n"
	checkRenderedView(t, "server_log_fallback", srv.View(), 100, 30)

	for _, dims := range [][2]int{{80, 24}, {120, 32}} {
		w, h := dims[0], dims[1]
		renderer.SetHasDarkBackground(dims[0] == 80)
		cli := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
		cli.th = makeTheme()
		cli.width, cli.height = w, h
		cli.cfg = &config.Client{}
		cli.identityLabel = "alice" + evilEsc
		cli.connStatus = "已连接" + evilOSC
		cli.localDir = "/tmp/dir" + evilEsc
		cli.statusToast = "toast" + evilC1
		cli.recoveryNotice = "检测到未完成任务" + evilEsc
		cli.visibleLocal = []localEntry{
			{name: "local" + evilEsc + "file.txt", path: "/tmp/local", size: 4096, regular: true},
			{name: "bad" + evilOSC + ".bin", path: "/tmp/bad", size: 1024, regular: true},
		}
		cli.localSelected["/tmp/bad"] = true
		cli.visibleRemote = []protocol.FileEntry{
			{FileName: "remote" + evilEsc + "pkg.tar.gz", Size: 2048},
		}
		cli.activeTasks = []queue.Task{
			{ID: "t1", Direction: protocol.DirectionUpload, Remote: "up" + evilEsc + ".bin", State: queue.Transferring, Done: 512, Size: 1024, Speed: 256},
			{ID: "t2", Direction: protocol.DirectionDownload, Remote: "dn" + evilOSC + ".bin", State: queue.Pending, Error: "err" + evilEsc + "\r\nline"},
		}
		for _, area := range []focusArea{areaLocal, areaRemote, areaQueue} {
			cli.focus = area
			checkRenderedView(t, fmt.Sprintf("client_%dx%d_area_%d", w, h, area), cli.View(), w, h)
		}
	}
}

func TestRegressionHighlightRowPreservesBackgroundAcrossEmbeddedReset(t *testing.T) {
	previous := lipgloss.DefaultRenderer()
	renderer := lipgloss.NewRenderer(io.Discard)
	renderer.SetColorProfile(termenv.TrueColor)
	renderer.SetHasDarkBackground(true)
	lipgloss.SetDefaultRenderer(renderer)
	t.Cleanup(func() { lipgloss.SetDefaultRenderer(previous) })

	th := makeTheme()
	probe := th.rowCursor.Render("\x00")
	open, _, ok := strings.Cut(probe, "\x00")
	if !ok || open == "" {
		t.Fatalf("expected non-empty cursor SGR prefix in TrueColor mode, got %q", probe)
	}

	styled := "▶ alice  " + th.badgeOk.Render("✔ 启用") + "  upload,download"
	got := highlightRow(th, styled, 40)
	if !strings.HasPrefix(got, open) {
		t.Fatalf("highlighted row missing leading cursor SGR: %q", got)
	}
	if !strings.Contains(got, "\x1b[0m"+open) {
		t.Fatalf("embedded SGR reset was not followed by restored cursor highlight: %q", got)
	}
	if lipgloss.Width(got) != 40 {
		t.Fatalf("expected padded highlight width 40, got %d", lipgloss.Width(got))
	}
	if frag := regressionVisibleSGR.FindString(ansi.Strip(got)); frag != "" {
		t.Fatalf("corrupted SGR fragment leaked: %q", frag)
	}
}
