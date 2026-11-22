package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
)

func (m *model) openTaskDetail(t queue.Task) {
	m.detailTitle = "任务详情"
	m.detailLines = buildTaskDetailLines(
		directionIcon(t.Direction),
		taskStateText(t),
		t.Remote,
		t.Path,
		t.Done,
		t.Size,
		t.SHA256,
		t.TransferID,
		t.Error,
		t.Updated,
	)
	m.infoScroll = 0
	m.modal = modalDetail
}

func (m *model) currentQueueTasks() []queue.Task {
	if m.historyView {
		return m.historyTasks
	}
	return m.activeTasks
}

func (m *model) selectedTask() *queue.Task {
	tasks := m.currentQueueTasks()
	if m.queueCursor < 0 || m.queueCursor >= len(tasks) {
		return nil
	}
	t := tasks[m.queueCursor]
	return &t
}

func (m *model) clampQueueCursor() {
	n := len(m.currentQueueTasks())
	if m.queueCursor >= n {
		m.queueCursor = max(0, n-1)
	}
}

func (m *model) refreshQueueSnapshots() bool {
	if m.runner != nil {
		m.connStatus = m.runner.Status()
	}
	if m.manager == nil {
		return false
	}
	if snap := m.manager.TrySnapshot(false); snap != nil {
		m.activeTasks = snap
	}
	if hist := m.manager.TrySnapshot(true); hist != nil {
		m.historyTasks = hist
	}
	hasUnfinished := false
	for _, t := range m.activeTasks {
		if !t.Terminal() {
			hasUnfinished = true
			break
		}
	}
	if !hasUnfinished {
		m.recoveryNotice = ""
	}
	m.clampQueueCursor()
	m.evaluateBatches()

	refreshFiles := false
	for _, t := range m.activeTasks {
		if t.State == queue.Completed && !m.completedSeen[t.ID] {
			m.completedSeen[t.ID] = true
			refreshFiles = true
		}
	}
	return refreshFiles
}

