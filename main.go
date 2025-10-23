// File: main.go
package main

import (
        "strconv"
        "context"
        "bytes"
        "encoding/binary"
    	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
        "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	polygonrest "github.com/polygon-io/client-go/rest"
	rmodels "github.com/polygon-io/client-go/rest/models"
	"gopkg.in/yaml.v3"

	"alertcat/internal/alerts"
	poly "alertcat/internal/polygon"
	rvolpkg "alertcat/internal/rvol"
)

/* ====================
   Config & Inputs
   ==================== */

type AppConfig struct {
	ServerPort int `yaml:"server_port"`
	Alert      struct {
		SoundFile       string `yaml:"sound_file"`
		EnableSound     bool   `yaml:"enable_sound"`
		CooldownSeconds int    `yaml:"cooldown_seconds"`
	} `yaml:"alert"`
	Rvol struct {
		DefaultThreshold float64 `yaml:"default_threshold"`
		LookbackDays     int     `yaml:"lookback_days"`
		BucketSize       string  `yaml:"bucket_size"` // 1m fixed
		DefaultMethod    string  `yaml:"default_method"`
		BaselineMode     string  `yaml:"baseline_mode"`
	} `yaml:"rvol"`
	Timezone string `yaml:"timezone"`
	Logging  struct {
		Level string `yaml:"level"`
	} `yaml:"logging"`
	Persistence struct {
		StateFile string `yaml:"state_file"`
	} `yaml:"persistence"`
}

type WatchEntry struct {
	Symbol string `yaml:"symbol"`
	Name   string `yaml:"name,omitempty"`
}
type WatchlistFile struct {
	Watchlist []WatchEntry `yaml:"watchlist"`
}

func loadYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

func mustET(tz string) *time.Location {
	if strings.TrimSpace(tz) == "" {
		tz = "America/New_York"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("tz load failed (%v); using America/New_York", err)
		loc, _ = time.LoadLocation("America/New_York")
	}
	return loc
}

/* ====================
   Sessions
   ==================== */

type SessionType string

const (
	SessionPre SessionType = "pre" // 04:00–20:00
	SessionRTH SessionType = "rth" // 09:30–20:00
    SessionPM  SessionType = "pm"  // 16:00–20:00
)

func sessionBounds(et *time.Location, date time.Time, sess SessionType) (startET, endET time.Time) {
    y, m, d := date.In(et).Date()
    startH, startM := 4, 0
    switch sess {
    case SessionRTH:
        startH, startM = 9, 30
    case SessionPM:
        startH, startM = 16, 0
    }
    startET = time.Date(y, m, d, startH, startM, 0, 0, et)
    endET = time.Date(y, m, d, 20, 0, 0, 0, et)
    return
}

func etClock(ts time.Time) string { return ts.Format("15:04:05") + " ET" }

/* ====================
   UI messages
   ==================== */

type statusMsg struct {
	Type      string `json:"type"` // "status"
	Level     string `json:"level"`
	Text      string `json:"text"`
}

type alertMsg struct {
	Type   string  `json:"type"`   // "alert"
	Kind   string  `json:"kind"`   // "lod" | "hod"
	Time   string  `json:"time"`   // "HH:MM:SS ET"
	Sym    string  `json:"sym"`
	Name   string  `json:"name,omitempty"`
	Price  float64 `json:"price"`
	TSUnix int64   `json:"ts_unix"` // ms
}

type historyMsg struct {
	Type   string     `json:"type"` // "history"
	Alerts []alertMsg `json:"alerts"`
}

type rvolAlertMsg struct {
	Type     string  `json:"type"` // "rvol_alert"
	Time     string  `json:"time"`
	Sym      string  `json:"sym"`
	Price    float64 `json:"price"`
	Volume   int64   `json:"volume"`
	Baseline float64 `json:"baseline"`
	RVOL     float64 `json:"rvol"`
	Method   string  `json:"method"`
    Delta    float64 `json:"delta,omitempty"` // NEW
}

type rvolHistoryMsg struct {
	Type   string         `json:"type"` // "rvol_history"
	Alerts []rvolAlertMsg `json:"alerts"`
}

type controlMsg struct {
	Type   string `json:"type"`   // "control"
	Action string `json:"action"` // pause/resume/set_rvol_* etc
	Value  any    `json:"value,omitempty"`
}

/* ====================
   Websocket hub
   ==================== */

var wsUpgrader = websocket.Upgrader{
	CheckOrigin:       func(*http.Request) bool { return true },
	EnableCompression: true,
}

type client struct {
	c      *websocket.Conn
	out    chan any
	done   chan struct{}
	paused atomic.Bool
}

type hub struct {
	mu        sync.RWMutex
	clients   map[*client]struct{}
	history   []alertMsg
	rvolHist  []rvolAlertMsg
	limit     int
}

func newHub(limit int) *hub {
	return &hub{
		clients:  make(map[*client]struct{}),
		history:  make([]alertMsg, 0, limit),
		rvolHist: make([]rvolAlertMsg, 0, 200),
		limit:    limit,
	}
}
func (h *hub) addHistory(a alertMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history = append(h.history, a)
	if h.limit > 0 && len(h.history) > h.limit {
		h.history = h.history[len(h.history)-h.limit:]
	}
}
func (h *hub) addRvolHistory(a rvolAlertMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rvolHist = append(h.rvolHist, a)
	if len(h.rvolHist) > 200 {
		h.rvolHist = h.rvolHist[len(h.rvolHist)-200:]
	}
}
func (h *hub) getHistory() (alerts []alertMsg, rvols []rvolAlertMsg) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	aa := make([]alertMsg, len(h.history))
	copy(aa, h.history)
	rv := make([]rvolAlertMsg, len(h.rvolHist))
	copy(rv, h.rvolHist)
	return aa, rv
}
func (h *hub) resetHistories() {
	h.mu.Lock()
	h.history = h.history[:0]
	h.rvolHist = h.rvolHist[:0]
	h.mu.Unlock()
}
func (h *hub) broadcast(v any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select { case c.out <- v: default: }
	}
}
func (h *hub) serveWS(onControl func(cl *client, ctrl controlMsg)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil { return }
		defer conn.Close()
		cl := &client{c: conn, out: make(chan any, 256), done: make(chan struct{})}
		h.mu.Lock()
		h.clients[cl] = struct{}{}
		h.mu.Unlock()

		// writer
		go func() {
			ping := time.NewTicker(45 * time.Second)
			defer ping.Stop()
			for {
				select {
				case v := <-cl.out:
					if cl.paused.Load() {
						if _, ok := v.(statusMsg); !ok {
							continue
						}
					}
					_ = conn.WriteJSON(v)
				case <-ping.C:
					_ = conn.WriteMessage(websocket.PingMessage, nil)
				case <-cl.done:
					return
				}
			}
		}()

		// greet + history
		select { case cl.out <- statusMsg{Type:"status", Level:"info", Text:"Connected"}: default: }
        alerts, rvols := h.getHistory()
        // Always send history payloads, even when empty, to satisfy UI contract.
        select { case cl.out <- historyMsg{Type:"history", Alerts: alerts}: default: }
        select { case cl.out <- rvolHistoryMsg{Type:"rvol_history", Alerts: rvols}: default: }

		// reader
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			return nil
		})
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil { break }
			if mt == websocket.TextMessage {
				var ctrl controlMsg
				if err := json.Unmarshal(data, &ctrl); err == nil && ctrl.Type == "control" {
					switch strings.ToLower(ctrl.Action) {
					case "pause":
						cl.paused.Store(true)
						select { case cl.out <- statusMsg{Type:"status", Level:"info", Text:"Paused (this tab)"}: default: }
					case "resume":
						cl.paused.Store(false)
						select { case cl.out <- statusMsg{Type:"status", Level:"success", Text:"Resumed (this tab)"}: default: }
					default:
						if onControl != nil {
							onControl(cl, ctrl)
						}
					}
				}
			}
		}
		close(cl.done)
		h.mu.Lock()
		delete(h.clients, cl)
		h.mu.Unlock()
	}
}

