const statusPill = document.getElementById("status");
const feed = document.getElementById("feed");
const liveFeed = document.getElementById("live"); // NEW: right column live stream
const soundBtn = document.getElementById("soundBtn");
const soundState = document.getElementById("soundState");
const fallbackAudio = document.getElementById("alertAudio");
const dateInput = document.getElementById("dateInput");
const btnStart = document.getElementById("btnStart");
const btnStop = document.getElementById("btnStop");
const btnPause = document.getElementById("btnPause");
const btnClear = document.getElementById("btnClear");
const btnReloadWL = document.getElementById("btnReloadWl"); // NEW
const chkHod = document.getElementById("chkHod");
const chkLod = document.getElementById("chkLod");
const chkCompact = document.getElementById("chkCompact"); // NEW
// NEW RVOL controls
const rvolThreshold = document.getElementById("rvolThreshold");
const rvolMethod = document.getElementById("rvolMethod");
const baselineSingle = document.querySelector('input[name="baseline"][value="single"]');
const baselineCumulative = document.querySelector('input[name="baseline"][value="cumulative"]');
const rvolActive = document.getElementById("rvolActive");
const silentMode = document.getElementById("silentMode");
const bucketSize = document.getElementById("bucketSize");
// NEW Recent Alerts
const alertsTableBody = document.querySelector("#alertsTable tbody");
const alertsCount = document.getElementById("alertsCount");
// Pinned UI
const pinnedWrap = document.getElementById("pinnedWrap");
const pinnedList = document.getElementById("pinned");
const btnUnpinAll = document.getElementById("btnUnpinAll");
const pinnedCountEl = document.getElementById("pinnedCount");
let ws = null;
let audioCtx = null;
let audioBuf = null;
let soundEnabled = true; // default: Sound ON
let paused = false;
let historyLoaded = false;
let allAlerts = []; // [{kind:"lod"|"hod", sym, name, price, time, ts_unix}]
let recentAlerts = []; // for RVOL [{time, symbol, price, volume, baseline, rvol, method}]
let silent = false;
let sessionDateET = ""; // YYYY-MM-DD (from /api/status)
// --- Mini chart management ---
let chartsLimit = 5; // (was const) allow dynamic limit in compact mode
const chartsById = new Map();
const activeChartQueue = [];
const chartLookbackMinDefault = 120;
let chartLookbackMin = chartLookbackMinDefault;
const chartRefreshMs = 15000;
// --- Pinning (persisted in this browser) ---
const pinLimit = 6;
let pinnedOrder = []; // array of ids (in pin insertion order)
let pinnedSet = new Set(); // quick membership
function loadPins(){
  try {
    const raw = localStorage.getItem("alertcat.pins") || "[]";
    const arr = JSON.parse(raw);
    pinnedOrder = Array.isArray(arr) ? arr.filter(Boolean) : [];
    pinnedSet = new Set(pinnedOrder);
  } catch {
    pinnedOrder = [];
    pinnedSet = new Set();
  }
}
function savePins(){
  localStorage.setItem("alertcat.pins", JSON.stringify(pinnedOrder));
}
function isPinnedId(id){ return pinnedSet.has(id); }
function togglePinById(id){
  if (pinnedSet.has(id)) {
    // Unpin
    pinnedSet.delete(id);
    pinnedOrder = pinnedOrder.filter(x => x !== id);
  } else {
    // Pin (respect limit; evict oldest)
    if (pinnedOrder.length >= pinLimit) {
      const evict = pinnedOrder.shift();
      if (evict) pinnedSet.delete(evict);
    }
    pinnedSet.add(id);
    pinnedOrder.push(id);
  }
  savePins();
  renderAll();
}
function setStatus(text, ok=false){
  statusPill.textContent = text;
  statusPill.className = ok ? "pill ok" : "pill";
}
async function loadSound() {
  try {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return false;
    audioCtx = new AC();
    const resp = await fetch("/alert.mp3", { cache: "force-cache" });
    const arr = await resp.arrayBuffer();
    audioBuf = await audioCtx.decodeAudioData(arr);
    return true;
  } catch { return false; }
}
async function enableSound() {
  if (!audioCtx || audioCtx.state === "suspended") { try { await audioCtx.resume(); } catch {} }
  soundEnabled = true;
  soundState.textContent = "Sound ON";
}
function playSound() {
  if (!soundEnabled || silent) return;
  if (audioCtx && audioBuf) {
    try {
      const src = audioCtx.createBufferSource();
      src.buffer = audioBuf;
      src.connect(audioCtx.destination);
      src.start();
      return;
    } catch {}
  }
  try { fallbackAudio.currentTime = 0; fallbackAudio.play(); } catch {}
}
function todayISO() {
  const d = new Date();
  const yyyy = d.getFullYear();
  const mm = String(d.getMonth()+1).padStart(2,"0");
  const dd = String(d.getDate()).padStart(2,"0");
  return `${yyyy}-${mm}-${dd}`;
}
function getParam(name){
  try {
    const u = new URL(location.href);
    return u.searchParams.get(name) || "";
  } catch { return ""; }
}
function shouldShow(kind){
  const hodOn = !!chkHod?.checked;
  const lodOn = !!chkLod?.checked;
  if (!hodOn && !lodOn) return false;
  if (kind === "hod") return hodOn;
  if (kind === "lod") return lodOn;
  return false;
}
function alertId(a){ return `${a.sym}_${a.ts_unix}_${a.kind}`; }
// ----------------- Build & render cards -----------------
function buildAlertCard(a, autoChart=false, isPinned=false, isLive=false) {
  const id = alertId(a);
  const card = document.createElement("div");
  card.className = `card ${a.kind}${isPinned ? " isPinned" : ""}${isLive ? " live" : ""}`;
  card.dataset.id = id;
  card.dataset.sym = a.sym;
  card.dataset.ts = String(a.ts_unix);
  card.dataset.kind = a.kind;
  card.dataset.live = isLive ? "1" : "0";
  const label = a.kind === "hod" ? "NEW HOD" : "NEW LOD";
  const priceFmt = Number(a.price).toFixed(4).replace(/\.?0+$/, '');
  if (isLive) {
    // Live variant: no chart row, no "More news" link
    card.innerHTML = `
      <div class="left">
        <span class="badge">${label}</span>
        <span class="sym">${a.sym}</span>
        <span class="name">${a.name || ""}</span>
        <button class="iconBtn pinBtn" title="Pin/Unpin" aria-pressed="${isPinned ? "true" : "false"}">${isPinned ? "★" : "☆"}</button>
      </div>
      <div class="price">$${priceFmt}</div>
      <div class="time">${a.time}</div>
      <div class="infoRow">
        <div class="infoCol">
          <div class="sectionTitle">News</div>
          <ul class="newsList" id="news_${id}" aria-live="polite"></ul>
        </div>
        <div class="infoCol">
          <div class="sectionTitle">SEC Filings (today)</div>
          <ul class="secList" id="sec_${id}" aria-live="polite"></ul>
        </div>
      </div>
    `;
  } else {
    // Original (feed/pinned) variant
    card.innerHTML = `
      <div class="left">
        <span class="badge">${label}</span>
        <span class="sym">${a.sym}</span>
        <span class="name">${a.name || ""}</span>
        <button class="iconBtn pinBtn" title="Pin/Unpin" aria-pressed="${isPinned ? "true" : "false"}">${isPinned ? "★" : "☆"}</button>
      </div>
      <div class="price">$${priceFmt}</div>
      <div class="time">${a.time}</div>
      <div class="chartRow">
        <div class="chartBox disabled" id="chart_${id}" aria-hidden="true"></div>
        <div class="chartBtns">
          <button class="toggleChart" data-action="toggle">Enable chart</button>
        </div>
      </div>
      <div class="infoRow">
        <div class="infoCol">
          <div class="sectionTitle">News</div>
          <ul class="newsList" id="news_${id}" aria-live="polite"></ul>
          <a class="moreLink" id="moreNews_${id}" target="_blank" rel="noopener">More news →</a>
        </div>
        <div class="infoCol">
          <div class="sectionTitle">SEC Filings (today)</div>
          <ul class="secList" id="sec_${id}" aria-live="polite"></ul>
        </div>
      </div>
    `;
  }
  // Prefill placeholders so Live cards never look empty while loading.
  const newsUL0 = card.querySelector(`#news_${id}`);
  const secUL0  = card.querySelector(`#sec_${id}`);
  if (newsUL0) newsUL0.innerHTML = `<li class="muted">loading…</li>`;
  if (secUL0)  secUL0.innerHTML  = `<li class="muted">loading…</li>`;
  // Pin button
  const pinBtn = card.querySelector('.pinBtn');
  pinBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    togglePinById(id);
  });
  // Chart toggle
  const btn = card.querySelector('button.toggleChart');
  if (btn) {
    btn.addEventListener('click', async () => {
      if (chartsById.has(id)) {
        destroyChart(id);
        btn.textContent = "Enable chart";
        btn.classList.remove('active');
        card.querySelector('.chartBox').classList.add('disabled');
      } else {
        // Show box BEFORE creating chart so sizing works
        card.querySelector('.chartBox').classList.remove('disabled');
        await ensureUnderLimitThenSpawn(id, a.sym);
        btn.textContent = "Disable chart";
        btn.classList.add('active');
      }
    });
  }
  if (autoChart && btn) {
    setTimeout(async () => {
      // Show box BEFORE creating chart so sizing works
      card.querySelector('.chartBox').classList.remove('disabled');
      await ensureUnderLimitThenSpawn(id, a.sym);
      const tbtn = card.querySelector('button.toggleChart');
      tbtn.textContent = "Disable chart";
      tbtn.classList.add('active');
    }, 0);
  }
  // Populate News/SEC scoped to THIS card to avoid duplicate-ID collisions
  setTimeout(() => populateNewsAndFilingsForCard(card, a.sym), 0);
  return card;
}
function renderPinned() {
  // Remove charts (fresh render)
  for (const id of Array.from(chartsById.keys())) {
    // we clear all charts in renderAll(); nothing specific here
  }
  // Build a quick index: id -> alert
  const amap = new Map(allAlerts.map(a => [alertId(a), a]));
  // Keep only pins that still exist in history
  const kept = [];
  for (const id of pinnedOrder) {
    if (amap.has(id)) kept.push(id);
  }
  if (kept.length !== pinnedOrder.length) {
    pinnedOrder = kept;
    pinnedSet = new Set(kept);
    savePins();
  }
  pinnedList.innerHTML = "";
  if (kept.length === 0) {
    pinnedWrap.classList.add("hidden");
    btnUnpinAll.disabled = true;
    pinnedCountEl.textContent = "(0)";
    return;
  }
  pinnedWrap.classList.remove("hidden");
  btnUnpinAll.disabled = false;
  pinnedCountEl.textContent = `(${kept.length})`;
  // Show newest pin first (reverse insertion order)
  const frag = document.createDocumentFragment();
  for (let i = kept.length - 1; i >= 0; i--) {
    const id = kept[i];
    const a = amap.get(id);
    if (!a) continue;
    // pinned cards: auto chart by default
    frag.appendChild(buildAlertCard(a, true, true));
  }
  pinnedList.appendChild(frag);
}
function renderFeed() {
  feed.innerHTML = "";
  // Exclude pinned from the feed and apply HOD/LOD filter
  const filtered = allAlerts.filter(a => !isPinnedId(alertId(a)) && shouldShow(a.kind));
  const frag = document.createDocumentFragment();
  filtered.forEach((a, idx) => {
    const autoChart = idx < chartsLimit; // charts for top N in feed (dynamic)
    frag.appendChild(buildAlertCard(a, autoChart, false));
  });
  feed.appendChild(frag);
}

