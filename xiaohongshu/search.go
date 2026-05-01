package xiaohongshu

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

type SearchResult struct {
	Search struct {
		Feeds FeedsValue `json:"feeds"`
	} `json:"search"`
}

// FilterOption 筛选选项结构体
type FilterOption struct {
	SortBy      string `json:"sort_by,omitempty" jsonschema:"排序依据: 综合|最新|最多点赞|最多评论|最多收藏,默认为'综合'"`
	NoteType    string `json:"note_type,omitempty" jsonschema:"笔记类型: 不限|视频|图文,默认为'不限'"`
	PublishTime string `json:"publish_time,omitempty" jsonschema:"发布时间: 不限|一天内|一周内|半年内,默认为'不限'"`
	SearchScope string `json:"search_scope,omitempty" jsonschema:"搜索范围: 不限|已看过|未看过|已关注,默认为'不限'"`
	Location    string `json:"location,omitempty" jsonschema:"位置距离: 不限|同城|附近,默认为'不限'"`
}

// internalFilterOption 内部使用的筛选选项（按文本定位，避免脆弱的 nth-child）
type internalFilterOption struct {
	GroupLabel string // 筛选组标签，如 "排序依据"
	OptionText string // 选项文本，如 "最多点赞"
}

// 筛选组标签 -> 合法选项集合。仅用于离线校验，不参与 DOM 查询索引。
var filterOptionsMap = map[string][]string{
	"排序依据": {"综合", "最新", "最多点赞", "最多评论", "最多收藏"},
	"笔记类型": {"不限", "视频", "图文"},
	"发布时间": {"不限", "一天内", "一周内", "半年内"},
	"搜索范围": {"不限", "已看过", "未看过", "已关注"},
	"位置距离": {"不限", "同城", "附近"},
}

// convertToInternalFilters 将 FilterOption 转换为内部的 internalFilterOption 列表
func convertToInternalFilters(filter FilterOption) ([]internalFilterOption, error) {
	pairs := []struct {
		groupLabel string
		text       string
	}{
		{"排序依据", filter.SortBy},
		{"笔记类型", filter.NoteType},
		{"发布时间", filter.PublishTime},
		{"搜索范围", filter.SearchScope},
		{"位置距离", filter.Location},
	}

	var out []internalFilterOption
	for _, p := range pairs {
		if p.text == "" {
			continue
		}
		if !isValidOption(p.groupLabel, p.text) {
			return nil, fmt.Errorf("筛选组 %q 中未找到文本 '%s'", p.groupLabel, p.text)
		}
		out = append(out, internalFilterOption{GroupLabel: p.groupLabel, OptionText: p.text})
	}
	return out, nil
}

func isValidOption(groupLabel, text string) bool {
	options, ok := filterOptionsMap[groupLabel]
	if !ok {
		return false
	}
	for _, opt := range options {
		if opt == text {
			return true
		}
	}
	return false
}

// validateInternalFilterOption 验证内部筛选选项是否有效
func validateInternalFilterOption(filter internalFilterOption) error {
	if filter.GroupLabel == "" || filter.OptionText == "" {
		return fmt.Errorf("筛选选项不能为空: %+v", filter)
	}
	if !isValidOption(filter.GroupLabel, filter.OptionText) {
		return fmt.Errorf("筛选组 %q 中不存在选项 %q", filter.GroupLabel, filter.OptionText)
	}
	return nil
}

type SearchAction struct {
	page *rod.Page
}

func NewSearchAction(page *rod.Page) *SearchAction {
	pp := page.Timeout(60 * time.Second)
	return &SearchAction{page: pp}
}

const (
	// 单步 selector 查找的短超时（找不到立即失败，避免无限等）。
	filterUITimeout = 5 * time.Second
	// 单次点击预算（包含 ScrollIntoView + WaitInteractable + Hover + Click）。
	// 比 filterUITimeout 长，给出足够时间让面板/选项渲染稳定。
	filterClickTimeout = 10 * time.Second
	// WaitInteractable 单独留 3s 预算；如果元素一直被遮挡 / 动画中，
	// 不要把整个 click 预算都耗在等"在最上层"上。
	filterClickWaitInteractable = 3 * time.Second
	// 一次具体动作（Click 或 Eval）的预算；和 WaitInteractable 分开，
	// 避免上一步把父 context 烧光。
	filterClickAction = 5 * time.Second
	// 面板打开 / hover 后等待动画 (fade-in/slide) 稳定的小延迟。
	// 没有它，rod 的 WaitInteractable "is on top" 检查会在动画期间误判。
	filterPanelSettleDelay = 400 * time.Millisecond
	// 整个筛选生效的总预算，issue 要求 15-30s 内必须给出结论。
	filterApplyTimeout = 25 * time.Second
	// 筛选生效轮询间隔。
	filterPollInterval = 250 * time.Millisecond
)

