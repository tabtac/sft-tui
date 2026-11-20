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

// formState 封装服务端模态框/向导的多控件表单状态与字段索引映射。
type formState struct {
	formRows       []formRow
	formInputs     []textinput.Model
	formUnits      []*formUnitField
	formSelects    []formSelectField
	formFocus      int
	formSubFocus   int
	formScroll     int
	formToggles    []bool
	formButtons    []formButton
	formBtnIdx     int
	dropdownOpen   bool
	dropdownRow    int
	dropdownCursor int
	formKeyIdx     map[string]int
	formOnEsc      func()
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

func (f *formState) resetForm() {
	f.formRows = nil
	f.formInputs = nil
	f.formUnits = nil
	f.formSelects = nil
	f.formToggles = nil
	f.formButtons = nil
	f.formBtnIdx = 0
	f.formFocus = 0
	f.formSubFocus = 0
	f.formScroll = 0
	f.dropdownOpen = false
	f.dropdownRow = -1
	f.dropdownCursor = 0
	f.formKeyIdx = make(map[string]int)
	f.formOnEsc = nil
}

func makeModalInput(val string) textinput.Model {
	ti := textinput.New()
	ti.CharLimit = 512
	ti.Width = 42
	ti.SetValue(val)
	return ti
}

func (f *formState) setInputKey(inputIdx int, key string) {
	if key == "" {
		return
	}
	if f.formKeyIdx == nil {
		f.formKeyIdx = make(map[string]int)
	}
	f.formKeyIdx[key] = inputIdx
}

func (f *formState) inputByKey(key string) (int, bool) {
	idx, ok := f.formKeyIdx[key]
	return idx, ok
}

func (f *formState) addTextRow(label, value string) int {
	idx := len(f.formInputs)
	f.formInputs = append(f.formInputs, makeModalInput(value))
	f.formUnits = append(f.formUnits, nil)
	f.formRows = append(f.formRows, formRow{
		kind:     formRowText,
		label:    label,
		inputIdx: idx,
	})
	return idx
}

func (f *formState) addKeyedTextRow(key, label, value string) int {
	idx := f.addTextRow(label, value)
	f.setInputKey(idx, key)
	return idx
}

func (f *formState) addSizeUnitRow(label string, bytesVal uint64, isRate bool) int {
	numStr, opts, sel := decomposeBytes(bytesVal, isRate)
	idx := len(f.formInputs)
	ti := makeModalInput(numStr)
	ti.Width = 18
	f.formInputs = append(f.formInputs, ti)
	f.formUnits = append(f.formUnits, &formUnitField{options: opts, selected: sel})
	f.formRows = append(f.formRows, formRow{
		kind:     formRowSizeUnit,
		label:    label,
		inputIdx: idx,
	})
	return idx
}

func (f *formState) addKeyedSizeUnitRow(key, label string, bytesVal uint64, isRate bool) int {
	idx := f.addSizeUnitRow(label, bytesVal, isRate)
	f.setInputKey(idx, key)
	return idx
}

func (f *formState) addSelectRow(label string, options []string, selected int, onChange func(string, int)) int {
	if selected < 0 || selected >= len(options) {
		selected = 0
	}
	idx := len(f.formSelects)
	f.formSelects = append(f.formSelects, formSelectField{
		options:  options,
		selected: selected,
		onChange: onChange,
	})
	f.formRows = append(f.formRows, formRow{
		kind:      formRowSelect,
		label:     label,
		selectIdx: idx,
	})
	return idx
}

func (f *formState) addToggleRow(label string, checked bool, onToggle func(bool)) int {
	idx := len(f.formToggles)
	f.formToggles = append(f.formToggles, checked)
	f.formRows = append(f.formRows, formRow{
		kind:      formRowToggle,
		label:     label,
		toggleIdx: idx,
		onToggle:  onToggle,
	})
	return idx
}

func (f *formState) addNoteRow(lines []string, accent bool) {
	cleaned := make([]string, len(lines))
	for i, ln := range lines {
		cleaned[i] = sanitizeInline(ln)
	}
	f.formRows = append(f.formRows, formRow{
		kind:       formRowNote,
		lines:      cleaned,
		noteAccent: accent,
	})
}

func (f *formState) parseSizeInput(inputIdx int, errLabel string) (uint64, error) {
	num := strings.TrimSpace(f.formInputs[inputIdx].Value())
	if num == "" {
		return 0, fmt.Errorf("%s不能为空", errLabel)
	}
	u := f.formUnits[inputIdx]
	if u != nil && u.selected >= 0 && u.selected < len(u.options) {
		num += " " + u.options[u.selected]
	}
	v, err := setup.ParseBytes(num)
	if err != nil {
		return 0, fmt.Errorf("%s：%w", errLabel, err)
	}
	return v, nil
}

func (f *formState) hasFocusableInputRow() bool {
	for _, r := range f.formRows {
		if r.kind != formRowNote {
			return true
		}
	}
	return false
}

func (f *formState) stepFocusableRow(from, dir int) (int, bool) {
	n := len(f.formRows)
	hasBtns := len(f.formButtons) > 0
	maxSlot := n - 1
	if hasBtns {
		maxSlot = n
	}
	if maxSlot < 0 || (dir != -1 && dir != 1) {
		return from, false
	}
	for cur := from + dir; cur >= 0 && cur <= maxSlot; cur += dir {
		if cur == n && hasBtns {
			return cur, true
		}
		if cur >= 0 && cur < n && f.formRows[cur].kind != formRowNote {
			return cur, true
		}
	}
	return from, false
}

func (f *formState) nextFocusableRow(from, dir int) int {
	n := len(f.formRows)
	hasBtns := len(f.formButtons) > 0
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
		if cur >= 0 && cur < n && f.formRows[cur].kind != formRowNote {
			return cur
		}
	}
	return 0
}

