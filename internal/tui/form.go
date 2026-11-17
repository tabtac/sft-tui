package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/config"
	"sft-tui/internal/setup"
)

var (
	rateUnits = []string{"B/s", "KB/s", "MB/s", "GB/s", "KiB/s", "MiB/s", "GiB/s"}
	byteUnits = []string{"B", "KB", "MB", "GB", "KiB", "MiB", "GiB"}
)

type formRowKind int

const (
	formRowText formRowKind = iota
	formRowSizeUnit
	formRowSelect
	formRowToggle
	formRowNote
)

type formSelectField struct {
	label    string
	options  []string
	selected int
	onChange func(val string, idx int)
}

type formUnitField struct {
	options  []string
	selected int
}

type formRow struct {
	kind       formRowKind
	label      string
	inputIdx   int
	selectIdx  int
	toggleIdx  int
	lines      []string
	onToggle   func(bool)
	noteAccent bool
}

type formButton struct {
	label   string
	primary bool
	onClick func() (tea.Model, tea.Cmd)
}

type deployWizardDraft struct {
	d          setup.Deployment
	scene      int // 0=本机演示, 1=局域网部署
	addresses  []string
	base       string
	step       int // 1..4 或 5(完成导出页)
	advanced   bool
	tuneLimits bool
	useGlobal  bool
	globalUp   uint64
	globalDown uint64
	globalBur  uint64
	addrBase   string
	saved      bool
	started    bool
	exportDir  string
	summaryErr string
	finishMsg  string
}

func decomposeBytes(val uint64, isRate bool) (string, []string, int) {
	opts := byteUnits
	if isRate {
		opts = rateUnits
	}
	if val == 0 {
		return "0", opts, 0
	}
	type unitScale struct {
		idx   int
		scale uint64
	}
	// 按常见单位整除优先反显（KiB/MiB/GiB 与 KB/MB/GB 均保留准确无损表达）
	scales := []unitScale{
		{6, 1 << 30},
		{3, 1_000_000_000},
		{5, 1 << 20},
		{2, 1_000_000},
		{4, 1 << 10},
		{1, 1_000},
	}
	for _, s := range scales {
		if val%s.scale == 0 {
			return strconv.FormatUint(val/s.scale, 10), opts, s.idx
		}
	}
	return strconv.FormatUint(val, 10), opts, 0
}

func (m *serverModel) resetForm() {
	m.formRows = nil
	m.formInputs = nil
	m.formUnits = nil
	m.formSelects = nil
	m.formLabels = nil
	m.formToggles = nil
	m.formToggleLbl = nil
	m.formButtons = nil
	m.formBtnIdx = 0
	m.formFocus = 0
	m.formSubFocus = 0
	m.formScroll = 0
	m.dropdownOpen = false
	m.dropdownRow = -1
	m.dropdownCursor = 0
}

func makeModalInput(val string) textinput.Model {
	ti := textinput.New()
	ti.CharLimit = 512
	ti.Width = 42
	ti.SetValue(val)
	return ti
}

func (m *serverModel) addTextRow(label, value string) int {
	idx := len(m.formInputs)
	m.formInputs = append(m.formInputs, makeModalInput(value))
	m.formUnits = append(m.formUnits, nil)
	m.formLabels = append(m.formLabels, label)
	m.formRows = append(m.formRows, formRow{
		kind:     formRowText,
		label:    label,
		inputIdx: idx,
	})
	return idx
}

func (m *serverModel) addSizeUnitRow(label string, bytesVal uint64, isRate bool) int {
	numStr, opts, sel := decomposeBytes(bytesVal, isRate)
	idx := len(m.formInputs)
	ti := makeModalInput(numStr)
	ti.Width = 18
	m.formInputs = append(m.formInputs, ti)
	m.formUnits = append(m.formUnits, &formUnitField{options: opts, selected: sel})
	m.formLabels = append(m.formLabels, label)
	m.formRows = append(m.formRows, formRow{
		kind:     formRowSizeUnit,
		label:    label,
		inputIdx: idx,
	})
	return idx
}

