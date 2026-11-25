import { csrfToken, h, userKey } from '../core/api.js';

const editor = document.querySelector('[data-editor]');
if (editor) init(editor);

function init(root) {
  const form = root.querySelector('form');
  const title = form.elements.title;
  const description = form.elements.description;
  const entryField = form.elements.entries;
  const search = root.querySelector('[data-search]');
  const results = root.querySelector('[data-results]');
  const searchStatus = root.querySelector('[data-search-status]');
  const items = root.querySelector('[data-items]');
  const count = root.querySelector('[data-count]');
  const empty = root.querySelector('[data-empty]');
  const draftStatus = root.querySelector('[data-draft-status]');
  const error = root.querySelector('[data-editor-error]');
  const discard = root.querySelector('[data-discard]');
  const restore = root.querySelector('[data-restore]');
  const saveButtons = [...root.querySelectorAll('[data-save]')];
  const preview = root.querySelector('[data-preview]');
  const previewDialog = root.querySelector('[data-preview-dialog]');
  const previewBody = root.querySelector('[data-preview-body]');
  const undo = root.querySelector('[data-undo-wrap]');
  const key = userKey('collection-draft:' + form.elements.id.value);
  const base = form.elements.base.value;

  let entries = JSON.parse(root.dataset.entries);
  let found = [], searchTimer, searchRequest, removed, dragged;
  let restoreDraft, previewRequest, saving = false;
  const snapshot = () => ({
    base, title: title.value, description: description.value, entries,
  });
  const initial = JSON.stringify(snapshot());

  function showError(message) {
    error.hidden = !message;
    error.textContent = message;
  }

  function persist() {
    try {
      localStorage.setItem(key, JSON.stringify(snapshot()));
      draftStatus.textContent = '本机草稿已保存';
      discard.hidden = false;
      restore.hidden = true;
      restoreDraft = null;
    } catch {
      draftStatus.textContent = '无法保存本机草稿，请保持页面打开直至保存。';
    }
  }

  function load(draft) {
    title.value = draft.title;
    description.value = draft.description;
    entries = draft.entries;
  }

  try {
    const draft = JSON.parse(localStorage.getItem(key));
    if (draft && Array.isArray(draft.entries) && typeof draft.title === 'string') {
      discard.hidden = false;
      if (draft.base === base) {
        load(draft);
        draftStatus.textContent = '已恢复本机草稿';
      } else {
        restoreDraft = draft;
        restore.hidden = false;
        draftStatus.textContent = '片单已有更新。请核对当前版本，再决定是否恢复旧草稿。';
      }
    }
  } catch {
    draftStatus.textContent = '无法读取本机草稿。';
  }

  function artwork(entry) {
    return entry.poster
      ? h('img', { src: entry.poster, alt: '', loading: 'lazy', width: 42, height: 63 })
      : h('span', { class: 'collection-poster-empty', 'aria-hidden': 'true' });
  }

  function button(text, action, props = {}) {
    return h('button', { type: 'button', class: 'btn btn-glass', text, onclick: action, ...props });
  }

  function drawResults() {
    results.replaceChildren(...found.map(entry => {
      const selected = entries.some(item => item.id === entry.id);
      return h('li', { class: 'collection-result' }, artwork(entry),
        h('div', {}, h('strong', { text: entry.title }), h('div', { class: 'hint', text: entry.meta })),
        button(selected ? '已加入' : '加入', () => {
          if (entries.some(item => item.id === entry.id)) return;
          if (entries.length >= 200) { showError('一份片单最多 200 部作品。'); return; }
          entries.push({ ...entry, note: '' });
          update();
          results.querySelector('button:not([disabled])')?.focus();
        }, { disabled: selected || saving, 'aria-label': selected ? entry.title + '已加入' : '加入' + entry.title }));
    }));
  }

  function drawEntries(focusID, action) {
    items.replaceChildren(...entries.map((entry, index) => {
      const controls = h('div', { class: 'collection-row-controls' },
        button('置顶', () => move(index, 0, 'top'), { disabled: index === 0, 'data-action': 'top', 'aria-label': '置顶' + entry.title }),
        button('上移', () => move(index, index - 1, 'up'), { disabled: index === 0, 'data-action': 'up', 'aria-label': '上移' + entry.title }),
        button('下移', () => move(index, index + 1, 'down'), { disabled: index === entries.length - 1, 'data-action': 'down', 'aria-label': '下移' + entry.title }),
        button('移除', () => {
          removed = { entry: entries.splice(index, 1)[0], index };
          undo.hidden = false;
          root.querySelector('[data-removed]').textContent = '已移除《' + entry.title + '》';
          update();
          root.querySelector('[data-undo]').focus();
        }, { 'data-action': 'remove', 'aria-label': '移除' + entry.title }),
      );
      const note = h('textarea', {
        rows: 2, maxlength: 300, placeholder: '推荐语（选填）', 'aria-label': entry.title + '的推荐语',
        oninput: event => { entry.note = event.target.value; },
      });
      note.value = entry.note ?? '';
      return h('li', { class: 'collection-editor-entry', dataset: { id: entry.id } },
        h('div', { class: 'collection-entry-top' },
          h('span', { class: 'collection-drag', draggable: 'true', 'aria-hidden': 'true', title: '拖动排序', text: '⠿' }),
          artwork(entry),
          h('div', {}, h('strong', { text: entry.title }), h('div', { class: 'hint', text: entry.meta }),
            entry.hidden && h('div', { class: 'hint', text: '暂时隐藏，访客不可见' })),
        ),
        note, controls,
      );
    }));
    count.textContent = entries.length + ' 部';
    empty.hidden = entries.length > 0;
    preview.disabled = !entries.length || saving;
    for (const button of saveButtons) button.disabled = preview.disabled;
    if (focusID) {
      const row = [...items.children].find(node => node.dataset.id === focusID);
      (row?.querySelector('[data-action="' + action + '"]:not(:disabled)') ??
        row?.querySelector('button:not(:disabled)'))?.focus();
    }
  }

  function update(focusID, action) {
    showError('');
    drawEntries(focusID, action);
    drawResults();
    persist();
  }

  function move(from, to, action = 'up') {
    if (from < 0 || to < 0 || to >= entries.length || from === to) return;
    const [entry] = entries.splice(from, 1);
    entries.splice(to, 0, entry);
    update(entry.id, action);
  }

  items.addEventListener('dragstart', event => {
    if (!event.target.closest('.collection-drag')) return;
    dragged = event.target.closest('[data-id]').dataset.id;
    event.dataTransfer.setData('text/plain', dragged);
    event.dataTransfer.effectAllowed = 'move';
  });
  items.addEventListener('dragover', event => {
    if (dragged && event.target.closest('[data-id]')) event.preventDefault();
  });
  items.addEventListener('dragend', () => { dragged = null; });
  items.addEventListener('drop', event => {
    const target = event.target.closest('[data-id]');
    if (!dragged || !target) return;
    event.preventDefault();
    move(entries.findIndex(item => item.id === dragged), entries.findIndex(item => item.id === target.dataset.id));
    dragged = null;
  });

  root.querySelector('[data-undo]').addEventListener('click', () => {
    if (!removed || entries.some(item => item.id === removed.entry.id)) return;
    if (entries.length >= 200) { showError('请先移除一部作品，再撤销这次移除。'); return; }
    entries.splice(removed.index, 0, removed.entry);
    undo.hidden = true;
    update(removed.entry.id, 'remove');
    removed = null;
  });

  async function find() {
    const q = search.value.trim();
    if (!q) return;
    const request = new AbortController();
    searchRequest = request;
    searchStatus.textContent = '正在搜索…';
    try {
      const response = await fetch('/me/collections/search?' + new URLSearchParams({ q }), {
        headers: { Accept: 'application/json' }, signal: request.signal,
      });
      const data = await response.json();
      if (request.signal.aborted) return;
      if (!response.ok) throw new Error(data.error ?? '搜索失败，请重试。');
      found = data;
      drawResults();
      searchStatus.textContent = found.length ? '找到 ' + found.length + ' 部作品' : '没有找到，试试别名或更短的关键词。';
    } catch (err) {
      if (!request.signal.aborted) searchStatus.textContent = err.message || '搜索失败，请重试。';
    }
  }

  search.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchRequest?.abort();
    found = [];
    drawResults();
    searchStatus.textContent = search.value.trim() ? '等待搜索…' : '输入片名，点「加入」收录作品。';
    if (search.value.trim()) searchTimer = setTimeout(find, 300);
  });
  search.addEventListener('keydown', event => {
    if (event.key !== 'Enter' || event.isComposing) return;
    event.preventDefault();
    clearTimeout(searchTimer);
    searchRequest?.abort();
    find();
  });

  // 标题框里按回车会触发表单里第一个提交按钮（保存或投稿），不让它意外提交。
  title.addEventListener('keydown', event => { if (event.key === 'Enter' && !event.isComposing) event.preventDefault(); });
  form.addEventListener('input', event => { if (event.target !== search) persist(); });
  discard.addEventListener('click', () => {
    if (!confirm('放弃本机草稿并恢复当前保存版本？')) return;
    try { localStorage.removeItem(key); }
    catch { showError('无法删除本机草稿。'); return; }
    load(JSON.parse(initial));
    removed = restoreDraft = null;
    undo.hidden = discard.hidden = restore.hidden = true;
    showError('');
    drawEntries();
    drawResults();
    draftStatus.textContent = '已恢复打开编辑器时的版本';
  });
  restore.addEventListener('click', () => {
    if (!restoreDraft || !confirm('恢复旧草稿会替换当前编辑内容。保存前请核对之前的修改。')) return;
    load(restoreDraft);
    update();
  });

  function formData() {
    entryField.value = JSON.stringify(entries.map(({ id, note }) => ({ id, note })));
    return new FormData(form);
  }

  // 预览用 fetch 取回片段，显示在本页的对话框里。不能把表单 POST 到新标签页：
  // 安卓上已安装的网页应用、国产浏览器和应用内浏览器打开新窗口时只带地址，POST 的内容会丢，
  // 服务器收到的是 GET /me/collections/preview，只能报错。
  preview.addEventListener('click', async () => {
    if (saving || !entries.length) return;
    showError('');
    previewRequest?.abort();
    const request = previewRequest = new AbortController();
    const timer = setTimeout(() => request.abort(), 15000);
    preview.setAttribute('aria-busy', 'true');
    try {
      const response = await fetch('/me/collections/preview', {
        method: 'POST', headers: { 'HX-Request': 'true', 'X-CSRF-Token': csrfToken() }, body: formData(),
        signal: request.signal,
      });
      const text = await response.text();
      if (request !== previewRequest) return;
      if (!response.ok) {
        const plain = (response.headers.get('Content-Type') ?? '').startsWith('text/plain');
        throw new Error(plain && text.trim() ? text.trim() : '预览失败，请重试。');
      }
      previewBody.innerHTML = text;
      previewDialog.showModal();
      previewDialog.scrollTop = 0;
    } catch (err) {
      if (request !== previewRequest) return;
      showError(request.signal.aborted ? '预览超时，请重试。' : err instanceof TypeError ? '网络连接失败，请重试。' : err.message);
    } finally {
      clearTimeout(timer);
      if (request === previewRequest) {
        previewRequest = null;
        preview.removeAttribute('aria-busy');
      }
    }
  });
  root.querySelector('[data-preview-close]').addEventListener('click', () => previewDialog.close());

  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (saving || !entries.length) return;
    const button = saveButtons.includes(event.submitter) ? event.submitter : saveButtons[0];
    if (button.dataset.reviewConfirm && !confirm(button.dataset.reviewConfirm)) return;
    showError('');
    const data = formData();
    data.set('intent', button.value);
    const savedDraft = JSON.stringify(snapshot());
    const label = button.textContent;
    const submitting = button.value === 'submit';
    saving = true;
    form.inert = true;
    button.textContent = submitting ? '提交中…' : '保存中…';
    draftStatus.textContent = button.textContent;
    const request = new AbortController();
    const timer = setTimeout(() => request.abort(), 15000);
    try {
      const response = await fetch(form.action, {
        method: 'POST', headers: { Accept: 'application/json', 'X-CSRF-Token': csrfToken() }, body: data,
        signal: request.signal,
      });
      const result = await response.json();
      if (!response.ok || !result.redirect) throw new Error(result.error ?? '未能确认操作结果，请先查看片单列表。');
      try { if (localStorage.getItem(key) === savedDraft) localStorage.removeItem(key); } catch { /* 服务器已保存 */ }
      location.href = result.redirect;
    } catch (err) {
      const uncertain = request.signal.aborted || err instanceof SyntaxError || err instanceof TypeError;
      showError(uncertain ? '未能确认操作结果，请先查看片单列表，再决定是否重试。' : err.message);
      draftStatus.textContent = '编辑内容仍在当前页面';
    } finally {
      clearTimeout(timer);
      saving = false;
      form.inert = false;
      button.textContent = label;
    }
  });

  drawEntries();
}
