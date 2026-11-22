// iOS "添加到桌面"后的启动图。
//
// iOS 不看 manifest 里的背景色，页面没给启动图时，从桌面打开先是一片白。
// 启动图要和屏幕像素一样大，机型一多就是十几张；而且要写中文站名，服务器上没有中文字体。
// 所以在这台设备上用 canvas 现画一张：尺寸正好、用的是系统字体。用户添加到桌面时 iOS 会把它存下来，
// 之后每次从桌面打开都先显示它，直到页面画出来。已经添加过的要删掉重新添加才会用上。

const SLOGAN = '发现你的下一部电影';
const ICON = '/static/img/icon-512.png';
const FONT = 'system-ui, -apple-system, "PingFang SC", "Hiragino Sans GB", sans-serif';
// 和 app.css 的 --bg / --text / --text-2 / --ambient 一致，启动图消失时页面接得上
const BG = '#0a0a0c';
const TEXT = '#f5f5f7';
const TEXT_2 = 'rgba(245, 245, 247, .56)';
const GLOW = [42, 42, 53];

/**
 * 按屏幕尺寸（CSS 像素）排版：图标、站名、标语竖着居中，整体略偏上。纯函数，方便测试。
 * 返回各元素左上角坐标和大小，文字的 y 是文字顶端。
 */
export function splashLayout(width, height) {
  const short = Math.min(width, height);
  const logo = Math.round(Math.min(120, Math.max(88, short * 0.26)));
  const nameSize = Math.round(logo * 0.25);
  const sloganSize = Math.round(logo * 0.14);
  const gap1 = Math.round(logo * 0.22);
  const gap2 = Math.round(nameSize * 0.45);
  const block = logo + gap1 + nameSize + gap2 + sloganSize;
  const top = Math.round(height * 0.45 - block / 2);
  return {
    logo: { x: Math.round((width - logo) / 2), y: top, size: logo },
    name: { y: top + logo + gap1, size: nameSize },
    slogan: { y: top + logo + gap1 + nameSize + gap2, size: sloganSize },
  };
}

/** 画一张启动图，返回 canvas。width / height 是 CSS 像素，dpr 是屏幕像素比。 */
export function renderSplash(doc, { width, height, dpr, icon, site }) {
  const canvas = doc.createElement('canvas');
  canvas.width = Math.round(width * dpr);
  canvas.height = Math.round(height * dpr);
  const ctx = canvas.getContext('2d');
  ctx.scale(dpr, dpr);

  ctx.fillStyle = BG;
  ctx.fillRect(0, 0, width, height);
  // 顶部一团淡淡的光，和页面的氛围光一样
  const radius = Math.max(width * 1.2, height * 0.63) * 0.75;
  const glow = ctx.createRadialGradient(width / 2, 0, 0, width / 2, 0, radius);
  glow.addColorStop(0, `rgba(${GLOW}, .55)`);
  glow.addColorStop(1, `rgba(${GLOW}, 0)`);
  ctx.fillStyle = glow;
  ctx.fillRect(0, 0, width, height);

  const layout = splashLayout(width, height);
  ctx.save();
  // 阴影不跟着 scale 缩放，要自己乘像素比
  ctx.shadowColor = 'rgba(0, 0, 0, .45)';
  ctx.shadowBlur = 30 * dpr;
  ctx.shadowOffsetY = 10 * dpr;
  ctx.drawImage(icon, layout.logo.x, layout.logo.y, layout.logo.size, layout.logo.size);
  ctx.restore();

  ctx.textAlign = 'center';
  ctx.textBaseline = 'top';
  ctx.fillStyle = TEXT;
  ctx.font = `600 ${layout.name.size}px ${FONT}`;
  ctx.fillText(site, width / 2, layout.name.y, width - 48);

  ctx.fillStyle = TEXT_2;
  ctx.font = `400 ${layout.slogan.size}px ${FONT}`;
  // 字间距会加在最后一个字后面，居中时往右挪半个补回来
  const spacing = layout.slogan.size * 0.18;
  let x = width / 2;
  if ('letterSpacing' in ctx) {
    ctx.letterSpacing = `${spacing}px`;
    x += spacing / 2;
  }
  ctx.fillText(SLOGAN, x, layout.slogan.y, width - 48);
  return canvas;
}

function toDataURL(canvas) {
  // toBlob 在后台编码，不卡页面；iOS 的启动图只认能直接读到的地址，所以再转成 data URL
  return new Promise((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (!blob) return reject(new Error('启动图生成失败'));
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.onerror = () => reject(reader.error);
      reader.readAsDataURL(blob);
    }, 'image/png');
  });
}

/** 给当前页面加上启动图。只在 iOS 浏览器里、还没添加到桌面时调用。 */
export async function addStartupImages(doc = document, win = window) {
  if (doc.querySelector('link[rel="apple-touch-startup-image"]')) return;
  const site = doc.querySelector('meta[name="apple-mobile-web-app-title"]')?.content || doc.title;
  const dpr = win.devicePixelRatio || 1;
  // iOS 的 screen.width / height 不随横竖屏变化，保险起见自己分长短边
  const short = Math.min(win.screen.width, win.screen.height);
  const long = Math.max(win.screen.width, win.screen.height);
  const icon = new win.Image();
  icon.src = ICON;
  await icon.decode();
  const orientations = [['portrait', short, long]];
  if (short >= 600) orientations.push(['landscape', long, short]); // iPad 横着也能从桌面打开
  for (const [orientation, width, height] of orientations) {
    const href = await toDataURL(renderSplash(doc, { width, height, dpr, icon, site }));
    const link = doc.createElement('link');
    link.rel = 'apple-touch-startup-image';
    link.media = `screen and (orientation: ${orientation})`;
    link.href = href;
    doc.head.append(link);
  }
}