func (m *serverModel) addSelectRow(label string, options []string, selected int, onChange func(string, int)) int {
	if selected < 0 || selected >= len(options) {
		selected = 0
	}
	idx := len(m.formSelects)
	m.formSelects = append(m.formSelects, formSelectField{
		label:    label,
		options:  options,
		selected: selected,
		onChange: onChange,
	})
	m.formRows = append(m.formRows, formRow{
		kind:      formRowSelect,
		label:     label,
		selectIdx: idx,
	})
	return idx
}

func (m *serverModel) addToggleRow(label string, checked bool, onToggle func(bool)) int {
	idx := len(m.formToggles)
	m.formToggles = append(m.formToggles, checked)
	m.formToggleLbl = append(m.formToggleLbl, label)
	m.formRows = append(m.formRows, formRow{
		kind:      formRowToggle,
		label:     label,
		toggleIdx: idx,
		onToggle:  onToggle,
	})
	return idx
}

func (m *serverModel) addNoteRow(lines []string, accent bool) {
	m.formRows = append(m.formRows, formRow{
		kind:       formRowNote,
		lines:      lines,
		noteAccent: accent,
	})
}

func (m *serverModel) finalizeFormFocus(focusFirstInput bool) {
	for i := range m.formInputs {
		m.formInputs[i].Blur()
	}
	m.formSubFocus = 0
	m.formScroll = 0
	first := m.nextFocusableRow(-1, 1)
	if !focusFirstInput && len(m.formButtons) > 0 {
		first = len(m.formRows)
	}
	m.setRowFocus(first, 0)
	// 默认把按钮索引指向主按钮，便于用户直接按 Enter 提交或浏览按钮
	for i, b := range m.formButtons {
		if b.primary {
			m.formBtnIdx = i
			break
		}
	}
}

func (m *serverModel) parseSizeInput(inputIdx int, errLabel string) (uint64, error) {
	num := strings.TrimSpace(m.formInputs[inputIdx].Value())
	if num == "" {
		return 0, fmt.Errorf("%s不能为空", errLabel)
	}
	u := m.formUnits[inputIdx]
	if u != nil && u.selected >= 0 && u.selected < len(u.options) {
		num += " " + u.options[u.selected]
	}
	v, err := setup.ParseBytes(num)
	if err != nil {
		return 0, fmt.Errorf("%s：%w", errLabel, err)
	}
	return v, nil
}

func (m *serverModel) nextFocusableRow(from, dir int) int {
	n := len(m.formRows)
	hasBtns := len(m.formButtons) > 0
	totalSlots := n
	if hasBtns {
		totalSlots = n + 1
	}
	if totalSlots == 0 {
		return 0
	}
	cur := from
	for step := 0; step < totalSlots; step++ {
		cur = (cur + dir + totalSlots) % totalSlots
		if cur == n && hasBtns {
			return cur
		}
		if cur >= 0 && cur < n && m.formRows[cur].kind != formRowNote {
			return cur
		}
	}
	return 0
}

func (m *serverModel) setRowFocus(rowIdx, sub int) {
	if m.formFocus >= 0 && m.formFocus < len(m.formRows) {
		r := m.formRows[m.formFocus]
		if (r.kind == formRowText || r.kind == formRowSizeUnit) && r.inputIdx >= 0 && r.inputIdx < len(m.formInputs) {
			m.formInputs[r.inputIdx].Blur()
		}
	}
	m.formFocus = rowIdx
	m.formSubFocus = sub
	m.dropdownOpen = false
	m.dropdownRow = -1
	if rowIdx >= 0 && rowIdx < len(m.formRows) {
		r := m.formRows[rowIdx]
		if (r.kind == formRowText || r.kind == formRowSizeUnit) && sub == 0 {
			m.formInputs[r.inputIdx].Focus()
			m.formInputs[r.inputIdx].CursorEnd()
		}
	}
	m.ensureFormRowVisible()
}

func (m *serverModel) ensureFormRowVisible() {
	if m.formFocus < 0 || m.formFocus >= len(m.formRows) {
		if len(m.formRows) == 0 {
			m.formScroll = 0
		}
		return
	}
	maxRows := m.formMaxVisibleRows()
	if maxRows <= 0 {
		return
	}
	if m.formFocus < m.formScroll {
		m.formScroll = m.formFocus
	} else {
		// 统计 m.formScroll..m.formFocus 的显示行数
		for m.formScroll < m.formFocus && m.formSliceLines(m.formScroll, m.formFocus) > maxRows {
			m.formScroll++
		}
	}
}

