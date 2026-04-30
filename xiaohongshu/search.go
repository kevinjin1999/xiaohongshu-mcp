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
	// 单步 UI 操作（点击筛选按钮 / 等待面板出现 / 点击某个选项）的短超时。
	filterUITimeout = 5 * time.Second
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
//  3. 点击完最后一个选项后进入条件竞争，命中以下任一条件就立即返回：
//     - feed 列表指纹变化
//     - 出现登录弹窗
//     - 出现安全验证 / 验证码
//     - 检测到空结果
//     - selector 找不到
//     - 达到 filterApplyTimeout 总预算
func (s *SearchAction) applyFilters(ctx context.Context, filters []internalFilterOption) error {
	page := s.page.Context(ctx)

	// 1. 点击前先快照当前 feed，用于检测筛选生效
	initial := captureSearchSnapshot(page)

	// 2. 短超时悬停筛选按钮
	filterBtn, err := page.Timeout(filterUITimeout).Element(`div.filter`)
	if err != nil {
		return fmt.Errorf("%w: 找不到筛选按钮 div.filter: %v", errors.ErrSelectorNotFound, err)
	}
	if err := filterBtn.Hover(); err != nil {
		return fmt.Errorf("%w: 悬停筛选按钮失败: %v", errors.ErrSelectorNotFound, err)
	}

	// 3. 短超时等待筛选面板出现
	panel, err := page.Timeout(filterUITimeout).Element(`div.filter-panel`)
	if err != nil {
		return fmt.Errorf("%w: 找不到筛选面板 div.filter-panel: %v", errors.ErrSelectorNotFound, err)
	}

	// 4. 按文本依次点击每个筛选选项
	for _, f := range filters {
		if err := clickFilterOption(panel, f.GroupLabel, f.OptionText); err != nil {
			return err
		}
	}

	// 5. 条件竞争：等待筛选生效或快速失败
	return waitForFilterApplied(ctx, page, initial)
}

// clickFilterOption 在筛选面板内按文本定位筛选行 + 选项并点击。
func clickFilterOption(panel *rod.Element, groupLabel, optionText string) error {
	rows, err := panel.Elements(`div.filters`)
	if err != nil || len(rows) == 0 {
		return fmt.Errorf("%w: 筛选面板内没有筛选行 div.filters: %v", errors.ErrSelectorNotFound, err)
	}

	for _, row := range rows {
		text, _ := row.Text()
		if !strings.Contains(text, groupLabel) {
			continue
		}
		tags, err := row.Elements(`div.tags`)
		if err != nil || len(tags) == 0 {
			return fmt.Errorf("%w: 筛选组 %q 内没有选项 div.tags: %v", errors.ErrSelectorNotFound, groupLabel, err)
		}
		for _, tag := range tags {
			t, _ := tag.Text()
			if strings.TrimSpace(t) != optionText {
				continue
			}
			if err := tag.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return fmt.Errorf("点击筛选选项 %q/%q 失败: %w", groupLabel, optionText, err)
			}
			return nil
		}
		return fmt.Errorf("%w: 筛选组 %q 中未找到选项 %q", errors.ErrSelectorNotFound, groupLabel, optionText)
	}
	return fmt.Errorf("%w: 未找到筛选组 %q", errors.ErrSelectorNotFound, groupLabel)
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
			if cur.Fingerprint != "" && cur.Fingerprint != initial.Fingerprint {
				return nil
			}
		}

		if time.Now().After(deadline) {
			logrus.WithFields(logrus.Fields{
				"initial_fp": initial.Fingerprint,
				"current_fp": cur.Fingerprint,
				"state":      cur.State,
			}).Warn("search filter apply timed out")
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

// searchSnapshot 表示某一时刻搜索页 __INITIAL_STATE__ 的指纹，用于检测筛选生效。
type searchSnapshot struct {
	State       string `json:"state"`       // stateFeeds / stateEmpty / stateUnknown
	Fingerprint string `json:"fingerprint"` // length:firstId,secondId,thirdId
}

const (
	stateFeeds   = "feeds"
	stateEmpty   = "empty"
	stateUnknown = "unknown"
)

// captureSnapshotJS 在浏览器内运行的快照脚本。挑这点 JS 是因为 __INITIAL_STATE__
// 是页面级 JS 全局变量，无法直接通过 go-rod 的 DOM 查询拿到。
const captureSnapshotJS = `() => {
	const out = { state: 'unknown', fingerprint: '' };
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
				const ids = data.slice(0, 3).map(f => (f && f.id) || '');
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
		stderrors.Is(err, errors.ErrSelectorNotFound):
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