func (m *model) recordEnqueuedBatch(tasks []queue.Task) {
	if len(tasks) == 0 {
		return
	}
	if m.activeBatches == nil {
		m.activeBatches = make(map[string]*batchTracker)
	}
	batchID := tasks[0].Batch
	if batchID == "" {
		batchID = tasks[0].ID
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	m.activeBatches[batchID] = &batchTracker{
		id:      batchID,
		started: time.Now(),
		taskIDs: ids,
	}
}

func (m *model) evaluateBatches() {
	if len(m.activeBatches) == 0 {
		return
	}
	byID := make(map[string]queue.Task, len(m.activeTasks)+len(m.historyTasks))
	for _, t := range m.activeTasks {
		byID[t.ID] = t
	}
	for _, t := range m.historyTasks {
		byID[t.ID] = t
	}

	for id, bt := range m.activeBatches {
		var s batchSummary
		s.BatchID = id
		s.Started = bt.started
		s.Total = len(bt.taskIDs)
		settled := true
		for _, tid := range bt.taskIDs {
			t, ok := byID[tid]
			if !ok {
				settled = false
				break
			}
			switch {
			case t.Terminal():
				switch t.State {
				case queue.Completed:
					s.Succeeded++
					s.TotalBytes += t.Size
				case queue.Skipped:
					s.Skipped++
				case queue.Cancelled:
					if t.Error != "" {
						s.Failed++
						s.FailedList = append(s.FailedList, batchFailedItem{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Error: t.Error})
					} else {
						s.Cancelled++
					}
				}
			case !t.State.Active() && t.Error != "":
				// preflight 或执行出错转入 Pending/Paused 状态
				s.Failed++
				s.FailedList = append(s.FailedList, batchFailedItem{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Error: t.Error})
			default:
				settled = false
			}
		}
		if !settled {
			continue
		}
		s.Finished = time.Now()
		delete(m.activeBatches, id)
		sCopy := s
		m.lastBatch = &sCopy
		m.batchHistory = append([]batchSummary{s}, m.batchHistory...)
		if len(m.batchHistory) > 20 {
			m.batchHistory = m.batchHistory[:20]
		}
		if s.Failed > 0 {
			m.setToastError(fmt.Sprintf("批次完成：成功 %d · 失败 %d · 跳过 %d（按 B 查摘要 / R 重失败）", s.Succeeded, s.Failed, s.Skipped))
		} else {
			m.setToast(fmt.Sprintf("批次完成：成功 %d · 跳过 %d（按 B 查看摘要）", s.Succeeded, s.Skipped))
		}
	}
}

func (m *model) failedTaskCount() int {
	n := 0
	for _, t := range m.activeTasks {
		if !t.WantCancel && !t.State.Active() && t.Error != "" && (t.State == queue.Pending || t.State == queue.Paused) {
			n++
		}
	}
	return n
}

func (m *model) retryFailedTasksCmd() tea.Cmd {
	if m.manager == nil {
		m.setToast("请先配置或导入身份（按 i）")
		return nil
	}
	if m.opBusy {
		m.setToast("已有队列控制操作正在执行，请稍候…")
		return nil
	}
	var ids []string
	for _, t := range m.activeTasks {
		if !t.WantCancel && !t.State.Active() && t.Error != "" && (t.State == queue.Pending || t.State == queue.Paused) {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		m.setToast("当前队列没有失败或报错待处理的任务")
		return nil
	}
	mgr := m.manager
	m.opBusy = true
	m.opLabel = fmt.Sprintf("正在重试 %d 个失败任务", len(ids))
	m.opStart = time.Now()
	return func() tea.Msg {
		resumed := 0
		var firstErr error
		for _, id := range ids {
			if err := mgr.Resume(id); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				resumed++
			}
		}
		if resumed == 0 && firstErr != nil {
			return batchOpResultMsg{err: firstErr, isGlobalOp: true}
		}
		notice := fmt.Sprintf("已重新提交 %d 个失败任务继续传输", resumed)
		if firstErr != nil {
			notice += "（部分失败：" + firstErr.Error() + "）"
		}
		return batchOpResultMsg{notice: notice, isGlobalOp: true}
	}
}

func (m *model) batchSummaryModalData() (title string, lines []string, footer string) {
	b := m.lastBatch
	if b == nil && len(m.batchHistory) > 0 {
		b = &m.batchHistory[0]
	}
	if b == nil {
		return "批次传输结果摘要", []string{"当前会话尚无已完成批次。"}, "Enter / Esc 关闭"
	}
	lines = []string{
		fmt.Sprintf("批次统计：总计 %d · 成功 %d · 失败 %d · 取消 %d · 跳过 %d", b.Total, b.Succeeded, b.Failed, b.Cancelled, b.Skipped),
		fmt.Sprintf("累计传输：%s · 耗时 %s", progress.FormatBytes(b.TotalBytes), b.Finished.Sub(b.Started).Round(time.Millisecond)),
	}
	if len(b.FailedList) > 0 {
		lines = append(lines, "", m.th.badgeErr.Render(fmt.Sprintf("失败项清单 (%d 项，按 [R] 一键重试)：", len(b.FailedList))))
		for _, item := range b.FailedList {
			lines = append(lines, fmt.Sprintf("  • %s %s — %s", directionIcon(item.Direction), sanitizeInline(item.Remote), sanitizeInline(item.Error)))
		}
	} else {
		lines = append(lines, "", m.th.badgeOk.Render("✔ 本批无失败项"))
	}
	return "批次传输结果摘要", lines, "↑↓ 滚动 · R 重试失败项 · Enter / Esc 关闭"
}

func clampSortKind(v int) sortKind {
	if v < int(sortNameAsc) || v > int(sortSizeAsc) {
		return sortNameAsc
	}
	return sortKind(v)
}

func (m *model) currentPrefs() uiPrefs {
	return uiPrefs{
		LocalOrder:  int(m.localSort),
		RemoteOrder: int(m.remoteSort),
	}
}

func (m *model) activeSortLabel() string {
	if m.focus == areaRemote {
		return "远端:" + m.sortLabel(m.remoteSort)
	}
	return "本地:" + m.sortLabel(m.localSort)
}

func (m *model) sortLabel(mode sortKind) string {
	switch mode {
	case sortNameDesc:
		return "名称↓"
	case sortSizeDesc:
		return "大小↓"
	case sortSizeAsc:
		return "大小↑"
	default:
		return "名称↑"
	}
}

func (m *model) rebuildLocalVisible() {
	focusPath := m.pendingLocalFocusPath
	m.pendingLocalFocusPath = ""
	if focusPath == "" && m.localCursor >= 0 && m.localCursor < len(m.visibleLocal) {
		focusPath = m.visibleLocal[m.localCursor].path
	}

	q := strings.ToLower(strings.TrimSpace(m.query))
	var out []localEntry
	for _, e := range m.localEntries {
		if q != "" && e.name != "../" && !strings.Contains(strings.ToLower(e.name), q) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.name == "../" || b.name == "../" {
			return a.name == "../"
		}
		if a.dir != b.dir {
			return a.dir
		}
		switch m.localSort {
		case sortNameDesc:
			return strings.ToLower(a.name) > strings.ToLower(b.name)
		case sortSizeDesc:
			if a.size != b.size {
				return a.size > b.size
			}
		case sortSizeAsc:
			if a.size != b.size {
				return a.size < b.size
			}
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	m.visibleLocal = out
	if focusPath != "" {
		for i, e := range out {
			if e.path == focusPath {
				m.localCursor = i
				return
			}
		}
	}
	if m.localCursor >= len(out) {
		m.localCursor = max(0, len(out)-1)
	}
}

func (m *model) rebuildRemoteVisible() {
	focusName := ""
	if m.remoteCursor >= 0 && m.remoteCursor < len(m.visibleRemote) {
		focusName = m.visibleRemote[m.remoteCursor].FileName
	}

	q := strings.ToLower(strings.TrimSpace(m.query))
	var out []protocol.FileEntry
	for _, f := range m.remoteEntries {
		if q != "" && !strings.Contains(strings.ToLower(f.FileName), q) {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch m.remoteSort {
		case sortNameDesc:
			return strings.ToLower(a.FileName) > strings.ToLower(b.FileName)
		case sortSizeDesc:
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		case sortSizeAsc:
			if a.Size != b.Size {
				return a.Size < b.Size
			}
		}
		return strings.ToLower(a.FileName) < strings.ToLower(b.FileName)
	})
	m.visibleRemote = out
	if focusName != "" {
		for i, f := range out {
			if f.FileName == focusName {
				m.remoteCursor = i
				return
			}
		}
	}
	if m.remoteCursor >= len(out) {
		m.remoteCursor = max(0, len(out)-1)
	}
}

func (m *model) clearSelectionForDir(dir protocol.Direction) {
	if dir == protocol.DirectionUpload {
		m.localSelected = make(map[string]bool)
	} else {
		m.remoteSelected = make(map[string]bool)
	}
}

func (m *model) prepareUpload() tea.Cmd {
	if m.manager == nil {
		m.setToast("请先配置或导入身份（按 i）")
		return nil
	}
	var chosen []localEntry
	if len(m.localSelected) > 0 {
		for _, e := range m.localEntries {
			if e.regular && m.localSelected[e.path] {
				chosen = append(chosen, e)
			}
		}
	} else if m.localCursor >= 0 && m.localCursor < len(m.visibleLocal) {
		if e := m.visibleLocal[m.localCursor]; e.regular {
			chosen = append(chosen, e)
		}
	}
	if len(chosen) == 0 {
		m.setToast("请先选中至少一个普通文件")
		return nil
	}

	specs := make([]queue.Spec, 0, len(chosen))
	for _, e := range chosen {
		specs = append(specs, queue.Spec{
			Direction: protocol.DirectionUpload,
			Remote:    filepath.Base(e.path),
			Path:      e.path,
			Size:      uint64(max(0, e.size)),
		})
	}
	return m.prepareBatch(protocol.DirectionUpload, specs, true)
}

func (m *model) prepareDownload() tea.Cmd {
	if m.manager == nil {
		m.setToast("请先配置或导入身份（按 i）")
		return nil
	}
	var chosen []protocol.FileEntry
	if len(m.remoteSelected) > 0 {
		for _, f := range m.remoteEntries {
			if m.remoteSelected[f.FileName] {
				chosen = append(chosen, f)
			}
		}
	} else if m.remoteCursor >= 0 && m.remoteCursor < len(m.visibleRemote) {
		chosen = append(chosen, m.visibleRemote[m.remoteCursor])
	}
	if len(chosen) == 0 {
		m.setToast("请先选中至少一个远端文件")
		return nil
	}

	specs := make([]queue.Spec, 0, len(chosen))
	for _, f := range chosen {
		specs = append(specs, queue.Spec{
			Direction: protocol.DirectionDownload,
			Remote:    f.FileName,
			Path:      filepath.Join(m.localDir, f.FileName),
			Size:      f.Size,
		})
	}
	return m.prepareBatch(protocol.DirectionDownload, specs, true)
}

func (m *model) prepareDeleteRemote() {
	if m.cfg == nil || m.runner == nil {
		m.setToast("请先按 i 选择或导入客户端身份")
		return
	}
	if m.remoteErr != "" {
		m.setToast("远端连接暂不可用，请按 r 刷新后再删除")
		return
	}
	if m.opBusy {
		m.setToast("已有操作正在执行，请稍候…")
		return
	}
	var names []string
	if len(m.remoteSelected) > 0 {
		for _, f := range m.remoteEntries {
			if m.remoteSelected[f.FileName] {
				names = append(names, f.FileName)
			}
		}
	} else if m.remoteCursor >= 0 && m.remoteCursor < len(m.visibleRemote) {
		names = append(names, m.visibleRemote[m.remoteCursor].FileName)
	}
	if len(names) == 0 {
		m.setToast("请选择要删除的远端文件")
		return
	}
	m.pendingDeleteNames = names
	m.infoScroll = 0
	m.clearToast()
	m.modal = modalDeleteRemote
}

func (m *model) deleteRemoteFilesCmd(names []string) tea.Cmd {
	runner := m.runner
	return func() tea.Msg {
		if runner == nil {
			return remoteDeletedMsg{failed: names, firstErr: fmt.Errorf("未连接到远端服务")}
		}
		var deleted, failed []string
		var firstErr error
		for _, name := range names {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := runner.Delete(ctx, name)
			cancel()
			if err != nil {
				failed = append(failed, name)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			deleted = append(deleted, name)
		}
		return remoteDeletedMsg{deleted: deleted, failed: failed, firstErr: firstErr}
	}
}

func (m *model) prepareBatch(dir protocol.Direction, specs []queue.Spec, clearSel bool) tea.Cmd {
	var conflicts []string
	if dir == protocol.DirectionUpload {
		remoteSet := make(map[string]bool, len(m.remoteEntries))
		for _, f := range m.remoteEntries {
			remoteSet[f.FileName] = true
		}
		for _, s := range specs {
			if remoteSet[s.Remote] {
				conflicts = append(conflicts, s.Remote)
			}
		}
	} else {
		for _, s := range specs {
			if _, err := os.Lstat(s.Path); err == nil {
				conflicts = append(conflicts, filepath.Base(s.Path))
			}
		}
	}
	if len(conflicts) > 0 {
		m.pendingBatchDir = dir
		m.pendingSpecs = specs
		m.conflictNames = conflicts
		m.pendingClearSel = clearSel
		m.infoScroll = 0
		m.clearToast()
		m.modal = modalConflict
		return nil
	}
	if clearSel {
		m.clearSelectionForDir(dir)
	}
	return m.enqueueSpecsCmd(specs)
}

func (m *model) resolveConflictsAutoRename() []queue.Spec {
	ignoreCase := m.pendingBatchDir != protocol.DirectionUpload && runtime.GOOS == "windows"
	var remoteUsed map[string]bool
	var dirUsed map[string]map[string]bool
	if m.pendingBatchDir == protocol.DirectionUpload {
		remoteUsed = make(map[string]bool, len(m.remoteEntries))
		for _, f := range m.remoteEntries {
			remoteUsed[f.FileName] = true
		}
	} else {
		dirUsed = make(map[string]map[string]bool)
	}
	out := make([]queue.Spec, 0, len(m.pendingSpecs))
	for _, s := range m.pendingSpecs {
		if m.pendingBatchDir == protocol.DirectionUpload {
			name := uniqueName(s.Remote, remoteUsed)
			remoteUsed[name] = true
			s.Remote = name
		} else {
			dir := filepath.Dir(s.Path)
			used := dirUsed[dir]
			if used == nil {
				used = make(map[string]bool)
				entries, err := os.ReadDir(dir)
				if err == nil {
					for _, e := range entries {
						k := e.Name()
						if ignoreCase {
							k = strings.ToLower(k)
						}
						used[k] = true
					}
				}
				if filepath.Clean(dir) == filepath.Clean(m.localDir) || len(entries) == 0 {
					for _, e := range m.localEntries {
						k := e.name
						if ignoreCase {
							k = strings.ToLower(k)
						}
						used[k] = true
					}
				}
				dirUsed[dir] = used
			}
			base := filepath.Base(s.Path)
			name := uniqueNameFold(base, used, ignoreCase)
			k := name
			if ignoreCase {
				k = strings.ToLower(k)
			}
			used[k] = true
			s.Path = filepath.Join(dir, name)
		}
		out = append(out, s)
	}
	return out
}

func (m *model) resolveConflictsSkip() []queue.Spec {
	conflictSet := make(map[string]bool, len(m.conflictNames))
	ignoreCase := m.pendingBatchDir != protocol.DirectionUpload && runtime.GOOS == "windows"
	for _, c := range m.conflictNames {
		if ignoreCase {
			conflictSet[strings.ToLower(c)] = true
		} else {
			conflictSet[c] = true
		}
	}
	out := make([]queue.Spec, 0, len(m.pendingSpecs))
	for _, s := range m.pendingSpecs {
		key := s.Remote
		if s.Direction == protocol.DirectionDownload {
			key = filepath.Base(s.Path)
			if ignoreCase {
				key = strings.ToLower(key)
			}
		}
		if conflictSet[key] {
			s.Skip = "用户跳过重名文件"
		}
		out = append(out, s)
	}
	return out
}

func trimUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	n := 0
	for i, r := range s {
		w := utf8.RuneLen(r)
		if w < 0 || i+w > maxBytes {
			return s[:n]
		}
		n = i + w
	}
	return s[:n]
}

func uniqueName(orig string, used map[string]bool) string {
	return uniqueNameFold(orig, used, false)
}

func uniqueNameFold(orig string, used map[string]bool, ignoreCase bool) string {
	isUsed := func(name string) bool {
		if ignoreCase {
			return used[strings.ToLower(name)]
		}
		return used[name]
	}
	if !isUsed(orig) {
		return orig
	}
	ext := filepath.Ext(orig)
	stem := strings.TrimSuffix(orig, ext)
	if len(ext) > 240 {
		ext = ""
		stem = orig
	}
	for i := 1; i < 10000; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		budget := protocol.MaxFileNameBytes - len(suffix) - len(ext)
		trimmed := strings.TrimRight(trimUTF8Bytes(stem, budget), " .")
		cand := fmt.Sprintf("%s%s%s", trimmed, suffix, ext)
		if trimmed == "" && budget > 0 {
			cand = fmt.Sprintf("%s-%d%s", trimUTF8Bytes(stem, budget-2), i, ext)
		}
		if len(cand) <= protocol.MaxFileNameBytes && protocol.ValidateName(cand) == nil && !isUsed(cand) {
			return cand
		}
	}
	suffix := fmt.Sprintf("-%d", time.Now().UnixNano())
	budget := protocol.MaxFileNameBytes - len(suffix) - len(ext)
	return fmt.Sprintf("%s%s%s", trimUTF8Bytes(stem, budget), suffix, ext)
}

func (m *model) enqueueSpecsCmd(specs []queue.Spec) tea.Cmd {
	mgr := m.manager
	return func() tea.Msg {
		tasks, err := mgr.Enqueue(specs)
		if err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: fmt.Sprintf("已入队 %d 个传输任务", len(tasks)), enqueued: tasks}
	}
}

func resumePendingOrPausedTask(mgr *queue.Manager, task queue.Task) tea.Msg {
	if task.WantCancel {
		if err := mgr.Cancel(task.ID); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: "重试取消：" + task.Remote}
	}
	if err := mgr.Resume(task.ID); err != nil {
		return batchOpResultMsg{err: err}
	}
	return batchOpResultMsg{notice: "已恢复任务：" + task.Remote}
}

