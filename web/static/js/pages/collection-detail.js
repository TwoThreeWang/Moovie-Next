import { api } from '../core/api.js';

const view = document.querySelector('[data-collection-view]');
if (view) {
  document.addEventListener('visibilitychange', sendWhenVisible);
  document.addEventListener('prerenderingchange', sendWhenVisible);
  sendWhenVisible();
}

function sendWhenVisible() {
  if (document.visibilityState !== 'visible' || document.prerendering) return;
  document.removeEventListener('visibilitychange', sendWhenVisible);
  document.removeEventListener('prerenderingchange', sendWhenVisible);
  api(view.dataset.collectionView, { method: 'POST', keepalive: true }).catch(() => {});
}