func (f *formState) currentDropdownOptions() []string {
	if f.dropdownRow < 0 || f.dropdownRow >= len(f.formRows) {
		return nil
	}
	r := f.formRows[f.dropdownRow]
	if r.kind == formRowSelect {
		return f.formSelects[r.selectIdx].options
	}
	if r.kind == formRowSizeUnit && f.formUnits[r.inputIdx] != nil {
		return f.formUnits[r.inputIdx].options
	}
	return nil
}

func (f *formState) stepUnitOrSelect(rowIdx, delta int) bool {
	if rowIdx < 0 || rowIdx >= len(f.formRows) {
		return false
	}
	r := f.formRows[rowIdx]
	switch r.kind {
	case formRowSelect:
		sf := &f.formSelects[r.selectIdx]
		if len(sf.options) == 0 {
			return false
		}
		sf.selected = (sf.selected + delta + len(sf.options)) % len(sf.options)
		if sf.onChange != nil {
			sf.onChange(sf.options[sf.selected], sf.selected)
		}
		return true
	case formRowSizeUnit:
		if f.formSubFocus != 1 {
			return false
		}
		if u := f.formUnits[r.inputIdx]; u != nil && len(u.options) > 0 {
			u.selected = (u.selected + delta + len(u.options)) % len(u.options)
			return true
		}
	}
	return false
}