func (s *SearchAction) Search(ctx context.Context, keyword string, filters ...FilterOption) ([]Feed, error) {
	page := s.page.Context(ctx)

	searchURL := makeSearchURL(keyword)
	page.MustNavigate(searchURL)
	page.MustWaitStable()

	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	if len(filters) > 0 {
		// 转换并校验所有筛选选项
		var allInternalFilters []internalFilterOption
		for _, filter := range filters {
			internalFilters, err := convertToInternalFilters(filter)
			if err != nil {
				return nil, fmt.Errorf("筛选选项转换失败: %w", err)
			}
			allInternalFilters = append(allInternalFilters, internalFilters...)
		}
		for _, filter := range allInternalFilters {
			if err := validateInternalFilterOption(filter); err != nil {
				return nil, fmt.Errorf("筛选选项验证失败: %w", err)
			}
		}

		if len(allInternalFilters) > 0 {
			if err := s.applyFilters(ctx, allInternalFilters); err != nil {
				return nil, err
			}
		}
	}

	return extractFeedsFromPage(page)
}

// applyFilters 通过原生筛选 UI 应用筛选条件。
//
// 与原实现的差异：
//  1. selector 全部按文本定位，不再依赖 nth-child；
//  2. 不再使用无界的 MustWaitStable；
//  3. 每次点击前重新 Hover 筛选按钮，并重新查询面板/选项，避免面板收起或句柄过期；
//  4. 关键：discovery 用 5s 短超时，但 element 句柄重新绑回 page 的长 context，
//     避免 click / eval 共用了 discovery 已经耗尽的 5s 预算；
//  5. 点击完最后一个选项后进入条件竞争，命中以下任一条件就立即返回：
//     - feed 列表指纹变化
//     - 出现登录弹窗
//     - 出现安全验证 / 验证码
//     - 检测到空结果
//     - selector 找不到
//     - 点击不可交互 / 失败
//     - 达到 filterApplyTimeout 总预算
func (s *SearchAction) applyFilters(ctx context.Context, filters []internalFilterOption) error {
	page := s.page.Context(ctx)

	// 1. 点击前先快照当前 feed（fingerprint/url），后面 wait 阶段用它对比。
	//    ActiveFilters 这里都是空的，因为面板还没打开。
	initial := captureSearchSnapshot(page)

	// 2. 短超时找筛选按钮，但句柄绑回 page 的长 context
	filterBtn, err := findElementShort(page, `div.filter`, filterUITimeout)
	if err != nil {
		return fmt.Errorf("%w: 找不到筛选按钮 div.filter: %v", errors.ErrSelectorNotFound, err)
	}

	// 3. 打开筛选面板（hover 优先，hover 不出来时回退到 click）
	if err := openFilterPanel(page, filterBtn); err != nil {
		return err
	}

	// 4. 依次点击每个筛选选项；每次都重新 hover + 重新查 panel/row/tag，避免句柄过期
	anyClicked := false
	for _, f := range filters {
		if err := filterBtn.Hover(); err != nil {
			return fmt.Errorf("%w: 重新悬停筛选按钮失败: %v", errors.ErrSelectorNotFound, err)
		}
		panel, err := findElementShort(page, `div.filter-panel`, filterUITimeout)
		if err != nil {
			return fmt.Errorf("%w: 找不到筛选面板 div.filter-panel: %v", errors.ErrSelectorNotFound, err)
		}
		alreadyActive, err := clickFilterOption(page, panel, f.GroupLabel, f.OptionText)
		if err != nil {
			return err
		}
		if !alreadyActive {
			anyClicked = true
		}
	}

	// 5. 趁面板还开着，捕获一次"点击后"快照（仅做诊断用）。
	//    日志里 after_active 能直接看到 click 是否让目标选项变成 active —— 这是
	//    "click 触发 UI handler 没"的关键证据。
	afterClick := captureSearchSnapshot(page)
	logrus.WithFields(logrus.Fields{
		"any_clicked":     anyClicked,
		"initial_active":  initial.ActiveFilters,
		"after_active":    afterClick.ActiveFilters,
		"initial_url":     initial.URLSearch,
		"after_url":       afterClick.URLSearch,
		"after_fp_prefix": shortFp(afterClick.Fingerprint),
	}).Info("filter click loop done; snapshot captured before panel close")

	// 6. 把鼠标移开筛选区，让 hover 触发的面板收起，触发"提交"。
	// 部分 UI 选项是 hover-保持+click 模型，关掉面板才会真正 apply。
	closeFilterPanel(page)

	// 7. 全部选项本来就已选中（典型：sort_by=综合 是默认值）→ 不需要等
	//    也不需要报 timeout，直接当成功，让调用方拿到当前默认 feed。
	if !anyClicked {
		logrus.Info("all requested filters were already active; treating as no-op success")
		return nil
	}

	// 8. 条件竞争：等待筛选生效或快速失败。基线是点击前的 initial，
	//    任何 fingerprint / URL 变化都说明搜索 XHR 完成了。
	return waitForFilterApplied(ctx, page, initial)
}

