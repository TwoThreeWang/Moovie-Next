// 在后台线程里求解人机校验，不卡住页面。收到 { token }，算完回复 { nonce }。
import { parseToken, search } from '../lib/pow.js';

self.addEventListener('message', (event) => {
  const parsed = parseToken(event.data?.token);
  if (!parsed) {
    self.postMessage({ error: '挑战格式不对' });
    return;
  }
  self.postMessage({ nonce: search(parsed.challenge, parsed.bits) });
});
