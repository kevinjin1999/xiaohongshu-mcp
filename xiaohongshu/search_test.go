package xiaohongshu

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

func TestSearch(t *testing.T) {

	t.Skip("SKIP: 测试发布")

	b := browser.NewBrowser(false)
	defer b.Close()

	page := b.NewPage()
	defer func() {
		_ = page.Close()
	}()

	action := NewSearchAction(page)

	feeds, err := action.Search(context.Background(), "Kimi")
	require.NoError(t, err)
	require.NotEmpty(t, feeds, "feeds should not be empty")

	fmt.Printf("成功获取到 %d 个 Feed\n", len(feeds))

	for _, feed := range feeds {
		fmt.Printf("Feed ID: %s\n", feed.ID)
		fmt.Printf("Feed Title: %s\n", feed.NoteCard.DisplayTitle)
	}
}

func TestSearchWithFilters(t *testing.T) {

	//t.Skip("SKIP: 测试筛选功能")

	b := browser.NewBrowser(false)
	defer b.Close()

	page := b.NewPage()
	defer func() {
		_ = page.Close()
	}()

	action := NewSearchAction(page)

	// 使用新的 FilterOption 结构
	filter := FilterOption{
		NoteType:    "图文",
		PublishTime: "一天内",
	}

	feeds, err := action.Search(context.Background(), "dn432", filter)
	require.NoError(t, err)
	require.NotEmpty(t, feeds, "feeds should not be empty")

	fmt.Printf("成功获取到 %d 个筛选后的 Feed\n", len(feeds))

	for _, feed := range feeds {
		fmt.Printf("Feed ID: %s\n", feed.ID)
		fmt.Printf("Feed Title: %s\n", feed.NoteCard.DisplayTitle)
	}
}

func TestFilterValidation(t *testing.T) {
	// 有效筛选选项转换
	validFilter := FilterOption{
		NoteType:    "图文",
		PublishTime: "一天内",
	}
	internalFilters, err := convertToInternalFilters(validFilter)
	require.NoError(t, err)
	require.Len(t, internalFilters, 2)

	for _, filter := range internalFilters {
		require.NoError(t, validateInternalFilterOption(filter))
		require.NotEmpty(t, filter.GroupLabel)
		require.NotEmpty(t, filter.OptionText)
	}

	// 无效筛选值
	invalidFilter := FilterOption{
		NoteType: "不存在的类型",
	}
	_, err = convertToInternalFilters(invalidFilter)
	require.Error(t, err)
	require.Contains(t, err.Error(), "未找到文本")

	// 全字段有效
	allFilters := FilterOption{
		SortBy:      "最新",
		NoteType:    "视频",
		PublishTime: "一周内",
		SearchScope: "已关注",
		Location:    "同城",
	}
	internalFilters, err = convertToInternalFilters(allFilters)
	require.NoError(t, err)
	require.Len(t, internalFilters, 5)
}

func TestConvertToInternalFilters_GroupLabels(t *testing.T) {
	// 验证转换后落在正确的筛选组，避免回归到 nth-child 索引
	got, err := convertToInternalFilters(FilterOption{
		SortBy:      "最多点赞",
		NoteType:    "视频",
		PublishTime: "半年内",
		SearchScope: "未看过",
		Location:    "附近",
	})
	require.NoError(t, err)
	require.Len(t, got, 5)

	expected := map[string]string{
		"排序依据": "最多点赞",
		"笔记类型": "视频",
		"发布时间": "半年内",
		"搜索范围": "未看过",
		"位置距离": "附近",
	}

	for _, f := range got {
		require.Equal(t, expected[f.GroupLabel], f.OptionText, "组 %q", f.GroupLabel)
	}
}

func TestValidateInternalFilterOption_Empty(t *testing.T) {
	require.Error(t, validateInternalFilterOption(internalFilterOption{}))
	require.Error(t, validateInternalFilterOption(internalFilterOption{GroupLabel: "排序依据"}))
	require.Error(t, validateInternalFilterOption(internalFilterOption{OptionText: "最新"}))
	require.Error(t, validateInternalFilterOption(internalFilterOption{GroupLabel: "不存在", OptionText: "最新"}))
	require.Error(t, validateInternalFilterOption(internalFilterOption{GroupLabel: "排序依据", OptionText: "不存在"}))
	require.NoError(t, validateInternalFilterOption(internalFilterOption{GroupLabel: "排序依据", OptionText: "最多点赞"}))
}

func TestParseSearchSnapshot(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantState string
		wantFp    string
	}{
		{"空字符串视为未知", "", stateUnknown, ""},
		{"非法 JSON 视为未知", "not-json", stateUnknown, ""},
		{
			"有 feed 时返回 feeds 状态及指纹",
			`{"state":"feeds","fingerprint":"3:a,b,c"}`,
			stateFeeds, "3:a,b,c",
		},
		{
			"empty 状态保留",
			`{"state":"empty","fingerprint":""}`,
			stateEmpty, "",
		},
		{
			"未知状态默认填 unknown",
			`{"fingerprint":"x"}`,
			stateUnknown, "x",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseSearchSnapshot(c.raw)
			require.Equal(t, c.wantState, got.State)
			require.Equal(t, c.wantFp, got.Fingerprint)
		})
	}
}

func TestIsFilterError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.ErrFilterTimeout, true},
		{errors.ErrLoginRequired, true},
		{errors.ErrCaptchaOrSecurity, true},
		{errors.ErrEmptyResult, true},
		{errors.ErrSelectorNotFound, true},
		{fmt.Errorf("wrap: %w", errors.ErrFilterTimeout), true},
		{stderrors.New("random"), false},
		{nil, false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, IsFilterError(c.err), "err=%v", c.err)
	}
}

func TestFilterErrorSentinelStrings(t *testing.T) {
	// 错误信息文本是稳定接口，wrapper 可能按字符串匹配
	require.Equal(t, "filter_timeout", errors.ErrFilterTimeout.Error())
	require.Equal(t, "login_required", errors.ErrLoginRequired.Error())
	require.Equal(t, "captcha_or_security_check", errors.ErrCaptchaOrSecurity.Error())
	require.Equal(t, "empty_result", errors.ErrEmptyResult.Error())
	require.Equal(t, "selector_not_found", errors.ErrSelectorNotFound.Error())
}