func (m *serverModel) formMaxVisibleRows() int {
	h := m.height
	if h <= 0 {
		h = 30
	}
	// 扣除边框(2)+内边距(2)+标题(1)+分隔线(2)+页脚(1)+按钮(2)+安全留白(2)
	return max(6, h-12)
}

func (m *serverModel) formRowHeight(r formRow) int {
	switch r.kind {
	case formRowNote:
		return max(1, len(r.lines))
	case formRowToggle:
		return 1
	default:
		return 2
	}
}

func (m *serverModel) formSliceLines(start, endInclusive int) int {
	lines := 0
	for i := start; i <= endInclusive && i < len(m.formRows); i++ {
		lines += m.formRowHeight(m.formRows[i])
	}
	return lines
}

func (m *serverModel) formVisibleRange() (start, end int) {
	n := len(m.formRows)
	if n == 0 {
		return 0, 0
	}
	start = max(0, min(n-1, m.formScroll))
	maxH := m.formMaxVisibleRows()
	used := 0
	end = start
	for end < n {
		h := m.formRowHeight(m.formRows[end])
		if end > start && used+h > maxH {
			break
		}
		used += h
		end++
	}
	return start, end
}

func (m *serverModel) currentDropdownOptions() []string {
	if m.dropdownRow < 0 || m.dropdownRow >= len(m.formRows) {
		return nil
	}
	r := m.formRows[m.dropdownRow]
	if r.kind == formRowSelect {
		return m.formSelects[r.selectIdx].options
	}
	if r.kind == formRowSizeUnit && m.formUnits[r.inputIdx] != nil {
		return m.formUnits[r.inputIdx].options
	}
	return nil
}

func (m *serverModel) openDropdownAt(rowIdx int) {
	if rowIdx < 0 || rowIdx >= len(m.formRows) {
		return
	}
	r := m.formRows[rowIdx]
	switch r.kind {
	case formRowSelect:
		m.dropdownRow = rowIdx
		m.dropdownCursor = m.formSelects[r.selectIdx].selected
		m.dropdownOpen = true
	case formRowSizeUnit:
		if u := m.formUnits[r.inputIdx]; u != nil {
			m.dropdownRow = rowIdx
			m.dropdownCursor = u.selected
			m.dropdownOpen = true
		}
	}
}

func (m *serverModel) applyDropdownSelection(idx int) {
	if !m.dropdownOpen || m.dropdownRow < 0 || m.dropdownRow >= len(m.formRows) {
		m.dropdownOpen = false
		return
	}
	r := m.formRows[m.dropdownRow]
	switch r.kind {
	case formRowSelect:
		sf := &m.formSelects[r.selectIdx]
		if idx >= 0 && idx < len(sf.options) {
			sf.selected = idx
			if sf.onChange != nil {
				sf.onChange(sf.options[idx], idx)
			}
		}
	case formRowSizeUnit:
		if u := m.formUnits[r.inputIdx]; u != nil && idx >= 0 && idx < len(u.options) {
			u.selected = idx
		}
	}
	m.dropdownOpen = false
	m.dropdownRow = -1
}

func (m *serverModel) stepUnitOrSelect(rowIdx, delta int) bool {
	if rowIdx < 0 || rowIdx >= len(m.formRows) {
		return false
	}
	r := m.formRows[rowIdx]
	switch r.kind {
	case formRowSelect:
		sf := &m.formSelects[r.selectIdx]
		if len(sf.options) == 0 {
			return false
		}
		sf.selected = (sf.selected + delta + len(sf.options)) % len(sf.options)
		if sf.onChange != nil {
			sf.onChange(sf.options[sf.selected], sf.selected)
		}
		return true
	case formRowSizeUnit:
		if m.formSubFocus != 1 {
			return false
		}
		if u := m.formUnits[r.inputIdx]; u != nil && len(u.options) > 0 {
			u.selected = (u.selected + delta + len(u.options)) % len(u.options)
			return true
		}
	}
	return false
}

func (m *serverModel) activateFormButton(idx int) (tea.Model, tea.Cmd) {
	if idx >= 0 && idx < len(m.formButtons) && m.formButtons[idx].onClick != nil {
		return m.formButtons[idx].onClick()
	}
	return m, nil
}