/* ====================
   One‑minute bars & mini‑chart 5m agg
   ==================== */

type oneMinBar struct {
	TsET time.Time
	O, H, L, C float64
	V          int64
}
type barStore struct {
	mu      sync.RWMutex
	bySym   map[string][]oneMinBar
	et      *time.Location
}

func newBarStore(et *time.Location) *barStore {
	return &barStore{ bySym: make(map[string][]oneMinBar), et: et }
}
func (bs *barStore) reset() {
	bs.mu.Lock()
	bs.bySym = make(map[string][]oneMinBar)
	bs.mu.Unlock()
}
func (bs *barStore) addAM(sym string, a poly.AggregateMinute, et *time.Location) {
	// use end timestamp E as the bar's minute (Polygon's AM is minute aggregate)
	t := time.Unix(0, a.E*int64(time.Millisecond)).In(et)
	min := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, et)
    ob := oneMinBar{TsET: min, O: a.O, H: a.H, L: a.L, C: a.C, V: int64(math.Round(a.V))}

	bs.mu.Lock()
	slice := bs.bySym[sym]
	// append or replace the last minute if same bucket
	if n := len(slice); n > 0 && slice[n-1].TsET.Equal(min) {
		slice[n-1] = ob
	} else {
		slice = append(slice, ob)
		if len(slice) > 1600 { // ~one full session
			slice = slice[len(slice)-1600:]
		}
	}
	bs.bySym[sym] = slice
	bs.mu.Unlock()
}

