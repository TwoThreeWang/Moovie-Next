// 账号注销后：清掉这个账号留在本机的数据（订阅地址、加密密钥、本地历史、缓存）。
import { clearPrefix } from '../core/idb.js';

const id = document.querySelector('[data-clear-user]')?.dataset.clearUser;
if (/^\d+$/.test(id ?? '')) clearPrefix(`u${id}:`).catch(() => {});