func (m *serverModel) triggerPrimaryButton() (tea.Model, tea.Cmd) {
	for i, b := range m.formButtons {
		if b.primary {
			return m.activateFormButton(i)
		}
	}
	if len(m.formButtons) > 0 {
		return m.activateFormButton(0)
	}
	return m.submitModalForm()
}

func (m *serverModel) handleFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// 1. 下拉菜单展开时优先拦截选择键
	if m.dropdownOpen {
		opts := m.currentDropdownOptions()
		switch k {
		case "esc":
			m.dropdownOpen = false
			m.dropdownRow = -1
		case "up", "k":
			if m.dropdownCursor > 0 {
				m.dropdownCursor--
			}
		case "down", "j":
			if m.dropdownCursor < len(opts)-1 {
				m.dropdownCursor++
			}
		case "enter", " ":
			m.applyDropdownSelection(m.dropdownCursor)
		}
		return m, nil
	}

	nRows := len(m.formRows)
	hasBtns := len(m.formButtons) > 0

	// 2. 在大小+单位行的横向前进与返回处理
	switch k {
	case "tab":
		if m.formFocus >= 0 && m.formFocus < nRows && m.formRows[m.formFocus].kind == formRowSizeUnit && m.formSubFocus == 0 {
			m.setRowFocus(m.formFocus, 1)
			return m, nil
		}
		if m.formFocus == nRows && hasBtns && m.formBtnIdx < len(m.formButtons)-1 {
			m.formBtnIdx++
			return m, nil
		}
		next := m.nextFocusableRow(m.formFocus, 1)
		m.setRowFocus(next, 0)
		return m, textinput.Blink

	case "shift+tab":
		if m.formFocus >= 0 && m.formFocus < nRows && m.formRows[m.formFocus].kind == formRowSizeUnit && m.formSubFocus == 1 {
			m.setRowFocus(m.formFocus, 0)
			return m, textinput.Blink
		}
		if m.formFocus == nRows && hasBtns && m.formBtnIdx > 0 {
			m.formBtnIdx--
			return m, nil
		}
		prev := m.nextFocusableRow(m.formFocus, -1)
		sub := 0
		if prev >= 0 && prev < nRows && m.formRows[prev].kind == formRowSizeUnit {
			sub = 1
		}
		m.setRowFocus(prev, sub)
		return m, textinput.Blink

	case "down":
		if hasBtns && m.formFocus == nRows {
			return m, nil
		}
		next := m.nextFocusableRow(m.formFocus, 1)
		m.setRowFocus(next, 0)
		return m, textinput.Blink

	case "up":
		if m.formFocus == nRows && hasBtns {
			prev := m.nextFocusableRow(nRows, -1)
			m.setRowFocus(prev, 0)
			return m, textinput.Blink
		}
		prev := m.nextFocusableRow(m.formFocus, -1)
		m.setRowFocus(prev, 0)
		return m, textinput.Blink

	case "left":
		if m.formFocus == nRows && hasBtns {
			m.formBtnIdx = (m.formBtnIdx + len(m.formButtons) - 1) % len(m.formButtons)
			return m, nil
		}
		if m.stepUnitOrSelect(m.formFocus, -1) {
			return m, nil
		}

	case "right":
		if m.formFocus == nRows && hasBtns {
			m.formBtnIdx = (m.formBtnIdx + 1) % len(m.formButtons)
			return m, nil
		}
		if m.stepUnitOrSelect(m.formFocus, 1) {
			return m, nil
		}

	case "alt+down":
		if m.formFocus >= 0 && m.formFocus < nRows {
			m.openDropdownAt(m.formFocus)
			return m, nil
		}

	case " ":
		if m.formFocus == nRows && hasBtns {
			return m.activateFormButton(m.formBtnIdx)
		}
		if m.formFocus >= 0 && m.formFocus < nRows {
			r := m.formRows[m.formFocus]
			switch r.kind {
			case formRowToggle:
				val := !m.formToggles[r.toggleIdx]
				m.formToggles[r.toggleIdx] = val
				if r.onToggle != nil {
					r.onToggle(val)
				}
				return m, nil
			case formRowSelect:
				m.openDropdownAt(m.formFocus)
				return m, nil
			case formRowSizeUnit:
				if m.formSubFocus == 1 {
					m.openDropdownAt(m.formFocus)
					return m, nil
				}
			}
		}

	case "enter":
		if m.formFocus == nRows && hasBtns {
			return m.activateFormButton(m.formBtnIdx)
		}
		if m.formFocus >= 0 && m.formFocus < nRows {
			r := m.formRows[m.formFocus]
			if r.kind == formRowSelect || (r.kind == formRowSizeUnit && m.formSubFocus == 1) {
				m.openDropdownAt(m.formFocus)
				return m, nil
			}
		}
		return m.triggerPrimaryButton()
	}

	if m.formFocus >= 0 && m.formFocus < nRows {
		r := m.formRows[m.formFocus]
		if (r.kind == formRowText || r.kind == formRowSizeUnit) && m.formSubFocus == 0 {
			var cmd tea.Cmd
			m.formInputs[r.inputIdx], cmd = m.formInputs[r.inputIdx].Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

type formHitKind int

const (
	hitFormRow formHitKind = iota
	hitUnitBox
	hitPopupOpt
	hitButton
)

type formHitBox struct {
	y0, y1 int // 相对表单起始行的 Y 范围 [y0, y1)
	x0, x1 int // 相对盒内左边界的 X 范围（仅弹窗按钮或单位下拉使用）
	kind   formHitKind
	row    int
	idx    int
}

func (m *serverModel) buildFormViewAndHits(innerW int) ([]string, string, []formHitBox) {
	var lines []string
	var hits []formHitBox
	start, end := m.formVisibleRange()
	if start > 0 {
		lines = append(lines, m.th.badgeMuted.Render(fmt.Sprintf("▲ 上方还有 %d 项（方向键/滚轮浏览）", start)))
	}
	for i := start; i < end; i++ {
		r := m.formRows[i]
		focused := i == m.formFocus
		prefix := "  "
		if focused {
			prefix = "▶ "
		}
		yStart := len(lines)
		switch r.kind {
		case formRowNote:
			for _, ln := range r.lines {
				if r.noteAccent {
					lines = append(lines, m.th.toast.Render(ln))
				} else {
					lines = append(lines, m.th.badgeMuted.Render(ln))
				}
			}

		case formRowText:
			lines = append(lines, prefix+m.th.hintKey.Render(r.label))
			lines = append(lines, "  "+m.formInputs[r.inputIdx].View())
			hits = append(hits, formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitFormRow, row: i})

		case formRowSizeUnit:
			numFocused := focused && m.formSubFocus == 0
			unitFocused := focused && m.formSubFocus == 1
			lblMark := prefix
			if unitFocused {
				lblMark = "▸ "
			}
			lines = append(lines, lblMark+m.th.hintKey.Render(r.label))
			unit := m.formUnits[r.inputIdx]
			unitText := ""
			if unit != nil && unit.selected >= 0 && unit.selected < len(unit.options) {
				unitText = fmt.Sprintf("[ %s ▼ ]", unit.options[unit.selected])
			}
			unitView := m.th.badgeID.Render(unitText)
			if unitFocused {
				unitView = m.th.tabActive.Render(unitText)
			} else if numFocused {
				unitView = m.th.rowNormal.Render(unitText)
			}
			inView := m.formInputs[r.inputIdx].View()
			inW := lipgloss.Width(inView)
			unitW := lipgloss.Width(unitView)
			rowLine := "  " + inView + "  " + unitView
			lineY := len(lines)
			lines = append(lines, rowLine)
			hits = append(hits,
				formHitBox{y0: lineY, y1: lineY + 1, x0: 4 + inW, x1: 4 + inW + unitW + 1, kind: hitUnitBox, row: i},
				formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitFormRow, row: i},
			)
			if m.dropdownOpen && m.dropdownRow == i && unit != nil {
				pLines := m.renderDropdownPopup(unit.options)
				for idx, pl := range pLines {
					hits = append(hits, formHitBox{y0: len(lines), y1: len(lines) + 1, x0: 0, x1: innerW, kind: hitPopupOpt, row: i, idx: idx})
					lines = append(lines, pl)
				}
			}

		case formRowSelect:
			sf := m.formSelects[r.selectIdx]
			lines = append(lines, prefix+m.th.hintKey.Render(r.label))
			cur := ""
			if sf.selected >= 0 && sf.selected < len(sf.options) {
				cur = sf.options[sf.selected]
			}
			box := fmt.Sprintf("[ %s ▼ ]", truncCell(cur, max(12, innerW-12)))
			if focused {
				lines = append(lines, "  "+m.th.tabActive.Render(box))
			} else {
				lines = append(lines, "  "+m.th.badgeID.Render(box))
			}
			hits = append(hits, formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitUnitBox, row: i})
			if m.dropdownOpen && m.dropdownRow == i {
				pLines := m.renderDropdownPopup(sf.options)
				for idx, pl := range pLines {
					hits = append(hits, formHitBox{y0: len(lines), y1: len(lines) + 1, x0: 0, x1: innerW, kind: hitPopupOpt, row: i, idx: idx})
					lines = append(lines, pl)
				}
			}

		case formRowToggle:
			box := "[ ]"
			if m.formToggles[r.toggleIdx] {
				box = "[✓]"
			}
			st := m.th.rowNormal
			if focused {
				st = m.th.rowCursor
			}
			lines = append(lines, st.Render(fmt.Sprintf("%s%s %s", prefix, box, r.label)))
			hits = append(hits, formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitFormRow, row: i})
		}
	}
	if end < len(m.formRows) {
		lines = append(lines, m.th.badgeMuted.Render(fmt.Sprintf("▼ 下方还有 %d 项（方向键/滚轮浏览）", len(m.formRows)-end)))
	}
	if len(m.formButtons) > 0 {
		var btns []string
		btnLineFocused := m.formFocus == len(m.formRows)
		xCursor := 2
		btnY := len(lines) + 1
		for i, b := range m.formButtons {
			lbl := " " + b.label + " "
			var rendered string
			switch {
			case btnLineFocused && i == m.formBtnIdx:
				rendered = m.th.tabActive.Render("▸" + lbl + "◂")
			case b.primary:
				rendered = m.th.badgeID.Render("[" + lbl + "]")
			default:
				rendered = m.th.tabInactive.Render("[" + lbl + "]")
			}
			w := lipgloss.Width(rendered)
			hits = append(hits, formHitBox{y0: btnY, y1: btnY + 1, x0: xCursor, x1: xCursor + w, kind: hitButton, idx: i})
			xCursor += w + 2
			btns = append(btns, rendered)
		}
		lines = append(lines, "", "  "+strings.Join(btns, "  "))
	}

	footer := "Tab/↑↓ 切项 · Space 勾选/下拉 · ←→ 切换单位 · Enter 确认 · Esc 返回"
	if m.dropdownOpen {
		footer = "下拉选择：↑↓ 选择项 · Enter / Space 确认 · Esc 收起"
	}
	return lines, footer, hits
}