type fiveBar struct {
	Time  int64   `json:"time"` // epoch seconds for Lightweight Charts
	Open  float64 `json:"open"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}
func floorTo5m(t time.Time) time.Time {
	m := t.Minute()
	b := (m / 5) * 5
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), b, 0, 0, t.Location())
}
func agg5m(slice []oneMinBar, fromET, toET time.Time) []fiveBar {
	if len(slice) == 0 || !fromET.Before(toET) {
		return []fiveBar{}
	}
	type bucket struct{ o,h,l,c float64; ok bool }
	bmap := make(map[int64]*bucket)
	keys := make([]int64, 0, 512)
	for _, m := range slice {
		if m.TsET.Before(fromET) || m.TsET.After(toET) { continue }
		start5 := floorTo5m(m.TsET)
		sec := start5.Unix()
		b := bmap[sec]
		if b == nil {
			b = &bucket{o:m.O, h:m.H, l:m.L, c:m.C, ok:true}
			bmap[sec] = b
			keys = append(keys, sec)
		} else {
			if m.H > b.h { b.h = m.H }
			if m.L < b.l { b.l = m.L }
			b.c = m.C
		}
	}
	sort.Slice(keys, func(i,j int) bool { return keys[i] < keys[j] })
	out := make([]fiveBar, 0, len(keys))
	for _, k := range keys {
		b := bmap[k]
		if b != nil && b.ok {
			out = append(out, fiveBar{Time:k, Open:b.o, High:b.h, Low:b.l, Close:b.c})
		}
	}
	return out
}

/* ====================
   HOD/LOD engine (symbol keyed)
   ==================== */

type instrumentState struct {
	Symbol      string
	Name        string
	LOD         float64
	HOD         float64
	AlertedLow  float64
	AlertedHigh float64
}

type odEngine struct {
	mu            sync.RWMutex
	bySymbol      map[string]*instrumentState
	allowed       map[string]struct{}
	et            *time.Location
	startET       time.Time
	endET         time.Time
	alertsAfterET time.Time
	h             *hub
	eps           float64
}

func newOdEngine(h *hub, et *time.Location, startET, endET, alertsAfterET time.Time) *odEngine {
	return &odEngine{
		bySymbol:      make(map[string]*instrumentState),
		allowed:       make(map[string]struct{}),
		et:            et,
		alertsAfterET: alertsAfterET,
		startET:       startET,
		endET:         endET,
		h:             h,
		eps:           1e-9,
	}
}
func (e *odEngine) setAllowed(symbols []string) {
	m := make(map[string]struct{}, len(symbols))
	for _, s := range symbols {
		ss := strings.ToUpper(strings.TrimSpace(s)); if ss != "" { m[ss] = struct{}{} }
	}
	e.mu.Lock(); e.allowed = m; e.mu.Unlock()
}
func (e *odEngine) upsertSymbol(sym, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sym = strings.ToUpper(strings.TrimSpace(sym))
	st, ok := e.bySymbol[sym]
	if !ok {
		st = &instrumentState{Symbol: sym, Name: name, LOD: math.Inf(1), HOD: math.Inf(-1)}
		e.bySymbol[sym] = st
	} else if st.Name == "" && name != "" {
		st.Name = name
	}
}
func (e *odEngine) trade(sym string, price float64, tsET time.Time) {
	if tsET.Before(e.startET) || tsET.After(e.endET) { return }
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.allowed[sym]; !ok { return }
	st := e.bySymbol[sym]
	if st == nil {
		st = &instrumentState{Symbol: sym, LOD: math.Inf(1), HOD: math.Inf(-1)}
		e.bySymbol[sym] = st
	}
	// suppress for backfill portion
	if tsET.Before(e.alertsAfterET) {
		if price < st.LOD-e.eps { st.LOD = price }
		if price > st.HOD+e.eps { st.HOD = price }
		return
	}

	// init no-alert
	if math.IsInf(st.LOD, 1) && math.IsInf(st.HOD, -1) {
		st.LOD, st.HOD = price, price
		return
	}
	// LOD
	if price < st.LOD-e.eps {
		st.LOD = price
		if st.AlertedLow == 0 || price < st.AlertedLow-e.eps {
			st.AlertedLow = price
			msg := alertMsg{Type:"alert", Kind:"lod", Time:etClock(tsET), Sym:sym, Name:st.Name, Price:price, TSUnix: tsET.UnixNano()/int64(time.Millisecond)}
			e.h.addHistory(msg); e.h.broadcast(msg)
		}
	}
	// HOD
	if price > st.HOD+e.eps {
		st.HOD = price
		if st.AlertedHigh == 0 || price > st.AlertedHigh+e.eps {
			st.AlertedHigh = price
			msg := alertMsg{Type:"alert", Kind:"hod", Time:etClock(tsET), Sym:sym, Name:st.Name, Price:price, TSUnix: tsET.UnixNano()/int64(time.Millisecond)}
			e.h.addHistory(msg); e.h.broadcast(msg)
		}
	}
}

// seedHiLo seeds a symbol's session LOD/HOD without emitting any alert.
// It also sets AlertedLow/AlertedHigh so the next alert only fires on a true breakout above/below the seeded values.
func (e *odEngine) seedHiLo(sym, name string, lod, hod float64) {
    e.mu.Lock()
    defer e.mu.Unlock()
    if _, ok := e.allowed[sym]; !ok { return }
    st := e.bySymbol[sym]
    if st == nil {
        st = &instrumentState{Symbol: sym, LOD: math.Inf(1), HOD: math.Inf(-1)}
        e.bySymbol[sym] = st
    }
    if name != "" && st.Name == "" {
        st.Name = name
    }
    // Only apply when we have real values
    if !math.IsInf(lod, 1) && lod > 0 {
        st.LOD = lod
        st.AlertedLow = lod
    }
    if !math.IsInf(hod, -1) && hod > 0 {
        st.HOD = hod
        st.AlertedHigh = hod
    }
}

/* ====================
   RVOL manager
   ==================== */

type rvolManager struct {
	mu          sync.RWMutex
	cfg         AppConfig
	et          *time.Location
	polygonKey  string
	httpClient  *http.Client
    rest        *polygonrest.Client
	threshold   float64
	method      rvolpkg.Method
	baselineMode string // "cumulative" or "single"
	active      bool
	cooldownSec int

	// per symbol
    baselines   map[string]rvolpkg.Baselines
    lastMinute  map[string]time.Time    // last bar minute
    lastClose   map[string]float64      // last minute's close for delta
	cumuVolumes map[string]map[int]int64 // per day minute cumulative map (session-based)
	lastAlertAt map[string]time.Time
	session     SessionType
	anchorDate  time.Time

	// recent alerts buffer (for new WS clients)
	h *hub
}

func newRvolManager(cfg AppConfig, et *time.Location, polygonKey string, h *hub) *rvolManager {
	m := &rvolManager{
		cfg:          cfg,
		et:           et,
		polygonKey:   polygonKey,
        httpClient:   &http.Client{Timeout: 10 * time.Second},
        rest:         polygonrest.NewWithClient(polygonKey, &http.Client{Timeout: 10 * time.Second}),
		threshold:    cfg.Rvol.DefaultThreshold,
		method:       rvolpkg.Method(strings.ToUpper(strings.TrimSpace(cfg.Rvol.DefaultMethod))),
		baselineMode: strings.ToLower(strings.TrimSpace(cfg.Rvol.BaselineMode)),
		active:       false, // default RVOL inactive (UI unchecked)
		cooldownSec:  maxInt(1, cfg.Alert.CooldownSeconds),
		baselines:    make(map[string]rvolpkg.Baselines),
		lastMinute:   make(map[string]time.Time),
        lastClose:    make(map[string]float64),
		cumuVolumes:  make(map[string]map[int]int64),
		lastAlertAt:  make(map[string]time.Time),
		h:            h,
	}
	if m.method != rvolpkg.MethodB {
		m.method = rvolpkg.MethodA
	}
	return m
}

// fetchPrevClose queries Polygon for the previous minute bar's close when we don't have it cached.
func (m *rvolManager) fetchPrevClose(sym string, currStartET time.Time) (float64, bool) {
    if m.rest == nil {
        return 0, false
    }
    prevStart := currStartET.Add(-1 * time.Minute)

    params := &rmodels.ListAggsParams{
        Ticker:     sym,
        Timespan:   rmodels.Minute,
        Multiplier: 1,
        From:       rmodels.Millis(prevStart),
        // REST 'To' is exclusive; use currStart to include exactly the previous minute bar.
        To: rmodels.Millis(currStartET),
    }
    lim := 2
    asc := rmodels.Asc
    adj := true
    params.Limit = &lim
    params.Order = &asc
    params.Adjusted = &adj

    ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
    defer cancel()

    iter := m.rest.ListAggs(ctx, params)
    var lastClose float64
    found := false
    for iter.Next() {
        a := iter.Item()
        lastClose = a.Close
        found = true
    }
    if err := iter.Err(); err != nil {
        return 0, false
    }
    return lastClose, found
}

// maxInt returns the maximum of a and b.
func maxInt(a, b int) int {
    if a > b {
        return a
    }
    return b
}

// serveStatic wires the static web UI and sound file.
func serveStatic(mux *http.ServeMux, webDir string, soundPath string) {
    abs, _ := filepath.Abs(webDir)
    log.Printf("Serving static from %s", abs)
    // index (no-cache)
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Cache-Control", "no-cache")
        http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
    })
    // assets (cacheable)
    fs := http.FileServer(http.Dir(webDir))
    mux.Handle("/assets/", http.StripPrefix("/assets/", fs))
    // news page
    mux.HandleFunc("/news.html", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Cache-Control", "no-cache")
        http.ServeFile(w, r, filepath.Join(webDir, "news.html"))
    })
    // audio
    mux.HandleFunc("/alert.mp3", func(w http.ResponseWriter, r *http.Request) {
        // Serve configured file if present, else an in-memory WAV beep (extension still /alert.mp3).
        if p := strings.TrimSpace(soundPath); p != "" {
            if st, err := os.Stat(p); err == nil && !st.IsDir() {
                w.Header().Set("Cache-Control", "public, max-age=864000")
                // Set appropriate Content-Type when we serve a real MP3 file
                lp := strings.ToLower(p)
                if strings.HasSuffix(lp, ".mp3") {
                    w.Header().Set("Content-Type", "audio/mpeg")
                }
                http.ServeFile(w, r, p)
                return
            }
        }
        if cachedBeepWAV == nil {
            cachedBeepWAV = synthBeepWAV(400, 880.0, 44100)
        }
        w.Header().Set("Content-Type", "audio/wav")
        w.Header().Set("Cache-Control", "public, max-age=864000")
        _, _ = w.Write(cachedBeepWAV)
    })
}
func (m *rvolManager) setSession(date time.Time, sess SessionType) {
	m.mu.Lock()
	m.session = sess
	m.anchorDate = date
	m.mu.Unlock()
}
func (m *rvolManager) loadBaselines(symbols []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
    // Limit parallel backfills to avoid rate limits / GOAWAYs
    maxParallel := 5
    sem := make(chan struct{}, maxParallel)
    wg := sync.WaitGroup{}
    for _, s := range symbols {
        s := s
        wg.Add(1)
        go func() {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()

            base := time.Second
            for attempt := 0; attempt < 3; attempt++ {
                b, err := rvolpkg.Backfill(ctx, m.httpClient, m.polygonKey, s, m.anchorDate, m.cfg.Rvol.LookbackDays, m.et)
                if err != nil {
                    if ctx.Err() != nil { return }
                    if attempt == 2 {
                        log.Printf("[rvol] backfill %s failed after retries: %v", s, err)
                        return
                    }
                    sleep := base * (1 << attempt)
                    jitter := time.Duration(rand.Int63n(int64(250 * time.Millisecond)))
                    time.Sleep(sleep + jitter)
                    continue
                }
                m.mu.Lock()
                m.baselines[s] = b
                // reset cumulative map
                m.cumuVolumes[s] = make(map[int]int64)
                m.mu.Unlock()
                return
            }
        }()
    }
    wg.Wait()
}
func (m *rvolManager) resetCooldowns() {
	m.mu.Lock()
	m.lastAlertAt = make(map[string]time.Time)
	m.mu.Unlock()
}
func (m *rvolManager) OnAM(sym string, a poly.AggregateMinute, lastPrice float64) {
    // We intentionally use the Polygon 1‑minute candle close for both the displayed price
    // and the delta calculation (current close − prior minute close). The last trade price
    // is not used here; keep the parameter for existing call sites.
    _ = lastPrice
    // Phase 1: compute RVOL & capture state under lock
    m.mu.Lock()
    if !m.active {
        m.mu.Unlock()
        return
    }
    bl := m.baselines[sym]
    if bl == nil {
        m.mu.Unlock()
        return
    }

    t := time.Unix(0, a.E*int64(time.Millisecond)).In(m.et)  // current bar end (minute)
    currStart := time.Unix(0, a.S*int64(time.Millisecond)).In(m.et)

    // bucket since 04:00 ET
    bucket := rvolpkg.MinuteIndexFrom0400ET(t, m.et)
    if bucket < 0 || bucket >= 16*60 {
        m.mu.Unlock()
        return
    }

    // Current volume (single vs cumulative)
    var curVol int64
    aVol := int64(math.Round(a.V))
    session := string(m.session)
    if strings.ToLower(m.baselineMode) == "cumulative" {
        startIdx := rvolpkg.SessionStartIndex(session, m.et, t)
        cumu := m.cumuVolumes[sym]
        if cumu == nil {
            cumu = make(map[int]int64)
            m.cumuVolumes[sym] = cumu
        }
        prev := int64(0)
        if bucket-1 >= startIdx {
            prev = cumu[bucket-1]
        }
        if bucket >= startIdx {
            cumu[bucket] = prev + aVol
            curVol = cumu[bucket]
        } else {
            curVol = 0
        }
    } else {
        curVol = aVol
    }

    rv, baseline := rvolpkg.ComputeRVOL(bl, bucket, curVol, m.method, m.baselineMode, session, m.et)
    if rv <= 0 {
        m.mu.Unlock()
        return
    }

    // Threshold/cooldown
    shouldAlert := false
    if rv >= m.threshold {
        la := m.lastAlertAt[sym]
        if time.Since(la) >= time.Duration(m.cooldownSec)*time.Second {
            shouldAlert = true
        }
    }

    // Try to use cached previous minute close when it's exactly t-1m
    prevClose, havePrev := m.lastClose[sym]
    prevMin := m.lastMinute[sym]
    cachedIsPrev := havePrev && prevMin.Equal(t.Add(-1*time.Minute))

    // Update caches for this bar for next minute
    m.lastMinute[sym] = t
    m.lastClose[sym] = a.C
    meth := string(m.method)
    threshold := m.threshold
    m.mu.Unlock()

    if !shouldAlert {
        return
    }

    // Phase 2: fetch previous minute close if needed (outside lock)
    if !cachedIsPrev {
        if c, ok := m.fetchPrevClose(sym, currStart); ok {
            prevClose = c
            havePrev = true
        }
    }

    // Price and delta strictly from Polygon 1‑minute bars.
    price := a.C
    delta := 0.0
    if havePrev && prevClose > 0 {
        delta = price - prevClose
    }

    // Phase 3: finalize (update cooldown, broadcast, log)
    m.mu.Lock()
    m.lastAlertAt[sym] = time.Now()
    m.mu.Unlock()

    msg := rvolAlertMsg{
        Type:     "rvol_alert",
        Time:     etClock(t),
        Sym:      sym,
        Price:    price,
        Volume:   curVol,
        Baseline: baseline,
        RVOL:     rv,
        Method:   meth,
        Delta:    delta, // NEW
    }
    m.h.addRvolHistory(msg)
    m.h.broadcast(msg)

    _ = alerts.LogToCSV(alerts.Alert{
        Timestamp: time.Now(),
        Symbol:    sym,
        Price:     price,
        Volume:    curVol,
        Baseline:  baseline,
        RVOL:      rv,
        Method:    meth,
        Bucket:    fmt.Sprintf("%02d:%02d", t.Hour(), t.Minute()),
        Threshold: threshold,
    })
}

/* ====================
   HTTP helpers
   ==================== */

func normalizedSoundPath(p string) string {
	if p = strings.TrimSpace(p); p == "" { return "" }
	if filepath.IsAbs(p) { return p }
	abs, err := filepath.Abs(p); if err != nil { return p }
	return abs
}

// Simple in‑memory WAV beep so /alert.mp3 always responds cache‑friendly.
var cachedBeepWAV []byte
func synthBeepWAV(durationMs int, freqHz float64, sampleRate int) []byte {
    if durationMs <= 0 { durationMs = 350 }
    if sampleRate <= 0 { sampleRate = 44100 }
    n := int(float64(durationMs) / 1000.0 * float64(sampleRate))
    samples := make([]int16, n)
    amp := 3000.0
    for i := 0; i < n; i++ {
        t := float64(i) / float64(sampleRate)
        val := amp * math.Sin(2*math.Pi*freqHz*t)
        if val > 32767 { val = 32767 }
        if val < -32768 { val = -32768 }
        samples[i] = int16(val)
    }
    var buf bytes.Buffer
    buf.WriteString("RIFF")
    dataSize := len(samples) * 2
    chunkSize := uint32(36 + dataSize)
    _ = binary.Write(&buf, binary.LittleEndian, chunkSize)
    buf.WriteString("WAVE")
    buf.WriteString("fmt ")
    _ = binary.Write(&buf, binary.LittleEndian, uint32(16))
    _ = binary.Write(&buf, binary.LittleEndian, uint16(1))
    _ = binary.Write(&buf, binary.LittleEndian, uint16(1))
    _ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
    byteRate := uint32(sampleRate * 2)
    _ = binary.Write(&buf, binary.LittleEndian, byteRate)
    _ = binary.Write(&buf, binary.LittleEndian, uint16(2))
    _ = binary.Write(&buf, binary.LittleEndian, uint16(16))
    buf.WriteString("data")
    _ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))
    for _, s := range samples {
        _ = binary.Write(&buf, binary.LittleEndian, s)
    }
    return buf.Bytes()
}

func priorOpenDayStr(dateStr string) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil { return dateStr }
	if t.Weekday() == time.Monday {
		return t.AddDate(0, 0, -3).Format("2006-01-02")
	}
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}
func parseRFC3339Maybe(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil { return t }
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil { return t }
	return time.Time{}
}

var httpClient = &http.Client{Timeout: 8 * time.Second}

type NewsItem struct {
	Title     string `json:"title"`
	Source    string `json:"source"`
	URL       string `json:"url"`
	Published string `json:"published"`
}
type SecFiling struct {
	FiledAt             string `json:"filedAt"`
	FormType            string `json:"formType"`
	Description         string `json:"description"`
	CompanyName         string `json:"companyName"`
	LinkToFilingDetails string `json:"linkToFilingDetails"`
}

// ===== FMP Profile (cached for the day) =====
type ProfileInfo struct {
    Symbol    string  `json:"symbol"`
    MarketCap float64 `json:"marketCap"`
    Country   string  `json:"country"`
    Industry  string  `json:"industry"`
}

var profMu sync.RWMutex
var profBySym = make(map[string]ProfileInfo) // reset on stream start (new session/day)

func fetchFmpProfileCached(fmpKey, symbol string) (ProfileInfo, error) {
    s := strings.ToUpper(strings.TrimSpace(symbol))
    if s == "" {
        return ProfileInfo{}, fmt.Errorf("missing ticker")
    }
    // Cache hit?
    profMu.RLock()
    if p, ok := profBySym[s]; ok {
        profMu.RUnlock()
        return p, nil
    }
    profMu.RUnlock()
    if fmpKey == "" {
        return ProfileInfo{}, fmt.Errorf("FMP_API_KEY is missing")
    }
    u := fmt.Sprintf("https://financialmodelingprep.com/stable/profile?symbol=%s&apikey=%s", s, fmpKey)
    resp, err := httpClient.Get(u)
    if err != nil {
        if resp != nil {
            _ = resp.Body.Close()
        }
        return ProfileInfo{}, err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return ProfileInfo{}, fmt.Errorf("fmp status %d", resp.StatusCode)
    }
    var arr []map[string]any
    if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
        return ProfileInfo{}, err
    }
    info := ProfileInfo{Symbol: s}
    if len(arr) > 0 {
        m := arr[0]
        switch v := m["marketCap"].(type) {
        case float64:
            info.MarketCap = v
        case json.Number:
            if f, e := v.Float64(); e == nil {
                info.MarketCap = f
            }
        case int64:
            info.MarketCap = float64(v)
        case int:
            info.MarketCap = float64(v)
        }
        if c, ok := m["country"].(string); ok {
            info.Country = c
        }
        if ind, ok := m["industry"].(string); ok {
            info.Industry = ind
        }
    }
    // Cache (store even if mostly empty to avoid refetch storms).
    profMu.Lock()
    profBySym[s] = info
    profMu.Unlock()
    return info, nil
}

func fetchPolygonNews(polygonKey, ticker, fromDate, toDateIncl string) []NewsItem {
	if polygonKey == "" { return nil }
	toLt := func(d string) string {
		t, _ := time.Parse("2006-01-02", d)
		return t.Add(24*time.Hour).Format("2006-01-02")
	}(toDateIncl)
	u := fmt.Sprintf("https://api.polygon.io/v2/reference/news?ticker=%s&published_utc.gte=%s&published_utc.lt=%s&limit=50&sort=published_utc.desc&apiKey=%s",
		ticker, fromDate, toLt, polygonKey)
	resp, err := httpClient.Get(u)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil { _ = resp.Body.Close() }
		return nil
	}
	defer resp.Body.Close()
	var pr struct{ Results []map[string]any `json:"results"` }
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil { return nil }
	out := make([]NewsItem, 0, len(pr.Results))
	for _, m := range pr.Results {
		title, _ := m["title"].(string)
		url, _ := m["url"].(string)
		pub, _ := m["published_utc"].(string)
		src := ""
		if pubr, ok := m["publisher"].(map[string]any); ok {
			if name, ok := pubr["name"].(string); ok { src = name }
		}
		if title != "" && url != "" && pub != "" {
			out = append(out, NewsItem{Title: title, Source: src, URL: url, Published: pub})
		}
	}
	return out
}
func fetchFmpNews(fmpKey, ticker, fromDate, toDateIncl string) []NewsItem {
	if fmpKey == "" { return nil }
	u := fmt.Sprintf("https://financialmodelingprep.com/api/v3/stock_news?tickers=%s&from=%s&to=%s&limit=50&apikey=%s",
		ticker, fromDate, toDateIncl, fmpKey)
	resp, err := httpClient.Get(u)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil { _ = resp.Body.Close() }
		return nil
	}
	defer resp.Body.Close()
	var arr []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil { return nil }
	out := make([]NewsItem, 0, len(arr))
	for _, m := range arr {
		title, _ := m["title"].(string)
		url, _ := m["url"].(string)
		pub, _ := m["publishedDate"].(string)
		src, _ := m["site"].(string)
		if title != "" && url != "" && pub != "" {
			out = append(out, NewsItem{Title: title, Source: src, URL: url, Published: pub})
		}
	}
	return out
}
func fetchSecFilings(secKey, ticker, dateStr string) []SecFiling {
	if secKey == "" { return nil }
	body := map[string]any{
		"query": fmt.Sprintf("ticker:%s AND filedAt:[%s TO %s]", ticker, dateStr, dateStr),
        "from":  0, "size": 50,
		"sort":  []map[string]map[string]string{{"filedAt": {"order": "desc"}}},
	}
    b, _ := json.Marshal(body)
    req, _ := http.NewRequest("POST", "https://api.sec-api.io?token="+secKey, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil { _ = resp.Body.Close() }
		return nil
	}
	defer resp.Body.Close()
	var out struct{ Filings []map[string]any `json:"filings"` }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return nil }
	results := make([]SecFiling, 0, len(out.Filings))
	for _, f := range out.Filings {
		results = append(results, SecFiling{
			FiledAt:             strField(f, "filedAt"),
			FormType:            strField(f, "formType"),
			Description:         strField(f, "description"),
			CompanyName:         strField(f, "companyName"),
			LinkToFilingDetails: strField(f, "linkToFilingDetails"),
		})
	}
	sort.Slice(results, func(i, j int) bool {
		return parseRFC3339Maybe(results[i].FiledAt).After(parseRFC3339Maybe(results[j].FiledAt))
	})
	return results
}
func strField(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok { return s }
	}
	return ""
}

/* ====================
   HOD/LOD seeding (session-anchored)
   ==================== */

// seedSessionHiLo fetches today's 1-minute aggregates from Polygon REST between sessStartET and nowET (bounded by sessEndET)
// and initializes each symbol's HOD/LOD to max(High) / min(Low) over that window, without emitting alerts.
func seedSessionHiLo(et *time.Location, polygonKey string, symbols []string, names map[string]string, sessStartET, nowET, sessEndET time.Time, eng *odEngine) {
    // Bound "to" inside the session and ensure window is valid
    if nowET.After(sessEndET) {
        nowET = sessEndET
    }
    if !sessStartET.Before(nowET) {
        // Nothing to seed (e.g., starting before the session anchor)
        return
    }
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    rest := polygonrest.NewWithClient(polygonKey, httpClient)

    maxParallel := 5
    sem := make(chan struct{}, maxParallel)
    wg := sync.WaitGroup{}

    for _, sym := range symbols {
        s := sym
        wg.Add(1)
        go func() {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()

            params := &rmodels.ListAggsParams{
                Ticker:     s,
                Timespan:   rmodels.Minute,
                Multiplier: 1,
                From:       rmodels.Millis(sessStartET),
                // Polygon's REST uses an exclusive upper bound; add 1 minute to include the current minute bar if present.
                To: rmodels.Millis(nowET.Add(1 * time.Minute)),
            }
            lim := 50000
            asc := rmodels.Asc
            adj := true
            params.Limit = &lim
            params.Order = &asc
            params.Adjusted = &adj

            iter := rest.ListAggs(ctx, params)
            minLow := math.Inf(1)
            maxHigh := math.Inf(-1)
            for iter.Next() {
                a := iter.Item() // models.Agg
                // Guard window (paranoia): restrict to [sessStartET, sessEndET]
                ts := time.Time(a.Timestamp).In(et)
                if ts.Before(sessStartET) || ts.After(sessEndET) {
                    continue
                }
                if a.Low < minLow {
                    minLow = a.Low
                }
                if a.High > maxHigh {
                    maxHigh = a.High
                }
            }
            if err := iter.Err(); err != nil {
                log.Printf("[seed H/L] %s: %v", s, err)
                return
            }
            // Only seed if we actually saw data in this window
            if !math.IsInf(minLow, 1) || !math.IsInf(maxHigh, -1) {
                // If only one side exists, mirror to avoid zero values
                if math.IsInf(minLow, 1) {
                    minLow = maxHigh
                }
                if math.IsInf(maxHigh, -1) {
                    maxHigh = minLow
                }
                eng.seedHiLo(s, names[s], minLow, maxHigh)
            }
        }()
    }
    wg.Wait()
}

/* ====================
   main
   ==================== */

func main() {
	portOverride := flag.Int("port", 0, "override server_port")
	flag.Parse()

	_ = godotenv.Load(".env")
	polygonKey := strings.TrimSpace(os.Getenv("POLYGON_API_KEY"))
	if polygonKey == "" {
		log.Fatal("POLYGON_API_KEY is missing (set in .env)")
	}
	fmpKey := strings.TrimSpace(os.Getenv("FMP_API_KEY")) // optional
	secKey := strings.TrimSpace(os.Getenv("SEC_API_KEY")) // optional

	var cfg AppConfig
	if err := loadYAML("config.yaml", &cfg); err != nil {
		log.Fatalf("load config.yaml: %v", err)
	}
	if cfg.ServerPort == 0 {
		if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
			if v, _ := strconv.Atoi(p); v > 0 { cfg.ServerPort = v }
		}
		if cfg.ServerPort == 0 { cfg.ServerPort = 8089 }
	}
	if *portOverride != 0 { cfg.ServerPort = *portOverride }
	if cfg.Alert.CooldownSeconds <= 0 { cfg.Alert.CooldownSeconds = 60 }
	if cfg.Rvol.LookbackDays <= 0 { cfg.Rvol.LookbackDays = 14 }
	if strings.TrimSpace(cfg.Rvol.BucketSize) == "" { cfg.Rvol.BucketSize = "1m" }
	if strings.TrimSpace(cfg.Rvol.DefaultMethod) == "" { cfg.Rvol.DefaultMethod = "A" }
	if strings.TrimSpace(cfg.Rvol.BaselineMode) == "" { cfg.Rvol.BaselineMode = "single" }

	var wl WatchlistFile
	if err := loadYAML("watchlist.yaml", &wl); err != nil {
		log.Fatalf("load watchlist.yaml: %v", err)
	}
	if len(wl.Watchlist) == 0 {
		log.Fatal("watchlist is empty")
	}
	var symbols []string
	nameBySymbol := make(map[string]string)
	seen := make(map[string]struct{})
	for _, w := range wl.Watchlist {
		s := strings.ToUpper(strings.TrimSpace(w.Symbol))
		if s == "" { continue }
		if _, dup := seen[s]; dup { continue }
		seen[s] = struct{}{}
		symbols = append(symbols, s)
		if w.Name != "" { nameBySymbol[s] = w.Name }
	}

	et := mustET(cfg.Timezone)

	// Hub + WS server (with RVOL control handling)
	h := newHub(500)

	// RVOL manager
	rvm := newRvolManager(cfg, et, polygonKey, h)

	// web mux
	mux := http.NewServeMux()
	serveStatic(mux, "web", normalizedSoundPath(cfg.Alert.SoundFile))
	mux.HandleFunc("/ws", h.serveWS(func(cl *client, ctrl controlMsg) {
		switch strings.ToLower(ctrl.Action) {
		case "set_rvol_threshold":
			v, _ := ctrl.Value.(float64)
			if v <= 0 { v = 2.0 }
			rvm.mu.Lock()
			rvm.threshold = v
			rvm.mu.Unlock()
			rvm.resetCooldowns()
			select { case cl.out <- statusMsg{Type:"status", Level:"success", Text:fmt.Sprintf("RVOL threshold set to %.2f", v)}: default: }
		case "set_rvol_method":
			s, _ := ctrl.Value.(string)
			s = strings.ToUpper(strings.TrimSpace(s))
			if s != "B" { s = "A" }
			rvm.mu.Lock(); rvm.method = rvolpkg.Method(s); rvm.mu.Unlock()
			rvm.resetCooldowns()
			select { case cl.out <- statusMsg{Type:"status", Level:"success", Text:"RVOL method set to "+s}: default: }
		case "set_baseline_mode":
			s, _ := ctrl.Value.(string)
			if s != "single" { s = "cumulative" }
			rvm.mu.Lock(); rvm.baselineMode = s; rvm.mu.Unlock()
			rvm.resetCooldowns()
			select { case cl.out <- statusMsg{Type:"status", Level:"success", Text:"Baseline mode: "+s}: default: }
		case "set_rvol_active":
			b, _ := ctrl.Value.(bool)
			rvm.mu.Lock(); rvm.active = b; rvm.mu.Unlock()
			select { case cl.out <- statusMsg{Type:"status", Level:"success", Text:fmt.Sprintf("RVOL %v", map[bool]string{true:"enabled", false:"disabled"}[b])}: default: }
		}
	}))

	// API: start/stop stream (Polygon Broker lifecycle is tied to stream start/stop)
	type streamReq struct {
		Mode    string `json:"mode"`              // "start" | "stop"
		Date    string `json:"date,omitempty"`    // YYYY-MM-DD
		Session string `json:"session,omitempty"` // "pre" | "rth"
	}
	type streamResp struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
    var streamCancel context.CancelFunc
    var streamCtx context.Context
    var broker *poly.Broker
    var bars = newBarStore(et)
    var eng *odEngine
    var lastPrice sync.Map // symbol -> price (float64) used in RVOL alert
    var subsMu sync.Mutex
    subs := make(map[string]*poly.Subscription) // symbol -> subscription

    // consumer helper
    startConsumer := func(ctx context.Context, sym string) *poly.Subscription {
        sub := broker.Subscribe(sym)
        go func(sym string, sub *poly.Subscription) {
            // pull from both channels
            for {
                select {
                case t := <-sub.Trades:
                    ts := time.Unix(0, t.T*int64(time.Millisecond)).In(et)
                    lastPrice.Store(sym, t.P)
                    eng.upsertSymbol(sym, nameBySymbol[sym])
                    eng.trade(sym, t.P, ts)
                case am := <-sub.Minutes:
                    // store in per-minute cache and feed RVOL
                    bars.addAM(sym, am, et)
                    lp := 0.0
                    if v, ok := lastPrice.Load(sym); ok {
                        if p, ok2 := v.(float64); ok2 { lp = p }
                    }
                    rvm.OnAM(sym, am, lp)
                case <-sub.Done():
                    return
                case <-ctx.Done():
                    return
                }
            }
        }(sym, sub)
        return sub
    }

	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed); return
		}
		var req streamReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest); return
		}
		switch strings.ToLower(req.Mode) {
        case "stop":
			if streamCancel != nil {
				streamCancel()
				streamCancel = nil
			}
            // Unsubscribe all active subs and clear
            if broker != nil {
                subsMu.Lock()
                for sym, sub := range subs {
                    broker.Unsubscribe(sub)
                    broker.RemoveSymbol(sym)
                }
                subs = make(map[string]*poly.Subscription)
                subsMu.Unlock()
            }
			// clear profile cache when stopping (safe)
			profMu.Lock()
			profBySym = make(map[string]ProfileInfo)
			profMu.Unlock()
			h.broadcast(statusMsg{Type:"status", Level:"info", Text:"Stopped"})
			_ = json.NewEncoder(w).Encode(streamResp{OK:true, Status:"Stopped"})
		case "start":
			if streamCancel != nil {
				streamCancel()
				streamCancel = nil
			}
			// parse date
			dt := time.Now().In(et)
			if strings.TrimSpace(req.Date) != "" {
				t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(req.Date), et)
				if err == nil { dt = t }
			}
            var sess SessionType
            switch strings.ToLower(strings.TrimSpace(req.Session)) {
            case "pre":
                sess = SessionPre
            case "pm":
                sess = SessionPM
            default:
                sess = SessionRTH
            }
			startET, endET := sessionBounds(et, dt, sess)
			// reset stores
			bars.reset()
			h.resetHistories()
			// reset FMP profile cache for the new trading day/session
			profMu.Lock()
			profBySym = make(map[string]ProfileInfo)
			profMu.Unlock()
            // IMPORTANT: HOD/LOD begins at 16:06 when PM is selected
            odStartET := startET
            if sess == SessionPre || sess == SessionPM {
                odStartET = odStartET.Add(6 * time.Minute) // 16:06 ET or 04:06 ET for pre
            }
            eng = newOdEngine(h, et, odStartET, endET, time.Now().In(et))
			eng.setAllowed(symbols)

			// Seed HOD/LOD from session start (04:06/09:30/16:06 ET) up to now so alerts reflect the true session range,
			// not "since program start".
			seedSessionHiLo(et, polygonKey, symbols, nameBySymbol, odStartET, time.Now().In(et), endET, eng)

			// RVOL
			rvm.setSession(dt, sess)
			rvm.loadBaselines(symbols) // load baselines for current watchlist

            // broker ctx
            streamCtx, streamCancel = context.WithCancel(context.Background())
			broker = poly.NewBroker(polygonKey)
            // subscribe per symbol (with consumers)
            subsMu.Lock()
            subs = make(map[string]*poly.Subscription)
            for _, s := range symbols {
                subs[s] = startConsumer(streamCtx, s)
            }
            subsMu.Unlock()
			// run broker
			go func() {
                if err := broker.Run(streamCtx); err != nil && streamCtx.Err() == nil {
					log.Printf("[polygon] broker stopped: %v", err)
				}
			}()
            label := map[SessionType]string{SessionPre:"Pre‑market", SessionRTH:"RTH", SessionPM:"PM"}[sess]
			h.broadcast(statusMsg{Type:"status", Level:"success", Text:fmt.Sprintf("%s started (%s–%s ET)", label, startET.Format("15:04"), endET.Format("15:04"))})
			_ = json.NewEncoder(w).Encode(streamResp{OK:true, Status:"Stream starting"})
		default:
			_ = json.NewEncoder(w).Encode(streamResp{OK:false, Status:"Unknown mode"})
		}
	})

	// API: status
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		type resp struct {
			Running              bool        `json:"running"`
			Session              string      `json:"session"`
			Date                 string      `json:"date"`
			StartET              string      `json:"startET"`
			EndET                string      `json:"endET"`
			Port                 int         `json:"port"`
			MiniChartLookbackMin int         `json:"mini_chart_lookback_min"`
			RVOL                 interface{} `json:"rvol"`
		}
		out := resp{
			Running:              streamCancel != nil,
			Session:              "", Date: "", StartET: "", EndET: "",
			Port:                 cfg.ServerPort,
			MiniChartLookbackMin: 120,
			RVOL: map[string]any{
				"threshold":     rvm.threshold,
				"method":        string(rvm.method),
				"baseline_mode": rvm.baselineMode,
				"active":        rvm.active,
			},
		}
		rvm.mu.RLock()
		date := rvm.anchorDate
		sess := rvm.session
		rvm.mu.RUnlock()
		if !date.IsZero() {
			out.Date = date.Format("2006-01-02")
			s0, e0 := sessionBounds(et, date, sess)
			out.Session = string(sess)
			out.StartET = s0.Format("15:04")
			out.EndET = e0.Format("15:04")
		}
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(out)
	})

	// API: clear history
	mux.HandleFunc("/api/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { http.Error(w, "POST only", http.StatusMethodNotAllowed); return }
		h.resetHistories()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// API: company profile (marketCap/country/industry) — cached per day
	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q := r.URL.Query()
		ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
		if ticker == "" {
			http.Error(w, "ticker required", http.StatusBadRequest)
			return
		}
		info, err := fetchFmpProfileCached(fmpKey, ticker)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"symbol":    info.Symbol,
			"marketCap": info.MarketCap,
			"country":   info.Country,
			"industry":  info.Industry,
		})
	})

	// API: mini chart bars
	mux.HandleFunc("/api/bars", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q := r.URL.Query()
		sym := strings.ToUpper(strings.TrimSpace(q.Get("symbol")))
		atms := strings.TrimSpace(q.Get("at"))
		minsStr := strings.TrimSpace(q.Get("mins"))
		windowMin := 120
		if m, err := strconv.Atoi(minsStr); err == nil && m > 0 && m <= 720 {
			windowMin = m
		}
		atUnix, err := strconv.ParseInt(atms, 10, 64)
		var atET time.Time
		if err != nil || atUnix <= 0 {
			// Fall back to "now" in ET for robustness
			atET = time.Now().In(et)
		} else {
			atET = time.Unix(0, atUnix*int64(time.Millisecond)).In(et)
		}

		// get slices
		bars.mu.RLock()
		minsSlice := append([]oneMinBar(nil), bars.bySym[sym]...)
		bars.mu.RUnlock()

		nowStart, nowEnd := time.Time{}, time.Time{}
		// bound inside session if running
		rvm.mu.RLock()
		date, sess := rvm.anchorDate, rvm.session
		rvm.mu.RUnlock()
		if !date.IsZero() {
			nowStart, nowEnd = sessionBounds(et, date, sess)
		}
		to := atET
		if !nowEnd.IsZero() && to.After(nowEnd) { to = nowEnd }
		if !nowStart.IsZero() && to.Before(nowStart) { to = nowStart }
		from := to.Add(-time.Duration(windowMin) * time.Minute)
		if !nowStart.IsZero() && from.Before(nowStart) { from = nowStart }

		fives := agg5m(minsSlice, from, to)
		type resp struct {
			OK     bool      `json:"ok"`
			Symbol string    `json:"symbol"`
			Bars   []fiveBar `json:"bars"`
		}
		_ = json.NewEncoder(w).Encode(resp{OK: true, Symbol: sym, Bars: fives})
	})

	// API: news + SEC
	mux.HandleFunc("/api/extra", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q := r.URL.Query()
		ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
		dateStr := strings.TrimSpace(q.Get("date"))
		if ticker == "" || dateStr == "" {
			http.Error(w, "ticker and date required", http.StatusBadRequest); return
		}
		from := priorOpenDayStr(dateStr)
		var news []NewsItem
		news = append(news, fetchPolygonNews(polygonKey, ticker, from, dateStr)...)
		news = append(news, fetchFmpNews(fmpKey, ticker, from, dateStr)...)
		// dedupe by URL
		uniq := map[string]NewsItem{}
		for _, n := range news { uniq[n.URL] = n }
		news = news[:0]
		for _, v := range uniq { news = append(news, v) }
		sort.Slice(news, func(i,j int) bool { return parseRFC3339Maybe(news[i].Published).After(parseRFC3339Maybe(news[j].Published)) })

		filings := fetchSecFilings(secKey, ticker, dateStr)
		_ = json.NewEncoder(w).Encode(map[string]any{"news": news, "filings": filings})
	})

	// Watchlist helpers
	mux.HandleFunc("/api/watchlist", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"symbols": symbols})
			return
		}
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/watchlist/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost { http.Error(w, "POST only", http.StatusMethodNotAllowed); return }
		// reload watchlist.yaml
		var wl WatchlistFile
		if err := loadYAML("watchlist.yaml", &wl); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "status": "Reload failed: " + err.Error()})
			return
		}
		var nsymbols []string
		nnames := map[string]string{}
		seen := map[string]struct{}{}
		for _, wle := range wl.Watchlist {
			s := strings.ToUpper(strings.TrimSpace(wle.Symbol))
			if s == "" { continue }
			if _, ok := seen[s]; ok { continue }
			seen[s] = struct{}{}
			nsymbols = append(nsymbols, s)
			if wle.Name != "" { nnames[s] = wle.Name }
		}
		// diff
		oldSet := map[string]struct{}{}; for _, s := range symbols { oldSet[s] = struct{}{} }
		newSet := map[string]struct{}{}; for _, s := range nsymbols { newSet[s] = struct{}{} }
		var added, removed, kept []string
		for s := range newSet { if _, ok := oldSet[s]; ok { kept = append(kept, s) } else { added = append(added, s) } }
		for s := range oldSet { if _, ok := newSet[s]; !ok { removed = append(removed, s) } }

        // update in-memory
        symbols = nsymbols
        nameBySymbol = nnames
        if eng != nil { eng.setAllowed(symbols) }
        if broker != nil {
            // Start consumers for ADDED under current stream context
            if len(added) > 0 {
                subsMu.Lock()
                for _, s := range added {
                    if _, exists := subs[s]; !exists {
                        subs[s] = startConsumer(streamCtx, s)
                    }
                }
                subsMu.Unlock()
            }
            // Unsubscribe REMOVED
            if len(removed) > 0 {
                subsMu.Lock()
                for _, s := range removed {
                    if sub, ok := subs[s]; ok {
                        broker.Unsubscribe(sub)
                        broker.RemoveSymbol(s)
                        delete(subs, s)
                    } else {
                        // Ensure broker won't resubscribe after reconnects
                        broker.RemoveSymbol(s)
                    }
                }
                subsMu.Unlock()
            }
        }
		// reload baselines for added symbols
		if len(added) > 0 {
			go rvm.loadBaselines(added)
		}

        status := fmt.Sprintf("Watchlist reloaded: +%d / -%d (kept %d)", len(added), len(removed), len(kept))
        _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": status, "added": added, "removed": removed, "kept": kept})
        h.broadcast(statusMsg{Type:"status", Level:"info", Text: status})
	})

	// serve static
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.ServerPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("UI: http://localhost:%d (sound: /alert.mp3)", cfg.ServerPort)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}