// shortFp 把超长 fingerprint 截短给日志用，避免一行刷屏。
func shortFp(fp string) string {
	if len(fp) <= 60 {
		return fp
	}
	return fp[:60] + "...(truncated)"
}

// closeFilterPanel 把鼠标移到 (0,0)，关掉 hover 触发的筛选面板。
// 用于触发 "click 选项后还要关面板才提交" 的 UI。失败不算错。
func closeFilterPanel(page *rod.Page) {
	if err := page.Mouse.MoveTo(proto.Point{X: 0, Y: 0}); err != nil {
		logrus.WithError(err).Debug("move mouse to (0,0) failed; ignored")
	}
}

// findElementShort 用短超时做 selector 存在性检查，但把返回的 element 句柄
// 绑回 page 的长 context。这样后续的 Click / Eval / WaitInteractable 不会
// 因为 discovery 的短 context 已经耗尽就立刻 deadline exceeded。
func findElementShort(page *rod.Page, selector string, timeout time.Duration) (*rod.Element, error) {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return nil, err
	}
	return el.Context(page.GetContext()), nil
}

// openFilterPanel 把筛选面板打开。先尝试 hover（XHS 主流交互），不行就 click 兜底。
func openFilterPanel(page *rod.Page, filterBtn *rod.Element) error {
	if err := filterBtn.Hover(); err != nil {
		return fmt.Errorf("%w: 悬停筛选按钮失败: %v", errors.ErrSelectorNotFound, err)
	}
	if _, err := findElementShort(page, `div.filter-panel`, filterUITimeout); err == nil {
		// 面板出现后再等一小段动画时间，避免 fade-in 期间 WaitInteractable 误判
		time.Sleep(filterPanelSettleDelay)
		return nil
	}
	// hover 没出面板，尝试 click 兜底
	if err := filterBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("%w: 悬停未打开面板，点击筛选按钮也失败: %v", errors.ErrFilterClickFailed, err)
	}
	if _, err := findElementShort(page, `div.filter-panel`, filterUITimeout); err != nil {
		return fmt.Errorf("%w: 点击筛选按钮后仍找不到面板 div.filter-panel: %v", errors.ErrSelectorNotFound, err)
	}
	time.Sleep(filterPanelSettleDelay)
	return nil
}

