const form = document.querySelector('#create-form');
const folderInput = document.querySelector('#folder');
const titleInput = document.querySelector('#title');
const channelSelect = document.querySelector('#channel');
const submitButton = document.querySelector('#submit');
const configError = document.querySelector('#config-error');
const connection = document.querySelector('#connection');
const jobList = document.querySelector('#job-list');
const empty = document.querySelector('#empty');
const template = document.querySelector('#job-template');
const toast = document.querySelector('#toast');
const telegramList = document.querySelector('#telegram-list');
const telegramEmpty = document.querySelector('#telegram-empty');
const telegramTemplate = document.querySelector('#telegram-template');
const telegramToken = document.querySelector('#telegram-token');
const telegramDialog = document.querySelector('#telegram-dialog');
const telegramForm = document.querySelector('#telegram-form');
const telegramMediaGrid = document.querySelector('#telegram-media-grid');
const siteSection = document.querySelector('#site-section');
const siteBody = document.querySelector('#site-body');
const siteGrid = document.querySelector('#site-grid');
const siteTemplate = document.querySelector('#site-template');
const sitePicker = document.querySelector('#site-picker');
const siteToggle = document.querySelector('#site-toggle');
const siteQueryInput = document.querySelector('#site-query');
const siteCategorySelect = document.querySelector('#site-category');
const siteStatus = document.querySelector('#site-status');
const siteRefreshButton = document.querySelector('#site-refresh');
const siteImportButton = document.querySelector('#site-import');
const siteEmpty = document.querySelector('#site-empty');
const sitePager = document.querySelector('.site-pager');
const sitePageLabel = document.querySelector('#site-page');
const sitePrevButton = document.querySelector('#site-prev');
const siteNextButton = document.querySelector('#site-next');
const siteSelectAll = document.querySelector('#site-select-all');
let toastTimer;
let lastJobs = '';
let lastTelegramImports = '';
let telegramChannels = [];
let requestedTelegramDraft = new URLSearchParams(location.search).get('telegram_import');
let siteID = '';
let siteQuery = '';
let siteCategory = '';
let sitePage = 1;
let sitePageCount = 1;
let siteReady = false;
let siteBusy = false;
let lastSiteTotal = -1;
let lastSiteReload = 0;
let lastSiteImporting = -1;
let siteReloadWanted = false;
let siteQueryTimer;
let lastSiteCategories = '';
let lastSiteAlbums = '';
let lastSiteOptions = '';
let lastSitePayload = null;
let sitePageIDs = [];
const sitePicked = new Set();
// Galleries whose real image count has already been asked for, so a poll every
// two seconds does not measure the same page over and over.
const siteMeasured = new Set();
let siteMeasuring = false;

// The region holds a page of hotlinked covers, so it is worth being able to fold
// it away. The choice is remembered because it is a working preference, not a
// one-off click.
const siteCollapsedKey = 'xrw.siteCollapsed';
let siteCollapsed = readSiteCollapsed();

function readSiteCollapsed() {
  try {
    return localStorage.getItem(siteCollapsedKey) === '1';
  } catch (cause) {
    return false;
  }
}

function notify(message) {
  toast.textContent = message;
  toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { toast.hidden = true; }, 4200);
}

async function api(path, options = {}) {
  const response = await fetch(path, options);
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `请求失败：${response.status}`);
  return body;
}

