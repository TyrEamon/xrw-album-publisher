// ==UserScript==
// @name         绮影志 · Telegram Web 图集导入
// @namespace    https://github.com/TyrEamon/xrw-album
// @version      0.2.4
// @description  在 Telegram Web 选择图集消息，并发送到本机 xrw-album 上传器检查。
// @match        https://web.telegram.org/*
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_registerMenuCommand
// @grant        GM_xmlhttpRequest
// @grant        GM_setClipboard
// @connect      127.0.0.1
// @connect      localhost
// @run-at       document-idle
// ==/UserScript==

(() => {
  'use strict';

  const scriptVersion = '0.2.4';
  const defaultLocalURL = 'http://127.0.0.1:8765';
  let localURL = String(GM_getValue('xrwLocalURL', defaultLocalURL)).replace(/\/$/, '');
  let importToken = String(GM_getValue('xrwImportToken', ''));
  let pickMode = false;
  let draft = null;
  let selectedMessages = 0;
  let uploadedImages = 0;
  let busy = false;
  let discussionTrigger = null;
  let selectedMainMediaCount = 0;
  const selectedKeys = new Set();
  const telegramMessageSelector = '.bubble, .message-list-item, [class*="message-list-item"], .Message';
  const maxDebugEntries = 300;
  let debugEntries = GM_getValue('xrwDebugEntries', []);
  if (!Array.isArray(debugEntries)) debugEntries = [];

  function debugLog(level, event, details = {}) {
    const entry = {
      time: new Date().toISOString(),
      level,
      event,
      details
    };
    debugEntries.push(entry);
    debugEntries = debugEntries.slice(-maxDebugEntries);
    GM_setValue('xrwDebugEntries', debugEntries);
    const method = level === 'error' ? 'error' : level === 'warn' ? 'warn' : 'log';
    console[method]('[XRW TG]', event, details);
  }

  function errorDetails(error) {
    return { message: error instanceof Error ? error.message : String(error) };
  }

  function copyDebugLog() {
    const lines = debugEntries.map((entry) => JSON.stringify(entry));
    GM_setClipboard(lines.join('\n') || '暂无调试日志', 'text');
    updatePanel(`已复制 ${debugEntries.length} 条调试日志。`);
  }

  const host = document.createElement('div');
  host.id = 'xrw-telegram-import';
  document.documentElement.append(host);
  const shadow = host.attachShadow({ mode: 'open' });
  shadow.innerHTML = `
    <style>
      :host { all: initial; }
      .panel {
        position: fixed; right: 18px; bottom: 18px; z-index: 2147483647; width: 280px;
        padding: 14px; border: 1px solid #554536; border-radius: 16px; color: #f6eee5;
        background: #15120ff2; box-shadow: 0 18px 60px #0009; backdrop-filter: blur(14px);
        font: 13px/1.45 system-ui, "Microsoft YaHei", sans-serif;
      }
      .head { display: flex; justify-content: space-between; align-items: center; gap: 12px; }
      .brand { color: #efc18f; font: 17px Georgia, "Songti SC", serif; }
      .version { margin-left: 5px; color: #7f7469; font: 10px/1 system-ui, sans-serif; }
      .head-actions { display: flex; align-items: center; gap: 6px; }
      .settings, .logs { min-height: auto; border: 0; padding: 2px 5px; color: #ab9d8e; background: transparent; cursor: pointer; font-size: 12px; }
      .settings { font-size: 16px; }
      .status { min-height: 38px; margin: 10px 0; color: #b8aa9b; overflow-wrap: anywhere; }
      .count { margin: 0 0 12px; color: #efc18f; font-size: 12px; }
      .actions { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
      .comments { grid-column: 1 / -1; }
      button {
        min-height: 36px; border: 1px solid #554536; border-radius: 999px; padding: 7px 11px;
        color: #f6eee5; background: #211b16; cursor: pointer; font: inherit;
      }
      button:hover { border-color: #d8a46e; }
      button.primary { color: #24170c; background: #efc18f; border-color: #efc18f; font-weight: 650; }
      button:disabled { opacity: .45; cursor: wait; }
      .hint { margin: 10px 0 0; color: #7f7469; font-size: 11px; }
    </style>
    <section class="panel">
      <div class="head"><span class="brand">绮影志 · TG 导入 <small class="version">v${scriptVersion}</small></span><span class="head-actions"><button class="logs" type="button" title="复制最近的调试日志">日志</button><button class="settings" type="button" title="连接设置">⚙</button></span></div>
      <div class="status">先启动本地上传器，再开启选择模式。</div>
      <p class="count">0 条消息 · 0 张图片</p>
      <div class="actions">
        <button class="primary toggle" type="button">开始选择</button>
        <button class="open" type="button" disabled>本地编辑</button>
        <button class="comments" type="button" disabled>读取该帖评论区图片</button>
        <button class="reset" type="button">新建草稿</button>
        <button class="hide" type="button">隐藏面板</button>
      </div>
      <p class="hint">选择主帖后可直接点 Comments 进入评论区，再继续选择完整图片消息。</p>
    </section>
  `;

  const panel = shadow.querySelector('.panel');
  const status = shadow.querySelector('.status');
  const count = shadow.querySelector('.count');
  const toggle = shadow.querySelector('.toggle');
  const open = shadow.querySelector('.open');
  const comments = shadow.querySelector('.comments');
  const reset = shadow.querySelector('.reset');
  const settings = shadow.querySelector('.settings');
  const logs = shadow.querySelector('.logs');

  const selectionStyle = document.createElement('style');
  selectionStyle.textContent = `
    [data-xrw-import-selected="true"] {
      outline: 3px solid #efb476 !important;
      outline-offset: 3px !important;
      border-radius: 10px !important;
    }
  `;
  document.head.append(selectionStyle);

  function updatePanel(message) {
    if (message) status.textContent = message;
    count.textContent = `${selectedMessages} 条消息 · ${uploadedImages} 张图片`;
    toggle.textContent = pickMode ? '结束选择' : '开始选择';
    toggle.classList.toggle('primary', !pickMode);
    open.disabled = !draft;
    for (const button of shadow.querySelectorAll('button')) {
      if (button.classList.contains('hide') || button.classList.contains('logs')) continue;
      button.disabled = busy || (button === open && !draft) || (button === comments && !discussionTrigger);
    }
  }

  function configure() {
    const nextURL = window.prompt('本地上传器地址', localURL || defaultLocalURL);
    if (nextURL === null) return false;
    const nextToken = window.prompt('粘贴本地上传器页面显示的“油猴脚本连接码”', importToken);
    if (nextToken === null) return false;
    localURL = nextURL.trim().replace(/\/$/, '') || defaultLocalURL;
    importToken = nextToken.trim();
    GM_setValue('xrwLocalURL', localURL);
    GM_setValue('xrwImportToken', importToken);
    debugLog('info', 'configuration.saved', { localURL, hasToken: Boolean(importToken) });
    updatePanel(importToken ? '连接设置已保存。' : '还没有填写连接码。');
    return Boolean(importToken);
  }

  function request(method, path, options = {}) {
    const startedAt = Date.now();
    const isFileUpload = /\/files$/.test(path);
    return new Promise((resolve, reject) => {
      GM_xmlhttpRequest({
        method,
        url: `${localURL}${path}`,
        headers: options.headers || {},
        data: options.data,
        responseType: 'text',
        timeout: options.timeout || 120000,
        onload(response) {
          let body = {};
          try { body = JSON.parse(response.responseText || '{}'); } catch { body = {}; }
          if (response.status < 200 || response.status >= 300) {
            debugLog('error', 'http.response', { method, path, status: response.status, error: body.error || '' });
            reject(new Error(body.error || `本地上传器返回 ${response.status}`));
            return;
          }
          if (!isFileUpload) debugLog('info', 'http.response', { method, path, status: response.status, elapsedMs: Date.now() - startedAt });
          resolve(body);
        },
        onerror: () => {
          debugLog('error', 'http.connection_error', { method, path, localURL, elapsedMs: Date.now() - startedAt });
          reject(new Error('无法连接本地上传器，请先启动上传器并检查地址。'));
        },
        ontimeout: () => {
          debugLog('error', 'http.timeout', { method, path, elapsedMs: Date.now() - startedAt });
          reject(new Error('发送到本地上传器超时。'));
        }
      });
    });
  }

  async function checkConnection() {
    try {
      await request('GET', '/api/state', { timeout: 5000 });
      updatePanel(`已连接本地上传器 · v${scriptVersion}`);
    } catch (error) {
      updatePanel(error.message);
    }
  }

  function findMessage(target) {
    if (!(target instanceof Element)) return null;
    const directMessage = target.closest(telegramMessageSelector);
    return directMessage && !host.contains(directMessage) && renderedMediaCount(directMessage) > 0
      ? directMessage
      : null;
  }

  function messageKey(message) {
    const data = message.dataset || {};
    const value = data.messageId || data.mid || data.id || data.peerId;
    if (value) return String(value);
    const link = [...message.querySelectorAll('a[href]')].find((item) => /t\.me\/.+\/\d+/.test(item.href));
    if (link) return link.href;
    const bounds = message.getBoundingClientRect();
    return `${location.hash}:${Math.round(bounds.top)}:${message.textContent.slice(0, 40)}`;
  }

  function sourceURL(message) {
    const link = [...message.querySelectorAll('a[href]')].find((item) => /t\.me\/.+\/\d+/.test(item.href));
    return link ? link.href : location.href;
  }

  function captionScore(text) {
    const value = String(text || '').trim();
    if (!value || /^\d{1,2}:\d{2}$/.test(value)) return -10000;
    let score = Math.min(value.length, 400);
    if (/标签\s*[:：]/i.test(value)) score += 1200;
    if (/(?:^|\s)#[^\s#]+/.test(value)) score += 500;
    if (/\b(?:album|no\.?\s*\d+)\b/i.test(value)) score += 300;
    if (/\b\d+\s*(?:comments?|repl(?:y|ies))\b/i.test(value)) score -= 80;
    return score;
  }

  function messageText(message) {
    const selectors = '.text-content, .message-text, .caption, [class*="text-content"], [class*="caption"]';
    const candidates = [...message.querySelectorAll(selectors)]
      .map((element) => String(element.innerText || '').trim())
      .filter(Boolean);
    candidates.push(String(message.innerText || '').trim());
    return [...new Set(candidates)].sort((left, right) => captionScore(right) - captionScore(left))[0] || '';
  }

  function parseCaption(text) {
    const cleaned = text.replace(/\r/g, '').trim();
    const tags = [...cleaned.matchAll(/#([^\s#，,。；;]+)/g)].map((match) => match[1]);
    const titlePart = cleaned.split(/标签\s*[:：]/i)[0].trim();
    const lines = titlePart.split('\n')
      .map((line, index) => ({
        index,
        text: line.normalize('NFKC')
          .replace(/[\uE000-\uF8FF\u200B-\u200D\uFEFF]/g, '')
          .replace(/[\u{F0000}-\u{FFFFD}\u{100000}-\u{10FFFD}]/gu, '')
          .replace(/^Album\s*[,，:：-]?\s*/i, '')
          .replace(/^(?:#[^\s#]+\s*)+/, '')
          .replace(/^[「『“”]+|[」』“”]+$/g, '')
          .trim()
      }))
      .filter((line) => line.text && !/^\d{1,2}:\d{2}$/.test(line.text) &&
        !/^\d+\s*(?:comments?|repl(?:y|ies))$/i.test(line.text) &&
        !/^(?:forwarded from|discussion started)$/i.test(line.text));
    lines.forEach((line) => {
      line.score = Math.min(line.text.length, 180) - line.index;
      if (/[\[【(（]\s*\d+\s*[Pp](?:\s*[,，/+ -]\s*\d+\s*[Vv])?/i.test(line.text)) line.score += 1200;
      if (/\b\d+(?:\.\d+)?\s*(?:GB|MB)\b/i.test(line.text)) line.score += 700;
      if (/\bNo\.?\s*\d+/i.test(line.text)) line.score += 500;
      if (/[「」『』\[\]【】]/.test(line.text)) line.score += 120;
    });
    const title = lines.sort((left, right) => right.score - left.score)[0]?.text || 'Telegram 图集';
    return { title: title || 'Telegram 图集', tags: [...new Set(tags)] };
  }

  function videoContainer(element) {
    return element.closest('video, [data-video], [class*="media-video"], [class*="video-container"], [class*="Video"]');
  }

  function isMediaImage(image) {
    const source = image.currentSrc || image.src || '';
    if (!source || source.startsWith('data:image/svg')) return false;
    const context = `${image.className || ''} ${image.parentElement?.className || ''}`.toLowerCase();
    if (/avatar|emoji|reaction|sticker|icon|profile/.test(context)) return false;
    if (videoContainer(image)) return false;
    const bounds = image.getBoundingClientRect();
    return Math.max(image.naturalWidth || 0, bounds.width) >= 120 &&
      Math.max(image.naturalHeight || 0, bounds.height) >= 120;
  }

  async function blobBitmapSize(blob) {
    const url = URL.createObjectURL(blob);
    try {
      return await new Promise((resolve, reject) => {
        const image = new Image();
        image.onload = () => resolve({ width: image.naturalWidth, height: image.naturalHeight });
        image.onerror = () => reject(new Error('无法读取图片尺寸'));
        image.src = url;
      });
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  function wait(milliseconds) {
    return new Promise((resolve) => setTimeout(resolve, milliseconds));
  }

  function mediaLoadSnapshot(message) {
    const images = [...message.querySelectorAll('img')].filter(isMediaImage);
    const canvases = [...message.querySelectorAll('canvas')].filter((canvas) => {
      const bounds = canvas.getBoundingClientRect();
      return bounds.width >= 120 && bounds.height >= 120 && !videoContainer(canvas);
    });
    const pending = images.filter((image) => !image.complete || !image.naturalWidth || !image.naturalHeight).length;
    const clearImageRects = images.filter((image) => image.naturalWidth >= 360 && image.naturalHeight >= 360)
      .map(elementRect);
    const small = images.filter((image) => image.naturalWidth > 0 && image.naturalHeight > 0 &&
      (Math.min(image.naturalWidth, image.naturalHeight) < 360 || Math.max(image.naturalWidth, image.naturalHeight) < 640) &&
      !clearImageRects.some((rect) => overlaps(elementRect(image), rect))).length;
    const smallCanvases = canvases.filter((canvas) => canvas.width > 0 && canvas.height > 0 &&
      (Math.min(canvas.width, canvas.height) < 360 || Math.max(canvas.width, canvas.height) < 640) &&
      !clearImageRects.some((rect) => overlaps(elementRect(canvas), rect))).length;
    const signature = images.map((image) =>
      `${image.currentSrc || image.src}|${image.complete ? 1 : 0}|${image.naturalWidth}x${image.naturalHeight}`).join('\n');
    return { images, canvases, pending, small: small + smallCanvases, signature };
  }

  async function waitForMediaReady(message) {
    const startedAt = Date.now();
    let previousSignature = '';
    let stablePasses = 0;
    let snapshot = mediaLoadSnapshot(message);
    debugLog('info', 'media.wait.started', {
      images: snapshot.images.length,
      canvases: snapshot.canvases.length,
      pending: snapshot.pending,
      suspectedPlaceholders: snapshot.small
    });
    for (let attempt = 0; attempt < 28; attempt += 1) {
      for (const image of snapshot.images) image.loading = 'eager';
      const elapsed = Date.now() - startedAt;
      if (elapsed >= 1500 && snapshot.pending === 0 && snapshot.small === 0 && snapshot.signature === previousSignature) {
        stablePasses += 1;
      } else {
        stablePasses = 0;
      }
      if (stablePasses >= 2) break;
      previousSignature = snapshot.signature;
      await wait(300);
      snapshot = mediaLoadSnapshot(message);
    }
    const level = snapshot.pending || snapshot.small ? 'warn' : 'info';
    debugLog(level, 'media.wait.finished', {
      elapsedMs: Date.now() - startedAt,
      images: snapshot.images.length,
      canvases: snapshot.canvases.length,
      pending: snapshot.pending,
      suspectedPlaceholders: snapshot.small
    });
    return snapshot;
  }

  function elementRect(element) {
    const bounds = element.getBoundingClientRect();
    return { left: bounds.left, top: bounds.top, right: bounds.right, bottom: bounds.bottom, width: bounds.width, height: bounds.height };
  }

  function overlaps(left, right) {
    const width = Math.max(0, Math.min(left.right, right.right) - Math.max(left.left, right.left));
    const height = Math.max(0, Math.min(left.bottom, right.bottom) - Math.max(left.top, right.top));
    const smallestArea = Math.min(left.width * left.height, right.width * right.height);
    return smallestArea > 0 && (width * height) / smallestArea >= 0.65;
  }

  async function imageBlobs(message) {
    await waitForMediaReady(message);
    const candidates = [];
    const seen = new Set();
    const imageRects = [];
    let coveredPlaceholders = 0;
    const renderedImages = [...message.querySelectorAll('img')].filter(isMediaImage).map((image) => ({
      type: 'url',
      source: image.currentSrc || image.src,
      width: image.naturalWidth || 0,
      height: image.naturalHeight || 0,
      rect: elementRect(image)
    })).sort((left, right) => (right.width * right.height) - (left.width * left.height));
    for (const candidate of renderedImages) {
      if (seen.has(candidate.source)) continue;
      if (imageRects.some((imageRect) => overlaps(candidate.rect, imageRect))) {
        coveredPlaceholders += 1;
        continue;
      }
      seen.add(candidate.source);
      candidates.push({
        type: 'url',
        source: candidate.source,
        width: candidate.width,
        height: candidate.height,
        rect: candidate.rect
      });
      imageRects.push(candidate.rect);
    }
    for (const element of message.querySelectorAll('[style*="background-image"]')) {
      const bounds = element.getBoundingClientRect();
      if (bounds.width < 120 || bounds.height < 120) continue;
      if (videoContainer(element)) continue;
      const rect = elementRect(element);
      if (imageRects.some((imageRect) => overlaps(rect, imageRect))) {
        coveredPlaceholders += 1;
        continue;
      }
      const match = getComputedStyle(element).backgroundImage.match(/^url\(["']?(.*?)["']?\)$/);
      if (!match || seen.has(match[1])) continue;
      seen.add(match[1]);
      candidates.push({ type: 'url', source: match[1], width: 0, height: 0, rect });
    }
    for (const canvas of message.querySelectorAll('canvas')) {
      const bounds = canvas.getBoundingClientRect();
      if (bounds.width >= 120 && bounds.height >= 120 && !videoContainer(canvas)) {
        const rect = elementRect(canvas);
        if (imageRects.some((imageRect) => overlaps(rect, imageRect))) {
          coveredPlaceholders += 1;
          continue;
        }
        candidates.push({ type: 'canvas', source: canvas, width: canvas.width, height: canvas.height, rect });
      }
    }

    const result = [];
    const samples = [];
    let failures = 0;
    for (const candidate of candidates) {
      try {
        let blob;
        if (candidate.type === 'canvas') {
          blob = await new Promise((resolve) => candidate.source.toBlob(resolve, 'image/png'));
        } else {
          const response = await fetch(candidate.source, { credentials: 'include' });
          if (!response.ok) throw new Error(`图片请求返回 ${response.status}`);
          blob = await response.blob();
        }
        if (!blob || blob.size <= 0 || !String(blob.type).startsWith('image/')) continue;
        const dimensions = candidate.width && candidate.height
          ? { width: candidate.width, height: candidate.height }
          : await blobBitmapSize(blob);
        if (samples.length < 20) samples.push({ type: candidate.type, width: dimensions.width, height: dimensions.height, bytes: blob.size });
        result.push(blob);
      } catch (error) {
        failures += 1;
        debugLog('error', 'media.read_failed', { type: candidate.type, ...errorDetails(error) });
        console.warn('XRW Telegram import could not read an image', error);
      }
    }
    debugLog(failures ? 'warn' : 'info', 'media.collected', {
      candidates: candidates.length,
      images: result.length,
      coveredPlaceholders,
      failures,
      samples
    });
    return { blobs: result, coveredPlaceholders, failures };
  }

  function discussionControl(target, message) {
    if (!(target instanceof Element)) return null;
    const control = target.closest('a, button, [role="button"], [class*="comment"], [class*="Comment"], [class*="replies"], [class*="Replies"], [class*="discussion"], [class*="Discussion"]');
    if (!control || !message.contains(control)) return null;
    const marker = `${control.textContent || ''} ${control.className || ''}`;
    return /comments?|discussion|repl(?:y|ies)|评论|回复/i.test(marker) ? control : null;
  }

  function findDiscussionControl(message) {
    const selector = 'a, button, [role="button"], [class*="comment"], [class*="Comment"], [class*="replies"], [class*="Replies"], [class*="discussion"], [class*="Discussion"]';
    for (const element of message.querySelectorAll(selector)) {
      const marker = `${element.textContent || ''} ${element.className || ''}`;
      if (/comments?|discussion|repl(?:y|ies)|评论|回复/i.test(marker)) return element;
    }
    return null;
  }

  function discussionLabel(message) {
    const elements = message.querySelectorAll('a, button, [role="button"], [class*="comment"], [class*="Comment"], [class*="replies"], [class*="Replies"]');
    for (const element of elements) {
      const text = String(element.textContent || '').trim();
      if (/\b\d+\s*(?:comments?|repl(?:y|ies))\b/i.test(text) || /\d+\s*(?:条)?(?:评论|回复)/.test(text)) return text;
    }
    return '';
  }

  function renderedMediaCount(message) {
    let count = [...message.querySelectorAll('img')].filter(isMediaImage).length;
    count += [...message.querySelectorAll('[style*="background-image"]')].filter((element) => {
      const bounds = element.getBoundingClientRect();
      return bounds.width >= 120 && bounds.height >= 120 && !videoContainer(element);
    }).length;
    count += [...message.querySelectorAll('canvas')].filter((canvas) => {
      const bounds = canvas.getBoundingClientRect();
      return bounds.width >= 120 && bounds.height >= 120 && !videoContainer(canvas);
    }).length;
    return count;
  }

  function renderedMessageCandidates() {
    const selectorGroups = [
      '.message-list-item', '[class*="message-list-item"]', '.Message',
      '.bubble[data-peer-id]', '.bubble'
    ];
    for (const selector of selectorGroups) {
      const candidates = [...document.querySelectorAll(selector)]
        .filter((message) => !host.contains(message) && renderedMediaCount(message) > 0);
      if (candidates.length) return candidates;
    }
    return [];
  }

  async function waitForDiscussionAlbum() {
    let best = null;
    let bestCount = 0;
    let stablePasses = 0;
    for (let attempt = 0; attempt < 40; attempt += 1) {
      await new Promise((resolve) => setTimeout(resolve, 500));
      const current = renderedMessageCandidates()
        .map((message) => ({ message, count: renderedMediaCount(message) }))
        .sort((left, right) => right.count - left.count)[0];
      if (!current || current.count <= selectedMainMediaCount) continue;
      stablePasses = current.count === bestCount ? stablePasses + 1 : 0;
      best = current.message;
      bestCount = current.count;
      updatePanel(`评论区已发现 ${bestCount} 张已加载媒体，正在等待页面稳定…`);
      if (stablePasses >= 2) return best;
    }
    return best;
  }

  function extensionFor(blob) {
    switch (blob.type) {
      case 'image/png': return 'png';
      case 'image/gif': return 'gif';
      case 'image/webp': return 'webp';
      default: return 'jpg';
    }
  }

  async function ensureDraft(message) {
    if (draft) return draft;
    if (!importToken && !configure()) throw new Error('需要先填写本地上传器连接码。');
    const caption = parseCaption(messageText(message));
    draft = await request('POST', '/api/telegram-imports', {
      headers: { 'Content-Type': 'application/json', 'X-XRW-Import-Token': importToken },
      data: JSON.stringify({ source_url: sourceURL(message), title: caption.title, tags: caption.tags })
    });
    debugLog('info', 'draft.created', { id: draft.id, title: caption.title, tags: caption.tags.length });
    open.disabled = false;
    return draft;
  }

  async function selectMessage(message) {
    const key = messageKey(message);
    if (selectedKeys.has(key)) {
      updatePanel('这条消息已经加入当前草稿。');
      return;
    }
    busy = true;
    debugLog('info', 'selection.started', { key: String(key).slice(0, 180) });
    updatePanel('正在等待图片加载稳定，再读取媒体…');
    try {
      const currentDraft = await ensureDraft(message);
      const media = await imageBlobs(message);
      const blobs = media.blobs;
      selectedKeys.add(key);
      message.dataset.xrwImportSelected = 'true';
      selectedMessages += 1;
      let added = 0;
      let duplicates = 0;
      for (let index = 0; index < blobs.length; index += 1) {
        const blob = blobs[index];
        const name = `tg-${String(key).replace(/[^a-z0-9_-]+/gi, '-').slice(-48)}-${index + 1}.${extensionFor(blob)}`;
        const result = await request('POST', `/api/telegram-imports/${encodeURIComponent(currentDraft.id)}/files`, {
          headers: {
            'Content-Type': blob.type || 'image/jpeg',
            'X-XRW-Import-Token': importToken,
            'X-XRW-File-Name': encodeURIComponent(name),
            'X-XRW-Source-Message': String(key).slice(0, 180)
          },
          data: blob
        });
        if (result.duplicate) duplicates += 1;
        else added += 1;
      }
      uploadedImages += added;
      const duplicateText = duplicates ? `，跳过 ${duplicates} 张完全重复图片` : '';
      const placeholderText = media.coveredPlaceholders ? `，用清晰图替代 ${media.coveredPlaceholders} 个重叠占位层` : '';
      const failureText = media.failures ? `，${media.failures} 个候选读取失败（见日志）` : '';
      const comments = discussionLabel(message);
      const trigger = findDiscussionControl(message);
      if (trigger) {
        discussionTrigger = trigger;
        selectedMainMediaCount = renderedMediaCount(message);
      }
      const commentsText = comments ? `；检测到 ${comments}，可点击“读取该帖评论区图片”自动补全` : '';
      debugLog('info', 'selection.finished', { key: String(key).slice(0, 180), added, duplicates, failures: media.failures });
      updatePanel(blobs.length
        ? `已加入 ${added} 张图片${duplicateText}${placeholderText}${failureText}${commentsText}。`
        : '已记录主帖文字，但网页里没有发现已加载图片；可继续选择后续图片消息。');
    } catch (error) {
      debugLog('error', 'selection.failed', { key: String(key).slice(0, 180), ...errorDetails(error) });
      updatePanel(error.message);
    } finally {
      busy = false;
      updatePanel();
    }
  }

  document.addEventListener('click', (event) => {
    if (!pickMode || busy) return;
    const message = findMessage(event.target);
    if (!message) return;
    if (discussionControl(event.target, message)) {
      updatePanel('正在打开评论区；打开后继续点击里面的图片消息。');
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    event.stopImmediatePropagation();
    selectMessage(message);
  }, true);

  toggle.addEventListener('click', () => {
    if (!importToken && !configure()) return;
    pickMode = !pickMode;
    document.documentElement.style.cursor = pickMode ? 'crosshair' : '';
    updatePanel(pickMode ? '选择模式已开启：点击主帖、后续图片消息或评论区图片消息。' : '选择已暂停，可进入本地页面编辑。');
  });

  open.addEventListener('click', () => {
    if (!draft) return;
    window.open(`${localURL}/?telegram_import=${encodeURIComponent(draft.id)}`, '_blank', 'noopener');
  });

  comments.addEventListener('click', async () => {
    if (!discussionTrigger || busy) return;
    const trigger = discussionTrigger;
    discussionTrigger = null;
    busy = true;
    updatePanel('正在打开该帖评论区…');
    trigger.click();
    const album = await waitForDiscussionAlbum();
    busy = false;
    if (!album) {
      updatePanel('没有找到评论区图片。请确认评论区已经打开并加载，再手动点击图片消息。');
      return;
    }
    updatePanel('评论区已打开，正在把完整图片加入同一个草稿…');
    await selectMessage(album);
  });

  reset.addEventListener('click', () => {
    draft = null;
    selectedMessages = 0;
    uploadedImages = 0;
    discussionTrigger = null;
    selectedMainMediaCount = 0;
    selectedKeys.clear();
    for (const element of document.querySelectorAll('[data-xrw-import-selected="true"]')) {
      delete element.dataset.xrwImportSelected;
    }
    updatePanel('已开始新的草稿；之前草稿仍保留在本地上传器中。');
  });

  settings.addEventListener('click', configure);
  logs.addEventListener('click', copyDebugLog);
  shadow.querySelector('.hide').addEventListener('click', () => { panel.hidden = true; });
  GM_registerMenuCommand('显示绮影志导入面板', () => { panel.hidden = false; });
  GM_registerMenuCommand('设置本地上传器连接', configure);
  GM_registerMenuCommand('复制绮影志导入日志', copyDebugLog);
  GM_registerMenuCommand('清空绮影志导入日志', () => {
    debugEntries = [];
    GM_setValue('xrwDebugEntries', debugEntries);
    updatePanel('调试日志已清空。');
  });
  debugLog('info', 'script.initialized', { version: scriptVersion, path: location.pathname, hash: location.hash });
  updatePanel();
  checkConnection();
})();