func (f *formState) activateFormButton(idx int) (tea.Model, tea.Cmd) {
	if idx >= 0 && idx < len(f.formButtons) && f.formButtons[idx].onClick != nil {
		return f.formButtons[idx].onClick()
	}
	return nil, nil
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

func (m *serverModel) formLayoutParams() (innerW, rowBudget, popupH int) {
	_, innerW = modalBoxDimensions(m.width)
	noticeLines := m.modalFormNoticeLines(innerW)
	btnBlock, _ := m.layoutFormButtons(innerW, 0)
	var maxPopupH int
	rowBudget, maxPopupH = m.formRowBudget(innerW, len(noticeLines), len(btnBlock))
	opts := m.currentDropdownOptions()
	if m.dropdownOpen {
		popupH = min(len(opts), maxPopupH)
	}
	return innerW, rowBudget, popupH
}

func (m *serverModel) ensureFormRowVisible() {
	if len(m.formRows) == 0 {
		m.formScroll = 0
		return
	}
	innerW, rowBudget, popupH := m.formLayoutParams()
	start, _ := m.solveFormVisibleRange(innerW, rowBudget, popupH)
	m.formScroll = start
}

func (m *serverModel) scrollFormBy(delta int) {
	n := len(m.formRows)
	if n == 0 {
		m.formScroll = 0
		return
	}
	innerW, rowBudget, popupH := m.formLayoutParams()
	maxStart := n - 1
	for maxStart > 0 && m.formFitEnd(maxStart-1, innerW, rowBudget, popupH) == n {
		maxStart--
	}
	m.formScroll = max(0, min(maxStart, m.formScroll+delta))
}

func (m *serverModel) focusInputRow(inputIdx int) tea.Cmd {
	for i, r := range m.formRows {
		if (r.kind == formRowText || r.kind == formRowSizeUnit) && r.inputIdx == inputIdx {
			m.setRowFocus(i, 0)
			return textinput.Blink
		}
	}
	return nil
}

func (m *serverModel) focusSelectRow(selectIdx int) tea.Cmd {
	for i, r := range m.formRows {
		if r.kind == formRowSelect && r.selectIdx == selectIdx {
			m.setRowFocus(i, 0)
			return nil
		}
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
	m.ensureFormRowVisible()
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
	m.ensureFormRowVisible()
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
			m.ensureFormRowVisible()
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
		if !m.hasFocusableInputRow() {
			m.scrollFormBy(1)
			return m, nil
		}
		if next, ok := m.stepFocusableRow(m.formFocus, 1); ok {
			m.setRowFocus(next, 0)
			return m, textinput.Blink
		}
		m.scrollFormBy(1)
		return m, nil

	case "up":
		if !m.hasFocusableInputRow() {
			m.scrollFormBy(-1)
			return m, nil
		}
		if prev, ok := m.stepFocusableRow(m.formFocus, -1); ok {
			m.setRowFocus(prev, 0)
			return m, textinput.Blink
		}
		m.scrollFormBy(-1)
		return m, nil

	case "pgup":
		if !m.hasFocusableInputRow() {
			m.scrollFormBy(-5)
			return m, nil
		}
		cur := m.formFocus
		for i := 0; i < 5; i++ {
			prev, ok := m.stepFocusableRow(cur, -1)
			if !ok {
				break
			}
			cur = prev
		}
		if cur != m.formFocus {
			m.setRowFocus(cur, 0)
			return m, textinput.Blink
		}
		m.scrollFormBy(-5)
		return m, nil

	case "pgdown":
		if !m.hasFocusableInputRow() {
			m.scrollFormBy(5)
			return m, nil
		}
		cur := m.formFocus
		for i := 0; i < 5; i++ {
			next, ok := m.stepFocusableRow(cur, 1)
			if !ok {
				break
			}
			cur = next
		}
		if cur != m.formFocus {
			m.setRowFocus(cur, 0)
			return m, textinput.Blink
		}
		m.scrollFormBy(5)
		return m, nil

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
		return m.triggerPrimaryButton()

	case "j", "k":
		// 焦点不在文本输入态时允许 j/k 上下切项，提升键盘操作一致性。
		inTextInput := false
		if m.formFocus >= 0 && m.formFocus < nRows {
			r := m.formRows[m.formFocus]
			inTextInput = (r.kind == formRowText || r.kind == formRowSizeUnit) && m.formSubFocus == 0
		}
		if !inTextInput {
			dir := 1
			if k == "k" {
				dir = -1
			}
			if !m.hasFocusableInputRow() {
				m.scrollFormBy(dir)
				return m, nil
			}
			if next, ok := m.stepFocusableRow(m.formFocus, dir); ok {
				m.setRowFocus(next, 0)
				return m, textinput.Blink
			}
			m.scrollFormBy(dir)
			return m, nil
		}
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
	x0, x1 int // 相对盒内左边界的 X 范围
	kind   formHitKind
	row    int
	idx    int
}

type dropdownLine struct {
	text   string
	optIdx int
}

func (m *serverModel) formMaxTotalLines() int {
	if m.height <= 0 {
		return 1 << 20
	}
	// renderModalRawBox(width, th, title, lines, footer) 固定占用 8 行边框/内边距/标题/分隔线/页脚。
	return max(3, m.height-8)
}

func (m *serverModel) formRowBaseHeight(r formRow, innerW int) int {
	switch r.kind {
	case formRowNote:
		return max(1, len(wrapModalLines(r.lines, innerW)))
	case formRowToggle:
		return 1
	default:
		return 2
	}
}

func (m *serverModel) modalFormNoticeLines(innerW int) []string {
	toast := sanitizeInline(strings.TrimSpace(m.statusToast))
	if toast == "" {
		return nil
	}
	wrapped := wrapModalLines([]string{"! " + toast}, innerW)
	out := make([]string, 0, len(wrapped))
	for _, ln := range wrapped {
		out = append(out, m.th.badgeErr.Render(ln))
	}
	return out
}

func (m *serverModel) layoutFormButtons(innerW, baseY int) ([]string, []formHitBox) {
	if len(m.formButtons) == 0 {
		return nil, nil
	}
	var lines []string
	var hits []formHitBox
	// 按行自动换行放置按钮，避免窄终端折行导致高度和命中区偏移。
	lines = append(lines, "")
	btnLineFocused := m.formFocus == len(m.formRows)

	var curParts []string
	curX := 2
	curY := baseY + len(lines)
	flushLine := func() {
		lines = append(lines, "  "+strings.Join(curParts, "  "))
		curParts = nil
		curX = 2
		curY = baseY + len(lines)
	}

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
		if len(curParts) > 0 && curX+w > innerW {
			flushLine()
		}
		hits = append(hits, formHitBox{y0: curY, y1: curY + 1, x0: curX, x1: curX + w, kind: hitButton, idx: i})
		curParts = append(curParts, rendered)
		curX += w + 2
	}
	if len(curParts) > 0 {
		lines = append(lines, "  "+strings.Join(curParts, "  "))
	}
	return lines, hits
}

func (m *serverModel) formRowBudget(innerW, noticeH, btnH int) (rowBudget, maxPopupH int) {
	totalMax := m.formMaxTotalLines()
	fixed := noticeH + btnH
	if noticeH > 0 && btnH == 0 {
		fixed++
	}
	rowBudget = max(2, totalMax-fixed)
	baseH := 2
	indReserve := 0
	if len(m.formRows) > 1 {
		indReserve = 2
	}
	maxPopupH = max(1, rowBudget-baseH-indReserve)
	return rowBudget, maxPopupH
}

func (m *serverModel) formFitEnd(s, innerW, rowBudget, popupH int) int {
	n := len(m.formRows)
	used := 0
	e := s
	for e < n {
		h := m.formRowBaseHeight(m.formRows[e], innerW)
		if m.dropdownOpen && m.dropdownRow == e {
			h += popupH
		}
		ind := 0
		if s > 0 {
			ind++
		}
		if e+1 < n {
			ind++
		}
		if e > s && used+h+ind > rowBudget {
			break
		}
		used += h
		e++
	}
	return e
}

func (m *serverModel) solveFormVisibleRange(innerW, rowBudget, popupH int) (start, end int) {
	n := len(m.formRows)
	if n == 0 {
		return 0, 0
	}

	target := m.formFocus
	if m.dropdownOpen && m.dropdownRow >= 0 && m.dropdownRow < n {
		target = m.dropdownRow
	} else if target < 0 || target >= n {
		s := max(0, min(n-1, m.formScroll))
		return s, m.formFitEnd(s, innerW, rowBudget, popupH)
	}

	s := max(0, min(n-1, m.formScroll))
	if target < s {
		s = target
	}
	e := m.formFitEnd(s, innerW, rowBudget, popupH)
	for s < target && target >= e {
		s++
		e = m.formFitEnd(s, innerW, rowBudget, popupH)
	}
	return s, e
}

func (m *serverModel) renderDropdownPopup(opts []string, innerW, maxH int) []dropdownLine {
	if len(opts) == 0 || maxH <= 0 {
		return nil
	}
	maxW := 18
	for _, o := range opts {
		if w := lipgloss.Width(o); w > maxW {
			maxW = w
		}
	}
	maxW = min(max(12, maxW+4), max(12, innerW-6))

	vis := min(len(opts), maxH)
	start := 0
	if len(opts) > vis {
		cur := max(0, min(len(opts)-1, m.dropdownCursor))
		start = scrollOffset(cur, len(opts), vis)
	}
	end := min(len(opts), start+vis)

	out := make([]dropdownLine, 0, end-start)
	for idx := start; idx < end; idx++ {
		mark := "  "
		st := m.th.rowNormal
		if idx == m.dropdownCursor {
			mark = "▶ "
			st = m.th.rowCursor
		}
		text := padPlainRightCell(mark+truncPlainCell(opts[idx], max(4, maxW-2)), maxW)
		out = append(out, dropdownLine{
			text:   "    " + st.Render(text),
			optIdx: idx,
		})
	}
	return out
}

func (m *serverModel) buildFormViewAndHits(innerW int) ([]string, string, []formHitBox) {
	noticeLines := m.modalFormNoticeLines(innerW)
	preBtnLines, _ := m.layoutFormButtons(innerW, 0)
	rowBudget, maxPopupH := m.formRowBudget(innerW, len(noticeLines), len(preBtnLines))

	var popup []dropdownLine
	if m.dropdownOpen && m.dropdownRow >= 0 && m.dropdownRow < len(m.formRows) {
		popup = m.renderDropdownPopup(m.currentDropdownOptions(), innerW, maxPopupH)
	}
	start, end := m.solveFormVisibleRange(innerW, rowBudget, len(popup))

	var lines []string
	var hits []formHitBox
	if start > 0 {
		ind := truncCell(fmt.Sprintf("▲ 上方还有 %d 项（方向键/滚轮浏览）", start), innerW)
		lines = append(lines, m.th.badgeMuted.Render(ind))
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
			for _, ln := range wrapModalLines(r.lines, innerW) {
				if r.noteAccent {
					lines = append(lines, m.th.toast.Render(ln))
				} else {
					lines = append(lines, m.th.badgeMuted.Render(ln))
				}
			}

		case formRowText:
			lines = append(lines, prefix+m.th.hintKey.Render(truncCell(r.label, max(8, innerW-2))))
			ti := m.formInputs[r.inputIdx]
			ti.Width = min(ti.Width, max(8, innerW-6))
			lines = append(lines, "  "+ti.View())
			hits = append(hits, formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitFormRow, row: i})

		case formRowSizeUnit:
			numFocused := focused && m.formSubFocus == 0
			unitFocused := focused && m.formSubFocus == 1
			lblMark := prefix
			if unitFocused {
				lblMark = "▸ "
			}
			lines = append(lines, lblMark+m.th.hintKey.Render(truncCell(r.label, max(8, innerW-2))))
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
			unitW := lipgloss.Width(unitView)
			ti := m.formInputs[r.inputIdx]
			ti.Width = min(ti.Width, max(6, innerW-unitW-8))
			inView := ti.View()
			inW := lipgloss.Width(inView)
			rowLine := "  " + inView + "  " + unitView
			lineY := len(lines)
			lines = append(lines, rowLine)
			hits = append(hits,
				formHitBox{y0: lineY, y1: lineY + 1, x0: 4 + inW, x1: 4 + inW + unitW + 1, kind: hitUnitBox, row: i},
				formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitFormRow, row: i},
			)
			if m.dropdownOpen && m.dropdownRow == i && unit != nil {
				for _, pl := range popup {
					hits = append(hits, formHitBox{y0: len(lines), y1: len(lines) + 1, x0: 0, x1: innerW, kind: hitPopupOpt, row: i, idx: pl.optIdx})
					lines = append(lines, pl.text)
				}
			}

		case formRowSelect:
			sf := m.formSelects[r.selectIdx]
			lines = append(lines, prefix+m.th.hintKey.Render(truncPlainCell(r.label, max(8, innerW-2))))
			cur := ""
			if sf.selected >= 0 && sf.selected < len(sf.options) {
				cur = sf.options[sf.selected]
			}
			box := fmt.Sprintf("[ %s ▼ ]", truncPlainCell(cur, max(8, innerW-12)))
			if focused {
				lines = append(lines, "  "+m.th.tabActive.Render(box))
			} else {
				lines = append(lines, "  "+m.th.badgeID.Render(box))
			}
			hits = append(hits, formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitUnitBox, row: i})
			if m.dropdownOpen && m.dropdownRow == i {
				for _, pl := range popup {
					hits = append(hits, formHitBox{y0: len(lines), y1: len(lines) + 1, x0: 0, x1: innerW, kind: hitPopupOpt, row: i, idx: pl.optIdx})
					lines = append(lines, pl.text)
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
			rowStr := truncPlainCell(fmt.Sprintf("%s%s %s", prefix, box, r.label), innerW)
			lines = append(lines, st.Render(rowStr))
			hits = append(hits, formHitBox{y0: yStart, y1: len(lines), x0: 0, x1: innerW, kind: hitFormRow, row: i})
		}
	}
	if end < len(m.formRows) {
		ind := truncCell(fmt.Sprintf("▼ 下方还有 %d 项（方向键/滚轮浏览）", len(m.formRows)-end), innerW)
		lines = append(lines, m.th.badgeMuted.Render(ind))
	}
	if len(noticeLines) > 0 {
		if len(m.formButtons) == 0 {
			lines = append(lines, "")
		}
		lines = append(lines, noticeLines...)
	}
	if len(m.formButtons) > 0 {
		btnLines, btnHits := m.layoutFormButtons(innerW, len(lines))
		lines = append(lines, btnLines...)
		hits = append(hits, btnHits...)
	}

	footer := "Tab/↑↓ 切项 · Space/Enter 勾选或下拉 · ←→ 切单位 · Esc 返回"
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
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.modal == srvModalHelp || m.modal == srvModalDetail {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollServerInfoModal("up")
		case tea.MouseButtonWheelDown:
			m.scrollServerInfoModal("down")
		}
		return m, nil
	}
	if len(m.formRows) == 0 {
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelUp {
		if m.dropdownOpen {
			if m.dropdownCursor > 0 {
				m.dropdownCursor--
			}
			return m, nil
		}
		if !m.hasFocusableInputRow() {
			m.scrollFormBy(-1)
			return m, nil
		}
		if prev, ok := m.stepFocusableRow(m.formFocus, -1); ok {
			m.setRowFocus(prev, 0)
		} else {
			m.scrollFormBy(-1)
		}
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		if m.dropdownOpen {
			if m.dropdownCursor < len(m.currentDropdownOptions())-1 {
				m.dropdownCursor++
			}
			return m, nil
		}
		if !m.hasFocusableInputRow() {
			m.scrollFormBy(1)
			return m, nil
		}
		if next, ok := m.stepFocusableRow(m.formFocus, 1); ok {
			m.setRowFocus(next, 0)
		} else {
			m.scrollFormBy(1)
		}
		return m, nil
	}
	if msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	_, innerW := modalBoxDimensions(m.width)
	lines, footer, hits := m.buildFormViewAndHits(innerW)
	boxStr := renderModalRawBox(m.width, m.th, m.modalTitle(), lines, footer)
	actualBoxW := lipgloss.Width(boxStr)
	boxH := lipgloss.Height(boxStr)
	boxX := max(0, (m.width-actualBoxW)/2)
	boxY := max(0, (m.height-boxH)/2)
	// 内容行起始：border(1)+pad(1)+title(1)+rule(1) = 4；左起始 boxX+3 (border1+pad2)
	relY := msg.Y - (boxY + 4)
	relX := msg.X - (boxX + 3)

	// 优先命中下拉菜单项，其次单位框/按钮/普通行
	for _, h := range hits {
		if h.kind == hitPopupOpt && relY >= h.y0 && relY < h.y1 && relX >= h.x0 && relX < h.x1 {
			m.applyDropdownSelection(h.idx)
			return m, nil
		}
	}
	if m.dropdownOpen {
		m.dropdownOpen = false
		m.dropdownRow = -1
		m.ensureFormRowVisible()
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
		if h.kind == hitFormRow && relY >= h.y0 && relY < h.y1 && relX >= h.x0 && relX < h.x1 {
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

func settingLabel(key string) string {
	labels := map[string]string{
		"max_streams":                       "单连接最大并发流",
		"chunk_size":                        "分块传输上限",
		"window_size":                       "单流接收窗口",
		"handshake_seconds":                 "TLS 握手超时 (秒)",
		"hello_seconds":                     "HELLO 超时 (秒)",
		"frame_read_seconds":                "读帧超时 (秒)",
		"write_seconds":                     "写超时 (秒)",
		"idle_seconds":                      "会话空闲超时 (秒)",
		"max_connections":                   "全局最大连接数",
		"max_handshakes":                    "全局并发握手数",
		"max_unauthenticated_per_ip":        "每 IP 未认证连接上限",
		"max_connections_per_user":          "单用户最大连接数",
		"max_streams_per_user":              "单用户最大并发流",
		"max_pending_transfers_per_user":    "单用户待处理任务上限",
		"max_files_per_user":                "单用户最多文件数",
		"max_new_tasks_per_minute":          "每分钟新建任务上限",
		"max_upload_checkpoints_per_second": "每秒检查点写入上限",
		"transfer_retention_seconds":        "断点检查点保留时长 (秒)",
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
