-- 以前任何人（包括没登录的访客和爬虫）打开没补全的作品页，豆瓣任务都会提到最高优先级、还会去读详情网页。
-- 现在只有登录用户打开才算有人在等；排着的这些先降回普通的"新作品"，登录用户再打开时会重新提上来。
UPDATE jobs SET priority = 5 WHERE kind = 'media.refresh' AND status = 'queued' AND priority = 20;
