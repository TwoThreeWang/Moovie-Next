// 分享卡片：在浏览器里用 canvas 画一张 1080×1440 的竖图（作品、月度报告、片单、短评），
// 带二维码，方便发微信、朋友圈和小红书。服务器不生成图片：省掉中文字体和图片处理，
// 也不额外消耗服务器资源。卡片内容由页面上的 data-card（JSON）提供。
import { encodeQR } from '../lib/qr.js';
import { wrapText } from '../lib/cardtext.js';
import { toast } from './api.js';

const W = 1080, H = 1440, PAD = 72, FOOT = 1196;
const BG = '#101014', FG = '#f4f4f5', DIM = 'rgba(244,244,245,.62)', BRAND = '#e2605d';
const FONT = '-apple-system, BlinkMacSystemFont, "PingFang SC", "Hiragino Sans GB", "Noto Sans CJK SC", "Microsoft YaHei", sans-serif';
const font = (size, weight = 400) => `${weight} ${size}px ${FONT}`;

/** 加载图片。跨域图片（TMDB）用 CORS 加载，失败时返回 null，画占位而不是让整张图失败。 */
function loadImage(src) {
  if (!src) return Promise.resolve(null);
  return new Promise((resolve) => {
    const img = new Image();
    const url = new URL(src, location.href);
    if (url.origin !== location.origin) img.crossOrigin = 'anonymous';
    img.onload = () => resolve(img);
    img.onerror = () => resolve(null);
    img.src = url.href;
  });
}

