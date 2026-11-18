// IndexedDB 的简单封装。浏览器不让用 IndexedDB 时（部分隐私模式）退回到内存，页面照常工作，只是不持久。
const NAME = 'moovie', VERSION = 1, STORES = ['kv', 'cache', 'private'];

let opening;
const memory = Object.fromEntries(STORES.map((s) => [s, new Map()]));

function open() {
  opening ??= new Promise((resolve) => {
    try {
      const request = indexedDB.open(NAME, VERSION);
      request.onupgradeneeded = () => { for (const store of STORES) if (!request.result.objectStoreNames.contains(store)) request.result.createObjectStore(store); };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => resolve(null);
      request.onblocked = () => resolve(null);
    } catch { resolve(null); }
  });
  return opening;
}

async function run(store, mode, action) {
  const db = await open();
  if (!db) return action(null)?.result; // 内存退路：和 IndexedDB 一样只返回结果本身
  return new Promise((resolve, reject) => {
    const tx = db.transaction(store, mode);
    const request = action(tx.objectStore(store));
    tx.oncomplete = () => resolve(request?.result);
    tx.onerror = tx.onabort = () => reject(tx.error);
  });
}

export const get = (store, key) => run(store, 'readonly', (s) => (s ? s.get(key) : { result: memory[store].get(key) }));
export const set = (store, key, value) => run(store, 'readwrite', (s) => (s ? s.put(value, key) : void memory[store].set(key, value)));
export const del = (store, key) => run(store, 'readwrite', (s) => (s ? s.delete(key) : void memory[store].delete(key)));

/** 返回 key 以 prefix 开头的所有条目：[{ key, value }]。 */
export async function entries(store, prefix = '') {
  const db = await open();
  if (!db) return [...memory[store]].filter(([k]) => String(k).startsWith(prefix)).map(([key, value]) => ({ key, value }));
  return new Promise((resolve, reject) => {
    const out = [];
    const range = prefix ? IDBKeyRange.bound(prefix, prefix + '￿') : undefined;
    const request = db.transaction(store, 'readonly').objectStore(store).openCursor(range);
    request.onsuccess = () => {
      const cursor = request.result;
      if (!cursor) return resolve(out);
      out.push({ key: cursor.key, value: cursor.value });
      cursor.continue();
    };
    request.onerror = () => reject(request.error);
  });
}

/** 删除所有 store 里 key 以 prefix 开头的条目（注销账号时清掉这个账号在本机的数据）。 */
export async function clearPrefix(prefix) {
  for (const store of STORES) {
    for (const { key } of await entries(store, prefix)) await del(store, key);
  }
}