function splitTags(value) {
  return [...new Set(value.split(/[\n,，]+/).map((tag) => tag.trim().replace(/^#+/, '')).filter(Boolean))];
}

function humanBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / (1024 ** index)).toFixed(index > 1 ? 1 : 0)} ${units[index]}`;
}

function fillChannels(select, chats, current = '') {
  select.replaceChildren(...chats.map((chat, index) => {
    const option = document.createElement('option');
    option.value = chat;
    option.textContent = `频道 ${index + 1} · ${chat}`;
    return option;
  }));
  if (chats.includes(current)) select.value = current;
}

const statusNames = {
  pending: '等待上传', uploading: '正在上传', failed: '上传中断', ready: '快照就绪'
};

function renderJobs(jobs) {
  const signature = JSON.stringify(jobs);
  if (signature === lastJobs) return;
  lastJobs = signature;
  jobList.replaceChildren();
  empty.hidden = jobs.length > 0;
  for (const job of jobs) {
    const card = template.content.firstElementChild.cloneNode(true);
    card.querySelector('.job-id').textContent = job.id;
    card.querySelector('h3').textContent = job.title;
    card.querySelector('.job-folder').textContent = job.folder;
    card.querySelector('.job-folder').title = job.folder;
    const badge = card.querySelector('.badge');
    badge.textContent = statusNames[job.status] || job.status;
    badge.classList.add(job.status);
    const percent = job.total_files ? Math.round(job.uploaded_files / job.total_files * 100) : 0;
    const progress = card.querySelector('.progress');
    progress.setAttribute('aria-valuemin', '0');
    progress.setAttribute('aria-valuemax', String(job.total_files));
    progress.setAttribute('aria-valuenow', String(job.uploaded_files));
    progress.querySelector('i').style.width = `${percent}%`;
    card.querySelector('.job-count').textContent = `${job.uploaded_files} / ${job.total_files} 张 · ${percent}%`;
    card.querySelector('.job-size').textContent = humanBytes(job.total_bytes);
    card.querySelector('.job-channel').textContent = job.channel_id;
    const error = card.querySelector('.job-error');
    if (job.last_error) {
      error.hidden = false;
      error.textContent = job.last_error;
    }
    const retry = card.querySelector('.retry');
    if (job.status === 'failed') {
      retry.hidden = false;
      retry.addEventListener('click', async () => {
        retry.disabled = true;
        try {
          await api(`/api/jobs/${encodeURIComponent(job.id)}/retry`, { method: 'POST' });
          notify('任务已重新进入上传队列');
          await refresh();
        } catch (cause) {
          notify(cause.message);
          retry.disabled = false;
        }
      });
    }
    if (job.snapshot_path) card.querySelector('.snapshot').textContent = `快照：${job.snapshot_path}`;
    const thumbs = card.querySelector('.thumbs');
    for (let position = 1; position <= Math.min(job.total_files, 4); position += 1) {
      const image = new Image();
      image.loading = 'lazy';
      image.alt = '';
      image.src = `/api/jobs/${encodeURIComponent(job.id)}/thumbnail/${position}`;
      thumbs.append(image);
    }
    jobList.append(card);
  }
}

// announceFilledImports turns the end of a site import into a one-off message.
// A draft holds zero files until its very last image lands, so leaving the
// "filling" state is the only moment the page can tell the import is complete.
const fillingDrafts = new Map();

function announceFilledImports(imports) {
  const filling = new Map();
  for (const draft of imports) {
    if (draft.importing) filling.set(draft.id, draft.title || draft.id);
  }
  for (const [id, title] of fillingDrafts) {
    if (filling.has(id)) continue;
    const done = imports.find((draft) => draft.id === id);
    if (done && done.file_count > 0) notify(`导入完成：${title}（${done.file_count} 张）`);
    else notify(`导入未完成：${title}`);
  }
  fillingDrafts.clear();
  for (const [id, title] of filling) fillingDrafts.set(id, title);
}

function renderTelegramImports(imports) {
  announceFilledImports(imports);
  const signature = JSON.stringify(imports);
  if (signature === lastTelegramImports) return;
  lastTelegramImports = signature;
  telegramList.replaceChildren();
  telegramEmpty.hidden = imports.length > 0;
  for (const draft of imports) {
    const card = telegramTemplate.content.firstElementChild.cloneNode(true);
    card.querySelector('.telegram-id').textContent = draft.id;
    card.querySelector('h3').textContent = draft.title;
    const source = card.querySelector('.telegram-source');
    const origin = draft.kind === 'wp' ? '站点图包' : 'Telegram Web';
    source.textContent = draft.source_url ? `${origin} · ${draft.source_url}` : origin;
    source.title = draft.source_url || '';
    const summary = card.querySelector('.telegram-summary');
    if (draft.importing) {
      summary.textContent = draft.target_count ? `抓取中… 目标 ${draft.target_count} 张` : '抓取中…';
      summary.classList.add('filling');
    } else {
      summary.textContent = draft.committed_job_id
        ? `${draft.file_count} 张 · 已建立任务 ${draft.committed_job_id}`
        : `${draft.file_count} 张 · ${humanBytes(draft.total_bytes)}`;
    }
    const button = card.querySelector('.edit-telegram');
    button.textContent = draft.committed_job_id ? '查看草稿' : '检查并导入';
    button.disabled = Boolean(draft.importing);
    button.addEventListener('click', () => openTelegramImport(draft.id));
    const deleteButton = card.querySelector('.delete-telegram');
    deleteButton.hidden = Boolean(draft.committed_job_id) || Boolean(draft.importing);
    deleteButton.addEventListener('click', async () => {
      if (!window.confirm(`删除草稿“${draft.title}”及其 ${draft.file_count} 张本地暂存图片？`)) return;
      deleteButton.disabled = true;
      try {
        await api(`/api/telegram-imports/${encodeURIComponent(draft.id)}`, { method: 'DELETE' });
        notify('草稿及本地暂存图片已删除');
        lastTelegramImports = '';
        await refresh();
      } catch (cause) {
        notify(cause.message);
        deleteButton.disabled = false;
      }
    });
    telegramList.append(card);
  }
}

function updateTelegramSelectionCount() {
  const items = [...telegramMediaGrid.children];
  const kept = items.filter((item) => !item.classList.contains('excluded')).length;
  document.querySelector('#telegram-selection-count').textContent = `保留 ${kept} / ${items.length} 张`;
  document.querySelector('#commit-telegram').disabled = kept === 0;
}

function renderTelegramFiles(draft) {
  telegramMediaGrid.replaceChildren();
  for (const file of draft.files || []) {
    const item = document.createElement('article');
    item.className = 'telegram-media-item';
    item.dataset.fileId = file.id;
    item.draggable = true;
    item.innerHTML = `
      <img loading="lazy" alt="">
      <button type="button" class="telegram-media-toggle">排除</button>
      <span>${file.position}</span>
    `;
    const image = item.querySelector('img');
    const toggle = item.querySelector('.telegram-media-toggle');
    image.src = `/api/telegram-imports/${encodeURIComponent(draft.id)}/files/${encodeURIComponent(file.id)}`;
    image.alt = file.name;
    const details = `${file.name}\n${file.width} × ${file.height} · ${humanBytes(file.size)}`;
    const syncSelectionState = () => {
      const excluded = item.classList.contains('excluded');
      toggle.textContent = excluded ? '恢复' : '排除';
      toggle.setAttribute('aria-label', `${excluded ? '恢复' : '排除'} ${file.name}`);
      item.title = `${details}\n点击图片${excluded ? '恢复' : '排除'}`;
    };
    const toggleSelection = () => {
      item.classList.toggle('excluded');
      syncSelectionState();
      updateTelegramSelectionCount();
    };
    image.addEventListener('click', toggleSelection);
    toggle.addEventListener('click', toggleSelection);
    syncSelectionState();
    item.addEventListener('dragstart', (event) => {
      event.dataTransfer.effectAllowed = 'move';
      event.dataTransfer.setData('text/plain', file.id);
      item.classList.add('dragging');
    });
    item.addEventListener('dragend', () => item.classList.remove('dragging'));
    item.addEventListener('dragover', (event) => {
      event.preventDefault();
      const dragging = telegramMediaGrid.querySelector('.dragging');
      if (!dragging || dragging === item) return;
      const bounds = item.getBoundingClientRect();
      const after = event.clientX > bounds.left + bounds.width / 2;
      telegramMediaGrid.insertBefore(dragging, after ? item.nextSibling : item);
    });
    telegramMediaGrid.append(item);
  }
  updateTelegramSelectionCount();
}

async function openTelegramImport(id) {
  try {
    const draft = await api(`/api/telegram-imports/${encodeURIComponent(id)}`);
    document.querySelector('#telegram-id').value = draft.id;
    document.querySelector('#telegram-edit-title').value = draft.title || '';
    document.querySelector('#telegram-edit-category').value = draft.category || '';
    document.querySelector('#telegram-edit-tags').value = (draft.tags || []).join('\n');
    document.querySelector('#telegram-dialog-source').textContent = draft.source_url
      ? `来源：${draft.source_url}` : '来源：Telegram Web';
    fillChannels(document.querySelector('#telegram-edit-channel'), telegramChannels);
    renderTelegramFiles(draft);
    document.querySelector('#commit-telegram').hidden = Boolean(draft.committed_job_id);
    if (typeof telegramDialog.showModal === 'function') telegramDialog.showModal();
    else telegramDialog.setAttribute('open', '');
  } catch (cause) {
    notify(cause.message);
  }
}

function syncSiteImportButton() {
  const count = sitePicked.size;
  siteImportButton.disabled = count === 0;
  siteImportButton.textContent = count ? `导入所选 ${count} 个` : '导入所选';
  syncSiteSelectAll();
}

// The header checkbox mirrors the selection of the current page: checked when
// every selectable card is picked, indeterminate on a partial selection.
function syncSiteSelectAll() {
  const total = sitePageIDs.length;
  const picked = sitePageIDs.filter((id) => sitePicked.has(id)).length;
  siteSelectAll.disabled = total === 0;
  siteSelectAll.checked = total > 0 && picked === total;
  siteSelectAll.indeterminate = picked > 0 && picked < total;
}

// Refresh failures carry a full upstream URL; keep the inline line readable.
function clipText(value, limit) {
  const text = String(value || '').trim();
  return text.length > limit ? `${text.slice(0, limit - 1)}…` : text;
}

function renderSiteStatus(statuses) {
  const sites = Array.isArray(statuses) ? statuses : [];
  siteSection.hidden = sites.length === 0;
  if (!sites.length) return;
  renderSitePicker(sites);
  if (!sites.some((item) => item.id === siteID)) selectSite(sites[0].id);

  const status = sites.find((item) => item.id === siteID) || sites[0];
  if (!siteReady && !siteCollapsed) {
    siteReady = true;
    loadSiteAlbums();
  }
  siteStatus.title = status.error || '';
  if (status.building) {
    const reading = status.progress || '正在更新列表…';
    // A big site is walked page by page, so say how far the index has got.
    siteStatus.textContent = status.total ? `${reading} · 已读到 ${status.total} 个` : reading;
    siteRefreshButton.disabled = true;
  } else {
    siteRefreshButton.disabled = false;
    const when = status.built_at
      ? new Date(status.built_at * 1000).toLocaleString('zh-CN', { hour12: false })
      : '尚未建立';
    const parts = [`共 ${status.total || 0} 个图包`, `列表更新于 ${when}`];
    if (status.importing) parts.push(`${status.importing} 个正在导入`);
    if (status.error) parts.push(`上次失败：${clipText(status.error, 120)}`);
    siteStatus.textContent = parts.join(' · ');
  }
  // The grid grows while a refresh walks a big site, and imports carry their
  // progress on the cards, so both want the list reloaded as they move. A rebuild
  // keeps the old albums until they are read again, so there the count does not
  // move and the list is picked up on a slow tick instead. The pass that watches
  // the last import end owes one more reload: without it the finished card keeps
  // saying 抓取中… until something else touches the grid.
  const importing = status.importing || 0;
  const finished = lastSiteImporting > 0 && importing === 0;
  lastSiteImporting = importing;
  const grown = status.building && status.total !== lastSiteTotal;
  lastSiteTotal = status.total;
  const ticking = status.building && Date.now() - lastSiteReload > 15000;
  if (ticking) lastSiteReload = Date.now();
  // A reload asked for while one was already running still has to happen, so the
  // request survives until a load actually starts (see loadSiteAlbums).
  if (importing || finished || grown || ticking) siteReloadWanted = true;
  if (siteReloadWanted && !siteCollapsed) loadSiteAlbums();
}

// The picker only appears once there is something to switch between, so a single
// site setup keeps the plain heading it had before.
function renderSitePicker(sites) {
  const signature = JSON.stringify(sites.map((item) => [item.id, item.name]));
  if (signature !== lastSiteOptions) {
    lastSiteOptions = signature;
    sitePicker.replaceChildren(...sites.map((item) => new Option(item.name, item.id)));
  }
  sitePicker.hidden = sites.length < 2;
  sitePicker.value = siteID;
}

// Switching sites resets the filters: a category id from one site means nothing
// on another, and a leftover query would just show an empty grid.
function selectSite(id) {
  if (id === siteID) return;
  siteID = id;
  siteQuery = '';
  siteCategory = '';
  sitePage = 1;
  siteQueryInput.value = '';
  siteCategorySelect.value = '';
  lastSiteCategories = '';
  lastSiteAlbums = '';
  lastSitePayload = null;
  siteMeasured.clear();
  sitePageIDs = [];
  sitePicked.clear();
  siteGrid.replaceChildren();
  siteReady = false;
  syncSiteImportButton();
}

function applySiteCollapsed() {
  siteBody.hidden = siteCollapsed;
  siteToggle.textContent = siteCollapsed ? '展开' : '收起';
  siteToggle.setAttribute('aria-expanded', String(!siteCollapsed));
}

function renderSiteCategories(categories) {
  const signature = JSON.stringify(categories);
  if (signature === lastSiteCategories) return;
  lastSiteCategories = signature;
  const options = [new Option('全部分类', '')];
  for (const term of categories) options.push(new Option(`${term.name} · ${term.count}`, String(term.id)));
  siteCategorySelect.replaceChildren(...options);
  siteCategorySelect.value = siteCategory;
}

function renderSiteAlbums(payload) {
  const albums = payload.albums || [];
  lastSitePayload = payload;
  const perPage = payload.per_page || 48;
  sitePageCount = Math.max(1, Math.ceil((payload.total || 0) / perPage));
  sitePage = payload.page || 1;
  sitePager.hidden = sitePageCount <= 1;
  sitePageLabel.textContent = `第 ${sitePage} / ${sitePageCount} 页 · 命中 ${payload.total || 0} 个图包`;
  sitePrevButton.disabled = sitePage <= 1;
  siteNextButton.disabled = sitePage >= sitePageCount;

  const signature = JSON.stringify(albums);
  if (signature === lastSiteAlbums) {
    syncSiteImportButton();
    return;
  }
  lastSiteAlbums = signature;
  siteGrid.replaceChildren();
  siteEmpty.hidden = albums.length > 0;
  siteEmpty.textContent = payload.total ? '这一页没有内容。' : '没有匹配的图包。';

  const selectable = [];
  for (const album of albums) {
    const card = siteTemplate.content.firstElementChild.cloneNode(true);
    const pick = card.querySelector('.site-pick');
    const image = card.querySelector('img');
    if (album.cover_url) {
      image.src = album.cover_url;
      image.alt = album.title || album.slug || '';
    } else {
      image.remove();
    }
    card.querySelector('h3').textContent = album.title || album.slug || `#${album.id}`;
    const bits = [];
    // Once the post itself has been read, the real image count is shown next to
    // the one the title advertises: a lot of galleries only publish a preview.
    if (album.photos) {
      const short = album.actual && album.actual < album.photos;
      bits.push(short ? `${album.actual} / ${album.photos} 张` : `${album.photos} 张`);
    } else if (album.actual) {
      bits.push(`${album.actual} 张`);
    }
    if (album.videos) bits.push(`${album.videos} 个视频`);
    if (album.date) bits.push(album.date.slice(0, 10));
    card.querySelector('.site-meta').textContent = bits.join(' · ');

    const flag = card.querySelector('.site-flag');
    if (album.actual && album.photos && album.actual < album.photos) {
      flag.hidden = false;
      flag.textContent = '不全';
    }

    const badge = card.querySelector('.site-badge');
    if (album.committed_job_id) {
      badge.textContent = '已发布';
      pick.disabled = true;
      sitePicked.delete(album.id);
    } else if (album.importing) {
      badge.textContent = '抓取中…';
      pick.disabled = true;
      sitePicked.delete(album.id);
    } else if (album.draft_id) {
      badge.textContent = '导入完成';
    } else {
      badge.textContent = '未导入';
    }
    if (!pick.disabled) {
      selectable.push(album.id);
      pick.checked = sitePicked.has(album.id);
      pick.addEventListener('change', () => {
        if (pick.checked) sitePicked.add(album.id);
        else sitePicked.delete(album.id);
        syncSiteImportButton();
      });
    }

    const error = card.querySelector('.site-error');
    if (album.error) {
      error.hidden = false;
      error.textContent = album.error;
    }
    const link = card.querySelector('.site-link');
    link.href = album.link || '#';
    link.hidden = !album.link;
    const open = card.querySelector('.site-open');
    if (album.draft_id) {
      open.hidden = false;
      open.textContent = album.committed_job_id ? '查看草稿' : '检查并导入';
      open.addEventListener('click', () => openTelegramImport(album.draft_id));
    }
    siteGrid.append(card);
  }
  sitePageIDs = selectable;
  syncSiteImportButton();
  measureSiteAlbums(payload);
}