function roundRect(ctx, x, y, w, h, r) {
  ctx.beginPath();
  if (ctx.roundRect) { ctx.roundRect(x, y, w, h, r); return; }
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

/** 按 object-fit: cover 把图片画进圆角矩形；没有图片时画一块深色占位。 */
function cover(ctx, img, x, y, w, h, r = 0) {
  ctx.save();
  roundRect(ctx, x, y, w, h, r);
  ctx.clip();
  if (img) {
    const scale = Math.max(w / img.naturalWidth, h / img.naturalHeight);
    const dw = img.naturalWidth * scale, dh = img.naturalHeight * scale;
    ctx.drawImage(img, x + (w - dw) / 2, y + (h - dh) / 2, dw, dh);
  } else {
    ctx.fillStyle = '#1d1d22';
    ctx.fillRect(x, y, w, h);
  }
  ctx.restore();
}

/** 写几行字，返回下一行的 y。 */
function lines(ctx, text, x, y, { size, weight = 400, color = FG, width = W - PAD * 2, max = 2, height = size * 1.4, align = 'left' }) {
  ctx.font = font(size, weight);
  ctx.fillStyle = color;
  ctx.textAlign = align;
  ctx.textBaseline = 'alphabetic';
  for (const line of wrapText(text, width, (s) => ctx.measureText(s).width, max)) {
    ctx.fillText(line, x, y + size);
    y += height;
  }
  ctx.textAlign = 'left';
  return y;
}

/** 背景：有海报时铺一层模糊的海报，再压暗；没有时用品牌色的光晕。 */
function background(ctx, img) {
  ctx.fillStyle = BG;
  ctx.fillRect(0, 0, W, H);
  if (img) {
    ctx.save();
    if ('filter' in ctx) ctx.filter = 'blur(48px) saturate(1.2)';
    cover(ctx, img, -80, -80, W + 160, H + 160);
    ctx.restore();
    const shade = ctx.createLinearGradient(0, 0, 0, H);
    shade.addColorStop(0, 'rgba(16,16,20,.55)');
    shade.addColorStop(0.55, 'rgba(16,16,20,.88)');
    shade.addColorStop(1, BG);
    ctx.fillStyle = shade;
    ctx.fillRect(0, 0, W, H);
  } else {
    const glow = ctx.createRadialGradient(160, 120, 0, 160, 120, 900);
    glow.addColorStop(0, 'rgba(226,96,93,.35)');
    glow.addColorStop(1, 'rgba(226,96,93,0)');
    ctx.fillStyle = glow;
    ctx.fillRect(0, 0, W, H);
  }
}

/** 底部：站点图标、名字和二维码。 */
function footer(ctx, card, logo) {
  ctx.fillStyle = 'rgba(255,255,255,.08)';
  ctx.fillRect(PAD, FOOT - 36, W - PAD * 2, 2);
  if (logo) cover(ctx, logo, PAD, FOOT + 40, 88, 88, 20);
  ctx.font = font(40, 700);
  ctx.fillStyle = FG;
  ctx.fillText(card.site || '影牛', PAD + 112, FOOT + 80);
  ctx.font = font(26);
  ctx.fillStyle = DIM;
  ctx.fillText('扫码查看 · 记录你看过的每一部', PAD + 112, FOOT + 122);
  // 二维码：白底，四周留白
  const size = 200, x = W - PAD - size, y = FOOT;
  ctx.fillStyle = '#fff';
  roundRect(ctx, x, y, size, size, 16);
  ctx.fill();
  try {
    const modules = encodeQR(card.url, 'M');
    const inner = size - 28, cell = inner / modules.length;
    ctx.fillStyle = '#000';
    modules.forEach((row, r) => row.forEach((dark, c) => {
      if (!dark) return;
      const left = Math.round(x + 14 + c * cell), top = Math.round(y + 14 + r * cell);
      ctx.fillRect(left, top, Math.round(x + 14 + (c + 1) * cell) - left, Math.round(y + 14 + (r + 1) * cell) - top);
    }));
  } catch { /* 地址太长放不进二维码时只留白底 */ }
}

function shadowed(ctx, draw) {
  ctx.save();
  ctx.shadowColor = 'rgba(0,0,0,.55)';
  ctx.shadowBlur = 48;
  ctx.shadowOffsetY = 20;
  draw();
  ctx.restore();
}

const layouts = {
  // 作品：大海报 + 片名、类型、评分、简介
  media(ctx, card, images) {
    const [poster] = images;
    background(ctx, poster);
    const pw = 480, ph = 720, px = (W - pw) / 2;
    shadowed(ctx, () => { ctx.fillStyle = '#000'; roundRect(ctx, px, 88, pw, ph, 28); ctx.fill(); });
    cover(ctx, poster, px, 88, pw, ph, 28);
    let y = 860;
    if (card.kicker) y = lines(ctx, card.kicker, W / 2, y, { size: 30, color: DIM, max: 1, align: 'center' }) + 8;
    y = lines(ctx, card.title, W / 2, y, { size: 68, weight: 700, max: 2, height: 84, align: 'center' }) + 10;
    const meta = [card.badge, card.sub].filter(Boolean).join('  ·  ');
    if (meta) y = lines(ctx, meta, W / 2, y, { size: 32, color: card.badge ? BRAND : DIM, weight: 600, max: 1, align: 'center' }) + 12;
    if (card.text) lines(ctx, card.text, W / 2, y, { size: 30, color: DIM, max: Math.max(1, Math.floor((FOOT - 60 - y) / 46)), height: 46, align: 'center' });
  },
  // 月度报告：身份、四个数字、海报墙
  report(ctx, card, images) {
    background(ctx, null);
    let y = 100;
    y = lines(ctx, card.kicker, PAD, y, { size: 32, color: DIM, max: 1 }) + 36;
    y = lines(ctx, '本月身份', PAD, y, { size: 30, color: BRAND, weight: 600, max: 1 });
    y = lines(ctx, card.title, PAD, y, { size: 92, weight: 800, max: 2, height: 112 }) + 4;
    y = lines(ctx, card.text, PAD, y, { size: 34, color: DIM, max: 2, height: 52 }) + 36;
    const stats = (card.stats ?? []).slice(0, 4), gap = 24, bw = (W - PAD * 2 - gap) / 2, bh = 150;
    stats.forEach(([value, label], i) => {
      const bx = PAD + (i % 2) * (bw + gap), by = y + Math.floor(i / 2) * (bh + gap);
      ctx.fillStyle = 'rgba(255,255,255,.06)';
      roundRect(ctx, bx, by, bw, bh, 24);
      ctx.fill();
      lines(ctx, String(value), bx + 32, by + 22, { size: 64, weight: 800, max: 1, width: bw - 64 });
      lines(ctx, label, bx + 32, by + 96, { size: 28, color: DIM, max: 1, width: bw - 64 });
    });
    y += Math.ceil(stats.length / 2) * (bh + gap) + 12;
    const posters = images.slice(0, 5), cw = (W - PAD * 2 - 4 * 18) / 5, ch = Math.min(cw * 1.5, FOOT - 70 - y);
    if (ch > 120) posters.forEach((img, i) => cover(ctx, img, PAD + i * (cw + 18), y, cw, ch, 14));
  },
  // 片单：标题、简介、海报网格
  list(ctx, card, images) {
    background(ctx, images[0]);
    let y = 100;
    y = lines(ctx, card.kicker || '片单', PAD, y, { size: 30, color: BRAND, weight: 600, max: 1 }) + 6;
    y = lines(ctx, card.title, PAD, y, { size: 76, weight: 800, max: 2, height: 92 }) + 8;
    if (card.text) y = lines(ctx, card.text, PAD, y, { size: 32, color: DIM, max: 3, height: 48 });
    if (card.sub) y = lines(ctx, card.sub, PAD, y + 4, { size: 28, color: DIM, max: 1 });
    y += 32;
    const cols = 5, gap = 20, cw = (W - PAD * 2 - gap * (cols - 1)) / cols, ch = cw * 1.5;
    images.slice(0, 10).forEach((img, i) => {
      const top = y + Math.floor(i / cols) * (ch + gap);
      if (top + ch <= FOOT - 60) cover(ctx, img, PAD + (i % cols) * (cw + gap), top, cw, ch, 14);
    });
  },
  // 短评：引号 + 正文 + 作者 + 作品
  comment(ctx, card, images) {
    const [poster] = images;
    background(ctx, poster);
    ctx.font = font(220, 800);
    ctx.fillStyle = BRAND;
    ctx.fillText('“', PAD - 10, 300);
    let y = lines(ctx, card.text, PAD, 300, { size: 46, weight: 500, max: 9, height: 72 }) + 28;
    if (card.badge) y = lines(ctx, card.badge, PAD, y, { size: 40, color: BRAND, max: 1 }) + 4;
    y = lines(ctx, '— ' + (card.author || ''), PAD, y, { size: 34, color: DIM, max: 1 }) + 40;
    if (y + 180 < FOOT - 50) {
      cover(ctx, poster, PAD, y, 120, 180, 14);
      lines(ctx, '《' + card.title + '》', PAD + 148, y + 50, { size: 38, weight: 700, max: 2, width: W - PAD * 2 - 148, height: 52 });
    }
  },
};

/** 画出卡片，返回 JPEG Blob。 */
export async function drawCard(card) {
  const canvas = document.createElement('canvas');
  canvas.width = W;
  canvas.height = H;
  const ctx = canvas.getContext('2d');
  const sources = card.kind === 'report' || card.kind === 'list' ? card.posters ?? [] : [card.poster];
  const [logo, ...images] = await Promise.all([loadImage('/static/img/apple-touch-icon.png'), ...sources.map(loadImage)]);
  await document.fonts?.ready;
  (layouts[card.kind] ?? layouts.media)(ctx, card, images);
  footer(ctx, card, logo);
  return new Promise((resolve, reject) => canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error('生成图片失败'))), 'image/jpeg', 0.9));
}