func (m *serverModel) renderFormLines(innerW int) ([]string, string) {
	lines, footer, _ := m.buildFormViewAndHits(innerW)
	return lines, footer
}

func (m *serverModel) handleModalMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || len(m.formRows) == 0 {
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelUp {
		if m.dropdownOpen {
			if m.dropdownCursor > 0 {
				m.dropdownCursor--
			}
			return m, nil
		}
		prev := m.nextFocusableRow(m.formFocus, -1)
		m.setRowFocus(prev, 0)
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		if m.dropdownOpen {
			if m.dropdownCursor < len(m.currentDropdownOptions())-1 {
				m.dropdownCursor++
			}
			return m, nil
		}
		next := m.nextFocusableRow(m.formFocus, 1)
		m.setRowFocus(next, 0)
		return m, nil
	}
	if msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	boxW := min(max(44, m.width-4), 86)
	innerW := boxW - 6
	lines, footer, hits := m.buildFormViewAndHits(innerW)
	boxStr := renderModalRawBox(m.width, m.th, m.modalTitle(), lines, footer)
	boxH := lipgloss.Height(boxStr)
	boxX := max(0, (m.width-boxW)/2)
	boxY := max(0, (m.height-boxH)/2)
	// 内容行起始：border(1)+pad(1)+title(1)+rule(1) = 4；左起始 boxX+3 (border1+pad2)
	relY := msg.Y - (boxY + 4)
	relX := msg.X - (boxX + 3)

	// 优先命中下拉菜单项，其次单位框/按钮/普通行
	for _, h := range hits {
		if h.kind == hitPopupOpt && relY >= h.y0 && relY < h.y1 {
			m.applyDropdownSelection(h.idx)
			return m, nil
		}
	}
	if m.dropdownOpen {
		m.dropdownOpen = false
		m.dropdownRow = -1
		return m, nil
	}
	for _, h := range hits {
		if relY >= h.y0 && relY < h.y1 && relX >= h.x0 && relX < h.x1 {
			switch h.kind {
			case hitButton:
				m.formFocus = len(m.formRows)
				m.formBtnIdx = h.idx
				return m.activateFormButton(h.idx)
			case hitUnitBox:
				sub := 0
				if m.formRows[h.row].kind == formRowSizeUnit {
					sub = 1
				}
				m.setRowFocus(h.row, sub)
				m.openDropdownAt(h.row)
				return m, nil
			}
		}
	}
	for _, h := range hits {
		if h.kind == hitFormRow && relY >= h.y0 && relY < h.y1 {
			r := m.formRows[h.row]
			m.setRowFocus(h.row, 0)
			if r.kind == formRowToggle {
				val := !m.formToggles[r.toggleIdx]
				m.formToggles[r.toggleIdx] = val
				if r.onToggle != nil {
					r.onToggle(val)
				}
			}
			return m, textinput.Blink
		}
	}
	return m, nil
}