// measureSiteAlbums asks the backend how many images the galleries on screen
// really carry. The index only knows the number in the title, which on some
// sites is several times the real one, so the count only becomes trustworthy
// after the posts themselves have been read. It is a nicety, not a requirement:
// a failure leaves the titles' numbers in place and a later render retries.
async function measureSiteAlbums(payload) {
  if (siteMeasuring || !siteID) return;
  const status = (payload.status || []).find((entry) => entry.id === siteID);
  if (status && status.building) return;
  const wanted = (payload.albums || [])
    .filter((album) => !album.actual && !siteMeasured.has(album.id))
    .map((album) => album.id);
  if (!wanted.length) return;
  for (const id of wanted) siteMeasured.add(id);
  siteMeasuring = true;
  try {
    const result = await api('/api/site-albums/verify', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ site: siteID, post_ids: wanted })
    });
    const actual = result.actual || {};
    let changed = false;
    for (const album of payload.albums) {
      if (Object.prototype.hasOwnProperty.call(actual, album.id)) {
        album.actual = actual[album.id];
        changed = true;
      }
    }
    if (changed) {
      lastSiteAlbums = '';
      renderSiteAlbums(payload);
    }
  } catch (cause) {
    for (const id of wanted) siteMeasured.delete(id);
  } finally {
    siteMeasuring = false;
  }
}