// clickFilterOption 在筛选面板内按文本定位筛选行 + 选项并点击。
//
// 返回 alreadyActive=true 表示目标选项点击前就已经是 active/selected
// 状态（典型：sort_by=综合 是默认值）。这种情况下没点也不需要等待，
// 直接当成功。
func clickFilterOption(page *rod.Page, panel *rod.Element, groupLabel, optionText string) (alreadyActive bool, err error) {
	rows, err := panel.Elements(`div.filters`)
	if err != nil || len(rows) == 0 {
		return false, fmt.Errorf("%w: 筛选面板内没有筛选行 div.filters: %v", errors.ErrSelectorNotFound, err)
	}

	for _, row := range rows {
		text, _ := row.Text()
		if !strings.Contains(text, groupLabel) {
			continue
		}
		tags, err := row.Elements(`div.tags`)
		if err != nil || len(tags) == 0 {
			return false, fmt.Errorf("%w: 筛选组 %q 内没有选项 div.tags: %v", errors.ErrSelectorNotFound, groupLabel, err)
		}
		for _, tag := range tags {
			t, _ := tag.Text()
			if strings.TrimSpace(t) != optionText {
				continue
			}
			// 句柄重新绑回 page 的长 context，避免继承 discovery 的短超时
			tag = tag.Context(page.GetContext())

			// 点击前先看选项是不是已经选中。已经选中说明默认就是这个值，
			// 没必要点也没必要等（不然 sort_by=综合 永远 timeout）。
			beforeClass := elementClass(tag)
			if isClassActive(beforeClass) {
				logrus.WithFields(logrus.Fields{
					"group": groupLabel, "option": optionText, "class": beforeClass,
				}).Info("filter option already active; skipping click")
				return true, nil
			}

			if err := clickInteractable(tag, groupLabel, optionText); err != nil {
				return false, err
			}

			// 点击后立刻读 class，让 logs 能看到 click 是否触发了 UI handler。
			// 如果 after_active=true 但后面 feed 没变 → click 只触发了 UI 层，
			// 没触发搜索 XHR（很可能是 synthetic-event 被 isTrusted 检查滤掉）。
			// 如果 after_active=false → click 根本没到目标元素的 handler。
			afterClass := elementClass(tag)
			logrus.WithFields(logrus.Fields{
				"group":              groupLabel,
				"option":             optionText,
				"before_class":       beforeClass,
				"after_class":        afterClass,
				"active_after_click": isClassActive(afterClass),
			}).Info("filter option clicked")

			return false, nil
		}
		return false, fmt.Errorf("%w: 筛选组 %q 中未找到选项 %q", errors.ErrSelectorNotFound, groupLabel, optionText)
	}
	return false, fmt.Errorf("%w: 未找到筛选组 %q", errors.ErrSelectorNotFound, groupLabel)
}

// elementClass 读 element 的 class 属性；空值或读取失败都返回 ""。
func elementClass(el *rod.Element) string {
	cls, err := el.Attribute("class")
	if err != nil || cls == nil {
		return ""
	}
	return *cls
}

// isClassActive 判断 class 字符串里是否含有 active / selected 标记。
// XHS 用 .active / .selected / 包含 "active" / 包含 "selected" 几种写法都见过。
func isClassActive(class string) bool {
	if class == "" {
		return false
	}
	return strings.Contains(class, "active") || strings.Contains(class, "selected")
}

// clickInteractable 用三层策略点击筛选选项。
//
// 策略：
//  1. 优先用 go-rod 真实鼠标点击（ScrollIntoView + WaitInteractable + Click），
//     这是 CLAUDE.md 推荐的路径。WaitInteractable 限定 3s，避免吃光全部预算。
//  2. WaitInteractable / Click 失败时，用一段 JS dispatchEvent('click', bubbles)
//     兜底。绕开 z-index/overlay/动画判定。
//  3. 全部失败时，再做一次诊断 JS 输出 bbox / 可见性 / elementFromPoint，
//     方便定位 DOM 实际响应的容器。
//
// 关键：每个步骤都重新 .Context(page) / .Timeout() 派生独立预算，避免上一步
// 把整个父 context 烧光。
func clickInteractable(el *rod.Element, groupLabel, optionText string) error {
	// 滚到视野内
	_ = el.ScrollIntoView()

	// 1. 优先 go-rod 真实鼠标点击：WaitInteractable 限定 3s，避免吃光预算
	waitEl := el.Timeout(filterClickWaitInteractable)
	if _, werr := waitEl.WaitInteractable(); werr != nil {
		logrus.WithFields(logrus.Fields{
			"group": groupLabel, "option": optionText, "err": werr,
		}).Debug("WaitInteractable failed, fallback to JS click")
	} else {
		// Click 用一个独立的 5s 预算
		clickEl := el.Timeout(filterClickAction)
		if cerr := clickEl.Click(proto.InputMouseButtonLeft, 1); cerr == nil {
			return nil
		} else {
			logrus.WithFields(logrus.Fields{
				"group": groupLabel, "option": optionText, "err": cerr,
			}).Debug("rod click failed, fallback to JS click")
		}
	}

	// 2. JS click 兜底：派发可冒泡的 MouseEvent，更接近真实点击
	jsEl := el.Timeout(filterClickAction)
	if _, err := jsEl.Eval(jsClickScript); err != nil {
		// 3. 诊断：rod + JS 都失败时，输出 DOM 现场，方便后续排查
		logClickDiagnostics(el, groupLabel, optionText)
		return fmt.Errorf("%w: 选项 %q/%q rod+JS 都点不动: %v", errors.ErrFilterClickFailed, groupLabel, optionText, err)
	}
	return nil
}