func (m *model) toggleSelectedTaskCmd() tea.Cmd {
	if m.manager == nil {
		return nil
	}
	t := m.selectedTask()
	if t == nil || t.Terminal() {
		return nil
	}
	mgr := m.manager
	task := *t
	if live := mgr.Task(t.ID); live != nil {
		if live.Terminal() {
			return nil
		}
		task = *live
	}
	return func() tea.Msg {
		switch {
		case task.State.Active():
			if err := mgr.Pause(task.ID); err != nil {
				return batchOpResultMsg{err: err}
			}
			return batchOpResultMsg{notice: "已请求暂停：" + task.Remote}
		case task.State == queue.Paused || task.State == queue.Pending:
			return resumePendingOrPausedTask(mgr, task)
		}
		return batchOpResultMsg{}
	}
}

// resumeOrRetrySelectedCmd 在队列/历史区按 F5 时根据选中任务状态执行恢复、重试或重传。
func (m *model) resumeOrRetrySelectedCmd() tea.Cmd {
	if m.manager == nil {
		m.setToast("请先配置或导入身份（按 i）")
		return nil
	}
	t := m.selectedTask()
	if t == nil {
		m.setToast("当前队列无选中任务")
		return nil
	}
	task := *t
	if live := m.manager.Task(t.ID); live != nil {
		task = *live
	}
	if m.historyView || task.Terminal() {
		spec := queue.Spec{Direction: task.Direction, Remote: task.Remote, Path: task.Path, Size: task.Size}
		return m.prepareBatch(task.Direction, []queue.Spec{spec}, false)
	}
	if task.State == queue.Paused || task.State == queue.Pending {
		mgr := m.manager
		return func() tea.Msg {
			return resumePendingOrPausedTask(mgr, task)
		}
	}
	m.setToast(fmt.Sprintf("任务当前正在进行（%s），无需恢复或重试", task.State))
	return nil
}