async function loadSiteAlbums() {
  if (siteBusy || siteSection.hidden || siteCollapsed || !siteID) return;
  siteBusy = true;
  siteReloadWanted = false;
  try {
    const params = new URLSearchParams({ site: siteID });
    if (siteQuery) params.set('q', siteQuery);
    if (siteCategory) params.set('category', siteCategory);
    if (sitePage > 1) params.set('page', String(sitePage));
    const payload = await api(`/api/site-albums?${params.toString()}`);
    renderSiteCategories(payload.categories || []);
    renderSiteAlbums(payload);
  } catch (cause) {
    notify(cause.message);
  } finally {
    siteBusy = false;
  }
}

function renderState(state) {
  connection.textContent = state.configured ? '本地服务已连接' : '等待配置';
  connection.classList.toggle('ready', state.configured);
  configError.hidden = state.configured;
  configError.textContent = state.configured ? '' : `配置未完成：${state.configuration_error}`;
  submitButton.disabled = !state.configured;
  telegramChannels = state.chat_ids || [];
  fillChannels(channelSelect, telegramChannels, channelSelect.value);
  telegramToken.textContent = state.telegram_import_token || '连接码不可用';
  renderTelegramImports(state.telegram_imports || []);
  renderJobs(state.jobs || []);
  renderSiteStatus(state.site_albums);
  if (requestedTelegramDraft && (state.telegram_imports || []).some((draft) => draft.id === requestedTelegramDraft)) {
    const id = requestedTelegramDraft;
    requestedTelegramDraft = '';
    openTelegramImport(id);
  }
}