// jsClickScript 派发一个可冒泡的 click MouseEvent，并把 mousedown / mouseup
// 顺序也补上。比直接 this.click() 更接近真实鼠标点击，能触发事件委托。
const jsClickScript = `() => {
	const r = this.getBoundingClientRect();
	const x = r.left + r.width / 2;
	const y = r.top + r.height / 2;
	const opts = { bubbles: true, cancelable: true, view: window, clientX: x, clientY: y, button: 0 };
	this.dispatchEvent(new MouseEvent('mousedown', opts));
	this.dispatchEvent(new MouseEvent('mouseup', opts));
	this.dispatchEvent(new MouseEvent('click', opts));
	return { x, y };
}`

// logClickDiagnostics 在 rod + JS 都失败时输出诊断信息：
// bbox / computed style / elementFromPoint / 是否在 viewport 内。
// JS 内部 JSON.stringify，避免 gson.JSON.Str() 对 object 返回空。
func logClickDiagnostics(el *rod.Element, groupLabel, optionText string) {
	res, err := el.Timeout(2 * time.Second).Eval(`() => {
		const r = this.getBoundingClientRect();
		const x = r.left + r.width / 2;
		const y = r.top + r.height / 2;
		const cs = window.getComputedStyle(this);
		const top = document.elementFromPoint(x, y);
		const topInfo = top ? {
			tag: top.tagName,
			cls: top.className,
			text: (top.textContent || '').slice(0, 40),
			isSelf: top === this,
			isDescendant: this.contains(top),
			isAncestor: top.contains(this),
		} : null;
		return JSON.stringify({
			bbox: { x: r.x, y: r.y, w: r.width, h: r.height },
			center: { x, y },
			inViewport: r.bottom > 0 && r.right > 0 && r.top < innerHeight && r.left < innerWidth,
			style: {
				display: cs.display,
				visibility: cs.visibility,
				pointerEvents: cs.pointerEvents,
				zIndex: cs.zIndex,
				opacity: cs.opacity,
			},
			elementFromPoint: topInfo,
			html: (this.outerHTML || '').slice(0, 200),
		});
	}`)
	if err != nil {
		logrus.WithFields(logrus.Fields{"group": groupLabel, "option": optionText, "err": err}).
			Warn("filter click diagnostics eval failed")
		return
	}
	logrus.WithFields(logrus.Fields{
		"group":  groupLabel,
		"option": optionText,
		"info":   res.Value.Str(),
	}).Warn("filter click failed; diagnostics dumped (用以排查 onClick 是否绑在 div.tags 上)")
}

