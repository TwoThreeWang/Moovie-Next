// 大图预览：点剧照打开，左右切换（按钮、方向键、手机上左右滑），点空白处或按 Esc 关闭。
// 用到时才加载这个模块；大图也是打开时才下载。
let dialog, image, counter, items = [], index = 0;

function build() {
  dialog = document.createElement('dialog');
  dialog.className = 'lightbox';
  dialog.innerHTML = `<button type="button" class="lb-btn lb-close" aria-label="关闭"><svg class="i"><use href="#i-x"/></svg></button>
<button type="button" class="lb-btn lb-prev" aria-label="上一张"><svg class="i"><use href="#i-chev-l"/></svg></button>
<figure><img alt="" decoding="async"><span class="spinner" aria-hidden="true"></span></figure>
<button type="button" class="lb-btn lb-next" aria-label="下一张"><svg class="i"><use href="#i-chev-r"/></svg></button>
<p class="lb-count" aria-live="polite"></p>`;
  image = dialog.querySelector('img');
  counter = dialog.querySelector('.lb-count');
  image.addEventListener('load', () => dialog.classList.remove('is-loading'));
  image.addEventListener('error', () => dialog.classList.remove('is-loading'));
  dialog.querySelector('.lb-close').addEventListener('click', () => dialog.close());
  dialog.querySelector('.lb-prev').addEventListener('click', () => show(index - 1));
  dialog.querySelector('.lb-next').addEventListener('click', () => show(index + 1));
  // 点图片以外的地方关闭
  dialog.addEventListener('click', (ev) => { if (ev.target === dialog || ev.target.tagName === 'FIGURE') dialog.close(); });
  dialog.addEventListener('keydown', (ev) => {
    if (ev.key === 'ArrowLeft') show(index - 1);
    if (ev.key === 'ArrowRight') show(index + 1);
  });
  // 手机上左右滑动切换
  let startX = null;
  dialog.addEventListener('touchstart', (ev) => { startX = ev.touches.length === 1 ? ev.touches[0].clientX : null; }, { passive: true });
  dialog.addEventListener('touchend', (ev) => {
    if (startX === null) return;
    const dx = ev.changedTouches[0].clientX - startX;
    if (Math.abs(dx) > 50) show(index + (dx < 0 ? 1 : -1));
    startX = null;
  });
  dialog.addEventListener('close', () => { image.removeAttribute('src'); });
  document.body.append(dialog);
}

function show(next) {
  if (!items.length) return;
  index = (next + items.length) % items.length;
  const item = items[index];
  dialog.classList.add('is-loading');
  image.src = item.src;
  image.alt = item.alt;
  counter.textContent = items.length > 1 ? `${index + 1} / ${items.length}` : '';
  dialog.classList.toggle('single', items.length < 2);
  // 提前下载下一张，切换时不用等
  if (items.length > 1) new Image().src = items[(index + 1) % items.length].src;
}

/** 打开大图预览。list: [{ src, alt }]，start 是先显示第几张。 */
export function openLightbox(list, start = 0) {
  if (!dialog) build();
  items = list;
  if (!dialog.open) dialog.showModal();
  show(start);
}