async function refresh() {
  try {
    renderState(await api('/api/state'));
  } catch {
    connection.textContent = '本地服务连接失败';
    connection.classList.remove('ready');
  }
}

document.querySelector('#pick-folder').addEventListener('click', async (event) => {
  event.currentTarget.disabled = true;
  try {
    const result = await api('/api/folders/pick', { method: 'POST' });
    if (result.path) {
      folderInput.value = result.path;
      if (!titleInput.value.trim()) titleInput.value = result.path.split(/[\\/]/).filter(Boolean).at(-1) || '';
    }
  } catch (cause) {
    notify(cause.message);
  } finally {
    event.currentTarget.disabled = false;
  }
});

document.querySelector('#open-output').addEventListener('click', async () => {
  try {
    await api('/api/snapshots/open', { method: 'POST' });
  } catch (cause) {
    notify(cause.message);
  }
});

document.querySelector('#copy-telegram-token').addEventListener('click', async () => {
  const value = telegramToken.textContent.trim();
  if (!value || value.includes('等待') || value.includes('不可用')) return;
  try {
    await navigator.clipboard.writeText(value);
    notify('油猴脚本连接码已复制');
  } catch {
    window.prompt('复制下面的连接码到油猴脚本', value);
  }
});

document.querySelector('#close-telegram').addEventListener('click', () => telegramDialog.close());