let sheet;

function ensureSheet() {
  if (sheet) return sheet;
  sheet = document.createElement('dialog');
  sheet.className = 'sheet share-sheet';
  sheet.innerHTML = `<div class="sheet-grab"></div>
<div class="share-preview"><span class="spinner" aria-hidden="true"></span><img alt="分享图片预览" hidden></div>
<p class="hint share-tip">长按图片可以保存，或直接发给朋友</p>
<div class="share-actions">
<button class="btn btn-primary" type="button" data-act="image" hidden>分享图片</button>
<a class="btn btn-glass" data-act="save" download>保存图片</a>
<button class="btn btn-glass" type="button" data-act="copy">复制链接</button>
<button class="btn btn-glass" type="button" data-act="system" hidden>更多…</button>
</div>`;
  sheet.addEventListener('click', (ev) => { if (ev.target === sheet) sheet.close(); });
  sheet.addEventListener('close', () => {
    const img = sheet.querySelector('img');
    if (img.src) URL.revokeObjectURL(img.src);
    img.removeAttribute('src');
  });
  document.body.append(sheet);
  return sheet;
}

/** 打开分享面板：生成卡片图片，提供分享图片、保存、复制链接。 */
export async function openShare(card) {
  card = { ...card, url: new URL(card.url || location.pathname, location.href).href, site: card.site || document.querySelector('.brand-t')?.textContent };
  const dialog = ensureSheet();
  const img = dialog.querySelector('img'), spinner = dialog.querySelector('.spinner');
  const save = dialog.querySelector('[data-act="save"]'), shareImage = dialog.querySelector('[data-act="image"]');
  const system = dialog.querySelector('[data-act="system"]');
  img.hidden = true;
  spinner.hidden = false;
  save.removeAttribute('href');
  shareImage.hidden = true;
  system.hidden = !navigator.share;
  dialog.querySelector('[data-act="copy"]').onclick = () => navigator.clipboard?.writeText(card.url).then(() => toast('链接已复制'), () => toast(card.url));
  system.onclick = () => navigator.share({ title: document.title, url: card.url }).catch(() => {});
  dialog.showModal();
  let blob;
  try {
    blob = await drawCard(card);
  } catch {
    spinner.hidden = true;
    toast('图片生成失败，可以先复制链接');
    return;
  }
  const url = URL.createObjectURL(blob);
  img.src = url;
  img.hidden = false;
  spinner.hidden = true;
  const name = (card.name || 'moovie') + '.jpg';
  save.href = url;
  save.download = name;
  const file = new File([blob], name, { type: 'image/jpeg' });
  if (navigator.canShare?.({ files: [file] })) {
    shareImage.hidden = false;
    shareImage.onclick = () => navigator.share({ files: [file], title: card.title }).catch(() => {});
  }
}