func (m *model) pauseSelectedCmd() tea.Cmd {
	t := m.selectedTask()
	if m.manager == nil || t == nil {
		return nil
	}
	mgr := m.manager
	id := t.ID
	name := t.Remote
	return func() tea.Msg {
		if err := mgr.Pause(id); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: "已请求暂停：" + name}
	}
}

func (m *model) cancelSelectedCmd() tea.Cmd {
	t := m.selectedTask()
	if m.manager == nil || t == nil {
		return nil
	}
	mgr := m.manager
	id := t.ID
	name := t.Remote
	return func() tea.Msg {
		if err := mgr.Cancel(id); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: "已请求取消：" + name}
	}
}

func (m *model) toggleAllCmd() tea.Cmd {
	if m.manager == nil {
		return nil
	}
	if m.opBusy {
		m.setToast("已有队列控制操作正在执行，请稍候…")
		return nil
	}
	hasActive := false
	for _, t := range m.activeTasks {
		if t.State.Active() {
			hasActive = true
			break
		}
	}
	mgr := m.manager
	m.opBusy = true
	m.opStart = time.Now()
	if hasActive {
		m.opLabel = "正在全部暂停"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := mgr.PauseAll(ctx); err != nil {
				return batchOpResultMsg{err: err, isGlobalOp: true}
			}
			return batchOpResultMsg{notice: "已全部暂停", isGlobalOp: true}
		}
	}
	m.opLabel = "正在全部恢复"
	return func() tea.Msg {
		if err := mgr.ResumeAll(); err != nil {
			return batchOpResultMsg{err: err, isGlobalOp: true}
		}
		return batchOpResultMsg{notice: "已全部恢复", isGlobalOp: true}
	}
}

func (m *model) retryHistoryCmd() tea.Cmd {
	t := m.selectedTask()
	if m.manager == nil || t == nil {
		return nil
	}
	spec := queue.Spec{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Size: t.Size}
	return m.prepareBatch(t.Direction, []queue.Spec{spec}, false)
}

func (m *model) pauseAllAndQuitCmd() tea.Cmd {
	mgr := m.manager
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := mgr.PauseAll(ctx); err != nil {
			return batchOpResultMsg{err: err, isGlobalOp: true}
		}
		return batchOpResultMsg{quit: true, isGlobalOp: true}
	}
}
