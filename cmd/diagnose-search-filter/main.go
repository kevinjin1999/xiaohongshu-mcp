// diagnose-search-filter 是一个手动诊断工具：用真实浏览器跑一次带筛选的 search_feeds，
// 打印耗时和结果或快速失败错误码。需要本地已登录小红书并保存了 cookies。
//
// 用法示例：
//
//	go run ./cmd/diagnose-search-filter -keyword=美食 -sort_by=最多点赞 -note_type=图文
package main

import (
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func main() {
	var (
		keyword     string
		sortBy      string
		noteType    string
		publishTime string
		searchScope string
		location    string
		headless    bool
		binPath     string
		timeoutSec  int
	)

	flag.StringVar(&keyword, "keyword", "美食", "搜索关键词")
	flag.StringVar(&sortBy, "sort_by", "", "排序依据: 综合|最新|最多点赞|最多评论|最多收藏")
	flag.StringVar(&noteType, "note_type", "", "笔记类型: 不限|视频|图文")
	flag.StringVar(&publishTime, "publish_time", "", "发布时间: 不限|一天内|一周内|半年内")
	flag.StringVar(&searchScope, "search_scope", "", "搜索范围: 不限|已看过|未看过|已关注")
	flag.StringVar(&location, "location", "", "位置距离: 不限|同城|附近")
	flag.BoolVar(&headless, "headless", true, "是否使用无头浏览器")
	flag.StringVar(&binPath, "bin", "", "浏览器二进制文件路径")
	flag.IntVar(&timeoutSec, "timeout", 60, "整体超时（秒）")
	flag.Parse()

	if keyword == "" {
		logrus.Fatal("必须指定 -keyword")
	}

	b := browser.NewBrowser(headless, browser.WithBinPath(binPath))
	defer b.Close()

	page := b.NewPage()
	defer func() { _ = page.Close() }()

	action := xiaohongshu.NewSearchAction(page)
	filter := xiaohongshu.FilterOption{
		SortBy:      sortBy,
		NoteType:    noteType,
		PublishTime: publishTime,
		SearchScope: searchScope,
		Location:    location,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	logrus.Infof("开始搜索: keyword=%q filter=%+v", keyword, filter)
	start := time.Now()

	feeds, err := action.Search(ctx, keyword, filter)
	elapsed := time.Since(start)

	if err != nil {
		logrus.Errorf("搜索失败 (耗时 %s): %v", elapsed, err)
		fmt.Printf("error_code: %s\n", errorCode(err))
		os.Exit(1)
	}

	logrus.Infof("搜索成功 (耗时 %s)，共 %d 条 feed", elapsed, len(feeds))
	for i, f := range feeds {
		if i >= 5 {
			fmt.Printf("... 还有 %d 条\n", len(feeds)-5)
			break
		}
		fmt.Printf("[%d] %s | likes=%s | %s\n",
			i+1, f.ID, f.NoteCard.InteractInfo.LikedCount, f.NoteCard.DisplayTitle)
	}
}

// errorCode 把错误映射回 issue 中规定的错误码字符串。
func errorCode(err error) string {
	switch {
	case stderrors.Is(err, errors.ErrFilterTimeout):
		return "filter_timeout"
	case stderrors.Is(err, errors.ErrLoginRequired):
		return "login_required"
	case stderrors.Is(err, errors.ErrCaptchaOrSecurity):
		return "captcha_or_security_check"
	case stderrors.Is(err, errors.ErrEmptyResult):
		return "empty_result"
	case stderrors.Is(err, errors.ErrSelectorNotFound):
		return "selector_not_found"
	case stderrors.Is(err, errors.ErrNoFeeds):
		return "no_feeds"
	default:
		return "unknown_error"
	}
}