/* ============ RIGHT: Live stream ============ */
let liveMaxItems = 6; // will be recomputed after first render
function trimLive() {
  if (!liveFeed) return;
  while (liveFeed.children.length > liveMaxItems) {
    liveFeed.removeChild(liveFeed.lastElementChild);
  }
}
function recomputeLiveCapacity() {
  if (!liveFeed) return;
  // Make sure the container has a precise height that fits the viewport.
  const boxTop = liveFeed.getBoundingClientRect().top;
  const target = Math.max(120, Math.floor(window.innerHeight - boxTop - 12));
  liveFeed.style.height = target + "px";

  // Estimate card height (including the vertical gap).
  let sample = liveFeed.querySelector(".card") || feed?.querySelector?.(".card");
  let gap = 8;
  try {
    const cs = getComputedStyle(liveFeed);
    gap = parseInt(cs.rowGap || cs.gap || "8", 10) || 8;
  } catch {}
  const cardH = sample ? Math.ceil(sample.getBoundingClientRect().height) + gap : 160;
  liveMaxItems = Math.max(1, Math.floor(target / cardH));
  trimLive();
}
function seedLiveFromHistory() {
  if (!liveFeed) return;
  liveFeed.innerHTML = "";
  // take last N alerts (newest last in allAlerts) so we can prepend in correct order
  const take = Math.min(liveMaxItems, allAlerts.length);
  for (let i = allAlerts.length - take; i < allAlerts.length; i++) {
    const a = allAlerts[i];
    if (!a) continue;
    if (!shouldShow(a.kind) || isPinnedId(alertId(a))) continue;
    const node = buildAlertCard(a, false, false, true); // LIVE variant
    liveFeed.prepend(node); // newest ends up at top
  }
  trimLive();
}
function addLiveCard(a) {
  if (!liveFeed) return;
  const node = buildAlertCard(a, false, false, true); // LIVE variant
  liveFeed.prepend(node); // newest to top
  trimLive();
}
function renderAll(){
  // Destroy all charts (both pinned + feed will fully re-render)
  for (const id of Array.from(chartsById.keys())) destroyChart(id);
  renderPinned();
  renderFeed();
  recomputeLiveCapacity();
  seedLiveFromHistory();
}
function addIncomingAlert(a){
  allAlerts.push(a);
  // If it’s pinned (unlikely on first arrive), it will appear in pinned on next render.
  if (shouldShow(a.kind) && !isPinnedId(alertId(a))) {
    const autoChart = true;
    const node = buildAlertCard(a, autoChart, false);
    feed.appendChild(node);
    playSound();
    addLiveCard(a); // mirror to the live stream on the right
  } else {
    // muted by filter or pinned; still play sound for pinned
    if (isPinnedId(alertId(a))) playSound();
  }
}
// NEW: Render recent RVOL alerts
function renderRecentAlerts() {
  if (!alertsTableBody || !alertsCount) return;
  alertsTableBody.innerHTML = "";
  const frag = document.createDocumentFragment();
  recentAlerts.slice().reverse().forEach(a => { // newest first
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${a.time}</td>
      <td>${a.symbol}</td>
      <td>$${Number(a.price).toFixed(2)}</td>
      <td>${a.volume}</td>
      <td>${Number(a.baseline).toFixed(0)}</td>
      <td>${Number(a.rvol).toFixed(2)}</td>
      <td>${a.method}</td>
    `;
    frag.appendChild(tr);
  });
  alertsTableBody.appendChild(frag);
  alertsCount.textContent = `(${recentAlerts.length})`;
}
function addRvolAlert(msg) {
  recentAlerts.push({
    time: msg.time,
    symbol: msg.sym,
    price: msg.price,
    volume: msg.volume,
    baseline: msg.baseline,
    rvol: msg.rvol,
    method: msg.method
  });
  if (recentAlerts.length > 200) recentAlerts.shift();
  renderRecentAlerts();
  if (!silent) playSound();
}
// ----------------- Mini charts -----------------
async function ensureUnderLimitThenSpawn(id, sym) {
  if (chartsById.has(id)) return;
  if (activeChartQueue.length >= chartsLimit) {
    // Prefer to evict a non-pinned chart first
    let evictId = activeChartQueue.find(x => !isPinnedId(x));
    if (!evictId) {
      // all pinned; evict the oldest anyway (if any)
      evictId = activeChartQueue[0];
    }
    if (evictId) destroyChart(evictId);
  }
  await spawnChart(id, sym);
  activeChartQueue.push(id);
}
function destroyChart(id) {
  if (!id) return;
  const rec = chartsById.get(id);
  if (!rec) return;
  try { if (rec.timer) clearInterval(rec.timer); } catch {}
  try { if (rec.ro) rec.ro.disconnect(); } catch {}
  try { rec.chart.remove(); } catch {}
  chartsById.delete(id);
  const idx = activeChartQueue.indexOf(id);
  if (idx >= 0) activeChartQueue.splice(idx, 1);
}
async function spawnChart(id, sym) {
  const box = document.getElementById(`chart_${id}`);
  if (!box) return;
  // Guard: library present?
  const LWC = (typeof window !== 'undefined') ? window.LightweightCharts : null;
  if (!LWC || typeof LWC.createChart !== 'function') {
    console.error('[mini-chart]', 'LightweightCharts not loaded; check <script> tag or network.');
    setStatus('Charts library failed to load', false);
    return;
  }
  // Show dimension debug just in case
  const w = box.clientWidth || 150;
  const h = box.clientHeight || 150;
  const chart = LWC.createChart(box, {
    width: w,
    height: h,
    layout: { background: { type: 'solid', color: '#0e1117' }, textColor: '#cbd5e1' },
    grid: { vertLines: { visible: false }, horzLines: { visible: false } },
    crosshair: { mode: 0 },
    timeScale: { borderVisible: false },
    rightPriceScale: { borderVisible: false },
  });
  let series;
  if (typeof chart.addCandlestickSeries === 'function') {
    series = chart.addCandlestickSeries({
      upColor: '#26a69a',
      downColor: '#ef5350',
      borderVisible: false,
      wickUpColor: '#26a69a',
      wickDownColor: '#ef5350',
    });
  } else if (typeof chart.addSeries === 'function') {
    series = chart.addSeries({
      type: 'Candlestick',
      upColor: '#26a69a',
      downColor: '#ef5350',
      borderVisible: false,
      wickUpColor: '#26a69a',
      wickDownColor: '#ef5350',
    });
  } else {
    console.error('[mini-chart]', 'No candlestick API on chart object:', chart);
    setStatus('Chart API mismatch', false);
    return;
  }
  async function loadBars(atMs) {
    const lookback = chartLookbackMin || 120;
    const url = `/api/bars?symbol=${encodeURIComponent(sym)}&at=${encodeURIComponent(atMs)}&mins=${encodeURIComponent(lookback)}`;
    try {
      const res = await fetch(url, { cache: 'no-store' });
      const j = await res.json();
      if (Array.isArray(j?.bars)) {
        const data = j.bars.map(b => ({
          time: Number(b.time), // epoch seconds expected
          open: Number(b.open),
          high: Number(b.high),
          low: Number(b.low),
          close: Number(b.close),
        }));
        series.setData(data);
        chart.timeScale().fitContent();
      } else {
        if (j && j.ok === false) {
          console.warn('[mini-chart]', 'bars fetch failed for', sym, j);
        }
      }
    } catch (e) {
      console.error('[mini-chart] fetch bars failed', sym, e);
    }
  }
  await loadBars(Date.now());
  const timer = setInterval(() => loadBars(Date.now()), chartRefreshMs);
  let ro;
  if (typeof ResizeObserver === 'function') {
    ro = new ResizeObserver(() => {
      const w2 = box.clientWidth || 150;
      const h2 = box.clientHeight || 150;
      chart.applyOptions({ width: w2, height: h2 });
    });
    ro.observe(box);
  }
  chartsById.set(id, { chart, series, container: box, timer, ro });
}
// ----------------- News + SEC -----------------
function sameETDate(iso, ymd) {
  if (!iso) return false;
  try {
    const d = new Date(iso);
    const s = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/New_York', year:'numeric', month:'2-digit', day:'2-digit' }).format(d);
    return s === ymd;
  } catch { return false; }
}
// NEW: scope to the specific card; ensures Live cards are populated
async function populateNewsAndFilingsForCard(cardEl, sym) {
  if (!cardEl) return;
  const newsUL = cardEl.querySelector('.newsList');
  const secUL  = cardEl.querySelector('.secList');
  if (!newsUL || !secUL) return;

  const isLive = cardEl.classList.contains('live');
  const d = sessionDateET || todayISO();

  const moreA = cardEl.querySelector('.moreLink');
  if (moreA) {
    moreA.href = `/news.html?ticker=${encodeURIComponent(sym)}&date=${encodeURIComponent(d)}`;
  }

  try {
    const res = await fetch(`/api/extra?ticker=${encodeURIComponent(sym)}&date=${encodeURIComponent(d)}&days=2`, { cache: 'no-store' });
    const j = await res.json();

    // ----- News -----
    const news = Array.isArray(j?.news) ? j.news : [];
    newsUL.innerHTML = "";
    if (isLive) {
      const todays = news
        .filter(n => sameETDate(n.published, d))
        .sort((a,b)=> new Date(b.published) - new Date(a.published));
      if (todays.length === 0) {
        newsUL.innerHTML = `<li class="muted">no headlines</li>`;
      } else {
        const n = todays[0];
        const time = n.published ? new Date(n.published).toLocaleString() : "";
        const li = document.createElement("li");
        li.className = "newsItem";
        li.innerHTML = `
          <a href="${n.url}" target="_blank" rel="noopener">
            <span class="headline">${escapeHTML(n.title || "")}</span>
            <span class="meta">${escapeHTML(n.source || "News")} • ${escapeHTML(time)}</span>
          </a>`;
        newsUL.appendChild(li);
      }
    } else {
      if (news.length === 0) {
        newsUL.innerHTML = `<li class="muted">No headlines.</li>`;
      } else {
        news.slice(0, 5).forEach(n => {
          const li = document.createElement("li");
          li.className = "newsItem";
          const time = n.published ? new Date(n.published).toLocaleString() : "";
          li.innerHTML = `
            <a href="${n.url}" target="_blank" rel="noopener">
              <span class="headline">${escapeHTML(n.title || "")}</span>
              <span class="meta">${escapeHTML(n.source || "News")} • ${escapeHTML(time)}</span>
            </a>`;
          newsUL.appendChild(li);
        });
      }
    }

    // ----- SEC filings -----
    const filings = Array.isArray(j?.filings) ? j.filings : [];
    secUL.innerHTML = "";
    if (isLive) {
      const todaysF = filings
        .filter(f => sameETDate(f.filedAt, d))
        .sort((a,b)=> new Date(b.filedAt) - new Date(a.filedAt));
      if (todaysF.length === 0) {
        secUL.innerHTML = `<li class="muted">no sec filings today</li>`;
      } else {
        const f = todaysF[0];
        const filedAt = f.filedAt ? new Date(f.filedAt).toLocaleString() : "";
        const desc = f.description || f.formType || "Filing";
        const link = f.linkToFilingDetails || "#";
        const label = f.formType ? `<span class="formType">${escapeHTML(f.formType)}</span>` : "";
        const li = document.createElement("li");
        li.className = "secItem";
        li.innerHTML = `
          <a href="${link}" target="_blank" rel="noopener">
            <span class="headline">${label} ${escapeHTML(desc)}</span>
            <span class="meta">${escapeHTML(f.companyName || "")} • ${escapeHTML(filedAt)}</span>
          </a>`;
        secUL.appendChild(li);
      }
    } else {
      if (filings.length === 0) {
        secUL.innerHTML = `<li class="muted">No SEC filings today.</li>`;
      } else {
        const limit = 5;
        filings.forEach((f, idx) => {
          const li = document.createElement("li");
          li.className = "secItem" + (idx >= limit ? " hidden" : "");
          const filedAt = f.filedAt ? new Date(f.filedAt).toLocaleString() : "";
          const desc = f.description || f.formType || "Filing";
          const link = f.linkToFilingDetails || "#";
          const label = f.formType ? `<span class="formType">${escapeHTML(f.formType)}</span>` : "";
          li.innerHTML = `
            <a href="${link}" target="_blank" rel="noopener">
              <span class="headline">${label} ${escapeHTML(desc)}</span>
              <span class="meta">${escapeHTML(f.companyName || "")} • ${escapeHTML(filedAt)}</span>
            </a>`;
          secUL.appendChild(li);
        });
        if (filings.length > limit) {
          const more = document.createElement("button");
          more.className = "linkBtn";
          more.textContent = `more… (${filings.length - limit})`;
          more.addEventListener("click", () => {
            secUL.querySelectorAll(".secItem.hidden").forEach(el => el.classList.remove("hidden"));
            more.remove();
          });
          const wrap = document.createElement("div");
          wrap.appendChild(more);
          secUL.parentElement.appendChild(wrap);
        }
      }
    }
  } catch {
    newsUL.innerHTML = `<li class="muted">News unavailable.</li>`;
    secUL.innerHTML = `<li class="muted">SEC unavailable.</li>`;
  }

  // right column might grow: recompute capacity so nothing gets clipped
  if (cardEl.classList.contains('live')) {
    recomputeLiveCapacity();
  }
}
function escapeHTML(s) {
  return String(s || "").replace(/[&<>"'`]/g, c =>
    ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;','`':'&#96;'}[c]));
}
// ----------------- WS & API -----------------
function connectWS() {
  if (ws) { try { ws.close(1000); } catch {} ws = null; }
  const proto = location.protocol === "https:" ? "wss://" : "ws://";
  ws = new WebSocket(proto + location.host + "/ws");
  ws.onopen = () => setStatus("Connected", true);
  ws.onclose = () => setStatus("Disconnected");
  ws.onerror = () => setStatus("Error");
  ws.onmessage = (evt) => {
    try {
      const msg = JSON.parse(evt.data);
      if (msg.type === "status") {
        setStatus(msg.text, msg.level === "success" || msg.level === "info");
        return;
      }
      if (msg.type === "history") {
        historyLoaded = true;
        allAlerts = msg.alerts.slice(); // oldest -> newest
        renderAll();
        return;
      }
      if (msg.type === "alert") {
        addIncomingAlert(msg);
        return;
      }
      // NEW: RVOL history and alerts
      if (msg.type === "rvol_history") {
        // Normalize server payload ({sym: "...", ...}) to the UI table shape ({symbol: "...", ...})
        // so that history renders correctly before any new real-time alerts arrive.
        recentAlerts = (Array.isArray(msg.alerts) ? msg.alerts : []).map(a => ({
          time: a.time,
          symbol: a.sym,     // <-- normalize field name
          price: a.price,
          volume: a.volume,
          baseline: a.baseline,
          rvol: a.rvol,
          method: a.method
        }));
        renderRecentAlerts();
        return;
      }
      if (msg.type === "rvol_alert") {
        addRvolAlert(msg);
        return;
      }
    } catch {}
  };
}
async function postJSON(url, body) {
  const res = await fetch(url, {
    method: "POST",
    headers: {"Content-Type":"application/json"},
    body: JSON.stringify(body)
  });
  try {
    const j = await res.json();
    if (j?.status) setStatus(j.status, !!j.ok);
    return j;
  } catch { return null; }
}
async function initStatus() {
  try {
    const res = await fetch("/api/status", { cache: "no-store" });
    const j = await res.json();
    if (typeof j?.mini_chart_lookback_min === 'number') {
      chartLookbackMin = j.mini_chart_lookback_min;
    }
    if (j?.date) {
      sessionDateET = j.date;
      dateInput.value = j.date;
    } else {
      sessionDateET = todayISO();
    }
    if (j?.running) {
      const map = { pre: "Pre-market", rth: "RTH", pm: "PM" };
      const label = map[j.session] || (j.session ? j.session.toUpperCase() : "Session");
      const suffix = j.startET ? ` from ${j.startET} to ${j.endET} ET` : "";
      setStatus(`${label} running on ${j.date}${suffix}`, true);
    } else {
      setStatus("Stopped");
    }
    // NEW: Sync RVOL settings
    if (j?.rvol) {
      rvolThreshold.value = j.rvol.threshold || 2.0;
      rvolMethod.value = j.rvol.method || "A";
      if (baselineSingle && baselineCumulative) {
        if (j.rvol.baseline_mode === "cumulative") {
          baselineCumulative.checked = true;
        } else {
          baselineSingle.checked = true;
        }
      }
      rvolActive.checked = !!j.rvol.active;
    }
  } catch {
    setStatus("Disconnected");
  }
}
function setPausedUI(v) {
  paused = v;
  btnPause.textContent = paused ? "Resume Alerts (tab)" : "Pause Alerts (tab)";
}
/* ------------ Compact mode toggle (persisted) ------------ */
function setCompact(on){
  document.body.classList.toggle('compact', !!on);
  try { localStorage.setItem('alertcat.compact', on ? '1' : '0'); } catch {}
  chartsLimit = on ? 2 : 5; // fewer auto-charts in compact mode
  renderAll(); // re-render to apply density + limits
}
function initCompact(){
  let on = false;
  try {
    const saved = localStorage.getItem('alertcat.compact');
    on = saved === '1';
  } catch {}
  // URL param can force compact on first load
  if (getParam('compact') === '1') on = true;
  if (chkCompact) chkCompact.checked = on;
  setCompact(on);
}
// ----------------- Init & wiring -----------------
(function init(){
  setStatus("Loading…");
  loadSound().then(() => {});
  loadPins();
  // Default sound ON; prime/resume audio context on first gesture
  if (soundState) soundState.textContent = "Sound ON";
  soundBtn.addEventListener("click", enableSound);
  window.addEventListener('pointerdown', enableSound, { once:true });
  window.addEventListener('keydown',    enableSound, { once:true });
  window.addEventListener('touchstart', enableSound, { once:true });
  // Compact mode first so layout is set before initial renders
  initCompact();
  if (chkCompact) {
    chkCompact.addEventListener('change', (e) => setCompact(!!e.target.checked));
  }
  dateInput.value = todayISO();
  // Start
  btnStart.addEventListener("click", async () => {
    const date = (dateInput.value || "").trim();
    if (!date) return;
    const sessEl = document.querySelector('input[name="session"]:checked');
    const session = sessEl ? sessEl.value : "rth";
    await postJSON("/api/stream", { mode: "start", date, session });
    // wipe UI and cancel any chart timers
    for (const id of Array.from(chartsById.keys())) destroyChart(id);
    feed.innerHTML = "";
    activeChartQueue.splice(0, activeChartQueue.length);
    chartsById.clear();
    allAlerts = [];
    historyLoaded = false;
    recentAlerts = []; // NEW
    renderRecentAlerts(); // NEW
    sessionDateET = date;
    // keep pins; they will re-render when history arrives
    renderAll();
  });
  // Stop
  btnStop.addEventListener("click", async () => {
    await postJSON("/api/stream", { mode: "stop" });
  });
  // Pause alerts in this tab only
  btnPause.addEventListener("click", () => {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    if (paused) {
      ws.send(JSON.stringify({ type: "control", action: "resume" }));
      setPausedUI(false);
    } else {
      ws.send(JSON.stringify({ type: "control", action: "pause" }));
      setPausedUI(true);
    }
  });
  // Clear UI (& server history) — pins are persisted but will be removed if their alerts are gone
  btnClear.addEventListener("click", async () => {
    for (const id of Array.from(chartsById.keys())) destroyChart(id);
    feed.innerHTML = "";
    activeChartQueue.splice(0, activeChartQueue.length);
    chartsById.clear();
    allAlerts = [];
    historyLoaded = false;
    recentAlerts = []; // NEW
    renderRecentAlerts(); // NEW
    try { await postJSON("/api/clear", {}); } catch {}
    renderAll();
  });
  // Reload watchlist (NEW)
  if (btnReloadWL) {
    btnReloadWL.addEventListener("click", async () => {
      await postJSON("/api/watchlist/reload", {}); // server reads watchlist.yaml
    });
  }
  // Unpin all
  if (btnUnpinAll) {
    btnUnpinAll.addEventListener("click", () => {
      pinnedOrder = [];
      pinnedSet = new Set();
      savePins();
      renderAll();
    });
  }
  // Filter toggles: re-render on change
  chkHod.addEventListener("change", renderAll);
  chkLod.addEventListener("change", renderAll);
  // NEW RVOL control listeners
  rvolThreshold.addEventListener("input", () => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({type: "control", action: "set_rvol_threshold", value: parseFloat(rvolThreshold.value)}));
    }
  });
  rvolMethod.addEventListener("change", () => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({type: "control", action: "set_rvol_method", value: rvolMethod.value}));
    }
  });
  document.querySelectorAll('input[name="baseline"]').forEach(r => {
    r.addEventListener("change", () => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({type: "control", action: "set_baseline_mode", value: r.value}));
      }
    });
  });
  rvolActive.addEventListener("change", () => {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({type: "control", action: "set_rvol_active", value: rvolActive.checked}));
    }
  });
  silentMode.addEventListener("change", (e) => {
    silent = e.target.checked;
    if (silent) stopAllSounds();
  });
  connectWS();
  initStatus().then(() => {});
  // Keep live pane sized correctly
  window.addEventListener("resize", () => {
    recomputeLiveCapacity();
  });
})();

function stopAllSounds() {
  // Placeholder for stopping sounds if needed
}
