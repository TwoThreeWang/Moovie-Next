-- attempted_at：任务最近一次开始执行的时间。后台"最近的错误"按它排序，显示的才是刚刚发生的错误；
-- 以前按 run_at 排，取到的是下次重试排得最远（也就是失败得最早）的那个任务的旧错误。
-- postponed_at：这一轮"被对方限流、推迟再做"开始的时间。推迟最多 3 天从这里算起，
-- 以前从入队时间算，排了几天的积压任务一碰上限流，每被拒绝一次就用掉一次重试。
ALTER TABLE jobs ADD COLUMN attempted_at timestamptz, ADD COLUMN postponed_at timestamptz;