siteQueryInput.addEventListener('input', () => {
  clearTimeout(siteQueryTimer);
  siteQueryTimer = setTimeout(() => {
    siteQuery = siteQueryInput.value.trim();
    sitePage = 1;
    lastSiteAlbums = '';
    loadSiteAlbums();
  }, 320);
});

siteCategorySelect.addEventListener('change', () => {
  siteCategory = siteCategorySelect.value;
  sitePage = 1;
  lastSiteAlbums = '';
  loadSiteAlbums();
});

sitePrevButton.addEventListener('click', () => {
  if (sitePage <= 1) return;
  sitePage -= 1;
  loadSiteAlbums();
});

siteNextButton.addEventListener('click', () => {
  if (sitePage >= sitePageCount) return;
  sitePage += 1;
  loadSiteAlbums();
});

sitePicker.addEventListener('change', () => {
  selectSite(sitePicker.value);
  loadSiteAlbums();
});

siteToggle.addEventListener('click', () => {
  siteCollapsed = !siteCollapsed;
  try {
    localStorage.setItem(siteCollapsedKey, siteCollapsed ? '1' : '0');
  } catch (cause) {
    // A blocked storage just means the choice does not survive a reload.
  }
  applySiteCollapsed();
  if (!siteCollapsed) loadSiteAlbums();
});