func (m *serverModel) renderDropdownPopup(opts []string) []string {
	var out []string
	maxW := 18
	for _, o := range opts {
		if w := lipgloss.Width(o); w > maxW {
			maxW = w
		}
	}
	maxW = min(maxW+4, 48)
	for idx, o := range opts {
		mark := "  "
		st := m.th.rowNormal
		if idx == m.dropdownCursor {
			mark = "▶ "
			st = m.th.rowCursor
		}
		out = append(out, "    "+st.Render(padRightCell(mark+o, maxW)))
	}
	return out
}

func settingLabel(key string) string {
	labels := map[string]string{
		"max_streams":                       "单连接最大并发流 (limits.max_streams)",
		"chunk_size":                        "分块传输上限 (limits.chunk_size)",
		"window_size":                       "单流接收窗口 (limits.window_size)",
		"handshake_seconds":                 "TLS 握手超时/秒 (timeouts.handshake_seconds)",
		"hello_seconds":                     "HELLO 超时/秒 (timeouts.hello_seconds)",
		"frame_read_seconds":                "读帧超时/秒 (timeouts.frame_read_seconds)",
		"write_seconds":                     "写超时/秒 (timeouts.write_seconds)",
		"idle_seconds":                      "会话空闲超时/秒 (timeouts.idle_seconds)",
		"max_connections":                   "全局最大连接数 (resources.max_connections)",
		"max_handshakes":                    "全局并发握手数 (resources.max_handshakes)",
		"max_unauthenticated_per_ip":        "每 IP 未认证连接上限 (max_unauthenticated_per_ip)",
		"max_connections_per_user":          "单用户最大连接数 (max_connections_per_user)",
		"max_streams_per_user":              "单用户最大并发流 (max_streams_per_user)",
		"max_pending_transfers_per_user":    "单用户待处理任务上限 (max_pending_transfers_per_user)",
		"max_files_per_user":                "单用户最多文件数 (max_files_per_user)",
		"max_new_tasks_per_minute":          "每分钟新建任务上限 (max_new_tasks_per_minute)",
		"max_upload_checkpoints_per_second": "每秒检查点写入上限 (max_upload_checkpoints_per_second)",
		"transfer_retention_seconds":        "断点检查点保留时长/秒 (transfer_retention_seconds)",
	}
	if s := labels[key]; s != "" {
		return s
	}
	return key
}

func configAdvancedFields(c *config.Server) []config.NumericField {
	var fields []config.NumericField
	for _, f := range config.ServerNumericFields(c) {
		group, key, _ := strings.Cut(f.Path, ".")
		if group == "bandwidth" || group == "logging" || (group == "resources" && (key == "max_file_bytes" || key == "user_storage_bytes")) {
			continue
		}
		fields = append(fields, f)
	}
	return fields
}