// waitForFilterApplied 在 filterApplyTimeout 内做条件竞争。
func waitForFilterApplied(ctx context.Context, page *rod.Page, initial searchSnapshot) error {
	deadline := time.Now().Add(filterApplyTimeout)
	ticker := time.NewTicker(filterPollInterval)
	defer ticker.Stop()

	for {
		// 登录弹窗：扫码登录组件出现
		if pageHas(page, `.login-container .qrcode-img`) {
			return errors.ErrLoginRequired
		}
		// 安全验证 / 验证码：常见 iframe 或带 captcha 的容器
		if pageHas(page, `iframe[src*="captcha"], iframe[id*="captcha"], div[class*="captcha"]`) {
			return errors.ErrCaptchaOrSecurity
		}

		cur := captureSearchSnapshot(page)
		switch cur.State {
		case stateEmpty:
			return errors.ErrEmptyResult
		case stateFeeds:
			if filterChanged(initial, cur) {
				return nil
			}
		}

		if time.Now().After(deadline) {
			fields := logrus.Fields{
				"initial_fp":     initial.Fingerprint,
				"current_fp":     cur.Fingerprint,
				"initial_url":    initial.URLSearch,
				"current_url":    cur.URLSearch,
				"initial_active": initial.ActiveFilters,
				"current_active": cur.ActiveFilters,
				"state":          cur.State,
			}
			if activeFiltersOnlyChanged(initial, cur) {
				// click 让 UI 选中状态变了，但 feed 列表 / URL 都没刷新。
				// 通常是 JS dispatchEvent 的 isTrusted=false 被 XHS 真实搜索 XHR
				// 过滤掉，只触发了纯前端 UI 更新。返回 timeout（而不是当成功）
				// 是 issue #3 的核心修复点，避免静默返回旧的"综合"结果。
				logrus.WithFields(fields).Warn(
					"filter timeout: only UI active state changed, " +
						"feed list / URL not refreshed (likely synthetic-event被忽略)",
				)
			} else {
				logrus.WithFields(fields).Warn("search filter apply timed out (no signal changed)")
			}
			return errors.ErrFilterTimeout
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pageHas 是 page.Has 的安全封装，错误吞掉视为不存在。
func pageHas(page *rod.Page, selector string) bool {
	has, _, _ := page.Has(selector)
	return has
}

// filterChanged 判断筛选是否真的生效。
//
// **重要**：ActiveFilters（DOM 里 .active/.selected 的变化）单独不算成功信号。
// XHS 的 click handler 会立刻把目标选项 mark 成 active（UI 状态），但是真正
// 触发搜索 XHR / 改写 __INITIAL_STATE__.search.feeds 是更晚一点。如果只看
// ActiveFilters 就返回，会在 feed 列表还没刷新前就退出，调用方拿到的还是
// 旧的"综合"结果（issue #3）。
//
// 因此只接受能直接反映 feed 列表已经更新的信号：
//  1. Fingerprint（feeds 全量 id 拼接）变化
//  2. URLSearch（location.search）变化 —— XHS 部分版本会把 sort= 写进 query，
//     这只有在搜索 XHR 完成后才会发生
//
// ActiveFilters 仅用于诊断（区分"click 没生效" vs "click 触发了 UI 但 feed
// 还没刷新"）。
func filterChanged(initial, cur searchSnapshot) bool {
	if cur.Fingerprint != "" && cur.Fingerprint != initial.Fingerprint {
		return true
	}
	if cur.URLSearch != "" && cur.URLSearch != initial.URLSearch {
		return true
	}
	return false
}

// activeFiltersOnlyChanged 用于诊断：UI 选中状态变了，但 feed 列表 / URL
// 都没变。命中这种情况说明 click 触发了 UI handler 但没触发实际的搜索请求
// （例如 JS dispatchEvent 的 isTrusted=false 被 XHS 滤掉），需要 timeout
// 而不是当成功。
func activeFiltersOnlyChanged(initial, cur searchSnapshot) bool {
	if cur.ActiveFilters == initial.ActiveFilters {
		return false
	}
	if cur.Fingerprint != "" && cur.Fingerprint != initial.Fingerprint {
		return false
	}
	if cur.URLSearch != "" && cur.URLSearch != initial.URLSearch {
		return false
	}
	return true
}

// extractFeedsFromPage 读取 __INITIAL_STATE__ 中的 feeds 列表。
func extractFeedsFromPage(page *rod.Page) ([]Feed, error) {
	result := page.MustEval(`() => {
		if (window.__INITIAL_STATE__ &&
		    window.__INITIAL_STATE__.search &&
		    window.__INITIAL_STATE__.search.feeds) {
			const feeds = window.__INITIAL_STATE__.search.feeds;
			const feedsData = feeds.value !== undefined ? feeds.value : feeds._value;
			if (feedsData) {
				return JSON.stringify(feedsData);
			}
		}
		return "";
	}`).String()

	if result == "" {
		return nil, errors.ErrNoFeeds
	}

	var feeds []Feed
	if err := json.Unmarshal([]byte(result), &feeds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal feeds: %w", err)
	}
	return feeds, nil
}

// searchSnapshot 表示某一时刻搜索页的多维度指纹。
//
// 单一来源（top-3 feed id）不够：实测发现"最多点赞"筛选时，22 条 feed 的
// 顺序变了，但前两条恰好是默认排序也排在前面的高赞 note，top-3 fingerprint
// 没明显变化，会误判筛选未生效。
//
// 现在多信号 OR：
//   - Fingerprint: __INITIAL_STATE__.search.feeds 的 length + 全部 id 拼接
//   - URLSearch: location.search（XHS 部分版本会把 sort= 写进 query）
//   - ActiveFilters: 当前面板里 .active/.selected 标签的文本
//
// 任何一个变化都视为筛选生效。
type searchSnapshot struct {
	State         string `json:"state"`         // stateFeeds / stateEmpty / stateUnknown
	Fingerprint   string `json:"fingerprint"`   // length:id1,id2,...,idN
	URLSearch     string `json:"urlSearch"`     // location.search
	ActiveFilters string `json:"activeFilters"` // joined active/selected tag texts
}

const (
	stateFeeds   = "feeds"
	stateEmpty   = "empty"
	stateUnknown = "unknown"
)

// captureSnapshotJS 在浏览器内运行的快照脚本。挑这点 JS 是因为 __INITIAL_STATE__
// 是页面级 JS 全局变量，无法直接通过 go-rod 的 DOM 查询拿到；URL / DOM 查询则
// 顺手一起做掉，避免多次 RPC。
const captureSnapshotJS = `() => {
	const out = { state: 'unknown', fingerprint: '', urlSearch: '', activeFilters: '' };
	out.urlSearch = location.search || '';

	// 当前面板里 active/selected 的筛选项文本
	const activeNodes = document.querySelectorAll(
		'div.filter-panel .active, div.filter-panel .selected, ' +
			'div.filter-panel [class*="active"], div.filter-panel [class*="selected"]'
	);
	const activeTexts = [];
	activeNodes.forEach(n => {
		const t = (n.textContent || '').trim();
		if (t) activeTexts.push(t);
	});
	out.activeFilters = activeTexts.join('|');

	if (window.__INITIAL_STATE__ &&
	    window.__INITIAL_STATE__.search &&
	    window.__INITIAL_STATE__.search.feeds) {
		const feeds = window.__INITIAL_STATE__.search.feeds;
		const data = feeds.value !== undefined ? feeds.value : feeds._value;
		if (Array.isArray(data)) {
			if (data.length === 0) {
				out.state = 'empty';
			} else {
				out.state = 'feeds';
				// 全量 id（不再只取 top-3），任何顺序变化都能识别
				const ids = data.map(f => (f && f.id) || '');
				out.fingerprint = data.length + ':' + ids.join(',');
			}
		}
	}
	return JSON.stringify(out);
}`

func captureSearchSnapshot(page *rod.Page) searchSnapshot {
	val, err := page.Eval(captureSnapshotJS)
	if err != nil || val == nil {
		return searchSnapshot{State: stateUnknown}
	}
	return parseSearchSnapshot(val.Value.String())
}

func parseSearchSnapshot(raw string) searchSnapshot {
	if raw == "" {
		return searchSnapshot{State: stateUnknown}
	}
	var snap searchSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return searchSnapshot{State: stateUnknown}
	}
	if snap.State == "" {
		snap.State = stateUnknown
	}
	return snap
}

// IsFilterError 帮助 wrapper / 调用方判断错误是否属于筛选快速失败错误族。
func IsFilterError(err error) bool {
	switch {
	case stderrors.Is(err, errors.ErrFilterTimeout),
		stderrors.Is(err, errors.ErrLoginRequired),
		stderrors.Is(err, errors.ErrCaptchaOrSecurity),
		stderrors.Is(err, errors.ErrEmptyResult),
		stderrors.Is(err, errors.ErrSelectorNotFound),
		stderrors.Is(err, errors.ErrFilterClickFailed):
		return true
	}
	return false
}

func makeSearchURL(keyword string) string {

	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("source", "web_explore_feed")

	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_search_result_notes
	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_explore_feed
	return fmt.Sprintf("https://www.xiaohongshu.com/search_result?%s", values.Encode())
}
