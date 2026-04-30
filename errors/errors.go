package errors

import "errors"

var ErrNoFeeds = errors.New("没有捕获到 feeds 数据")
var ErrNoFeedDetail = errors.New("没有捕获到 feed 详情数据")

// search_feeds 筛选相关的快速失败错误。
// 错误信息使用稳定的英文标识，方便 wrapper / 调用方按 errors.Is 区分。
var (
	ErrFilterTimeout     = errors.New("filter_timeout")
	ErrLoginRequired     = errors.New("login_required")
	ErrCaptchaOrSecurity = errors.New("captcha_or_security_check")
	ErrEmptyResult       = errors.New("empty_result")
	ErrSelectorNotFound  = errors.New("selector_not_found")
)