siteRefreshButton.addEventListener('click', async (event) => {
  const full = event.shiftKey;
  siteRefreshButton.disabled = true;
  try {
    const status = await api('/api/site-albums/refresh', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ site: siteID, full })
    });
    notify(full ? '正在重建整份图包列表' : '正在读取有更新的图包');
    renderSiteStatus(status);
  } catch (cause) {
    notify(cause.message);
    siteRefreshButton.disabled = false;
  }
});

siteSelectAll.addEventListener('change', () => {
  const checked = siteSelectAll.checked;
  for (const id of sitePageIDs) {
    if (checked) sitePicked.add(id);
    else sitePicked.delete(id);
  }
  // Reflect the new state on the visible cards without rebuilding the grid.
  for (const input of siteGrid.querySelectorAll('.site-pick')) {
    if (!input.disabled) input.checked = checked;
  }
  syncSiteImportButton();
});

siteImportButton.addEventListener('click', async () => {
  const postIDs = [...sitePicked];
  if (!postIDs.length) return;
  siteImportButton.disabled = true;
  try {
    const result = await api('/api/site-albums/import', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ site: siteID, post_ids: postIDs })
    });
    sitePicked.clear();
    lastSiteAlbums = '';
    notify(`已开始导入 ${result.accepted} 个图包，完成后会出现在“导入草稿”里`);
    await loadSiteAlbums();
    await refresh();
  } catch (cause) {
    notify(cause.message);
  } finally {
    syncSiteImportButton();
  }
});

telegramForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const button = document.querySelector('#commit-telegram');
  const fileIDs = [...telegramMediaGrid.children]
    .filter((item) => !item.classList.contains('excluded'))
    .map((item) => item.dataset.fileId);
  button.disabled = true;
  button.textContent = '正在建立上传任务…';
  try {
    const id = document.querySelector('#telegram-id').value;
    const job = await api(`/api/telegram-imports/${encodeURIComponent(id)}/commit`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        title: document.querySelector('#telegram-edit-title').value.trim(),
        category: document.querySelector('#telegram-edit-category').value.trim(),
        tags: splitTags(document.querySelector('#telegram-edit-tags').value),
        channel_id: document.querySelector('#telegram-edit-channel').value,
        file_ids: fileIDs
      })
    });
    notify(`已建立 ${job.total_files} 张图片的上传任务`);
    telegramDialog.close();
    await refresh();
  } catch (cause) {
    notify(cause.message);
  } finally {
    button.disabled = false;
    button.textContent = '建立任务并上传';
  }
});

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  submitButton.disabled = true;
  submitButton.textContent = '正在扫描图片…';
  try {
    const payload = {
      folder: folderInput.value.trim(),
      title: titleInput.value.trim(),
      category: document.querySelector('#category').value.trim(),
      tags: splitTags(document.querySelector('#tags').value),
      channel_id: channelSelect.value
    };
    const job = await api('/api/jobs', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
    });
    notify(`已建立 ${job.total_files} 张图片的上传任务`);
    form.reset();
    if (channelSelect.options.length) channelSelect.selectedIndex = 0;
    await refresh();
  } catch (cause) {
    notify(cause.message);
  } finally {
    submitButton.disabled = false;
    submitButton.textContent = '建立任务并上传';
  }
});

applySiteCollapsed();
refresh();
setInterval(refresh, 2000);
