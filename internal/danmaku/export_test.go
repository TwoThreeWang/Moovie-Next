package danmaku

import "time"

// SetWait 改页面请求等第三方弹幕的时长，测试用。
func SetWait(s *Service, wait time.Duration) { s.wait = wait }
