package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	poly "alertcat/internal/polygon"
	rvolpkg "alertcat/internal/rvol"
)

func TestServeStaticSoundEndpointsServeConfiguredFiles(t *testing.T) {
	webDir := t.TempDir()
	mustWriteFile(t, filepath.Join(webDir, "index.html"), []byte("ok"))
	mustWriteFile(t, filepath.Join(webDir, "news.html"), []byte("ok"))

	up := []byte("ID3-up-sound")
	down := []byte("ID3-down-sound")
	scalp := []byte("ID3-scalp-sound")
	upPath := filepath.Join(webDir, "up.mp3")
	downPath := filepath.Join(webDir, "down.mp3")
	scalpPath := filepath.Join(webDir, "scalp.mp3")
	mustWriteFile(t, upPath, up)
	mustWriteFile(t, downPath, down)
	mustWriteFile(t, scalpPath, scalp)

	mux := http.NewServeMux()
	serveStatic(mux, webDir, upPath, downPath, scalpPath)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		path string
		want []byte
	}{
		{path: "/alert.mp3", want: up},
		{path: "/alert-up.mp3", want: up},
		{path: "/alert-down.mp3", want: down},
		{path: "/scalp.mp3", want: scalp},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "audio/mpeg") {
				t.Fatalf("Content-Type = %q, want audio/mpeg", got)
			}
			if got := resp.Header.Get("Cache-Control"); got != "public, max-age=864000" {
				t.Fatalf("Cache-Control = %q, want public, max-age=864000", got)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !bytes.Equal(body, tc.want) {
				t.Fatalf("response body mismatch for %s", tc.path)
			}
		})
	}
}

func TestServeStaticSoundFallbackIsStableWAV(t *testing.T) {
	webDir := t.TempDir()
	mustWriteFile(t, filepath.Join(webDir, "index.html"), []byte("ok"))
	mustWriteFile(t, filepath.Join(webDir, "news.html"), []byte("ok"))

	cachedBeepWAV = nil
	want := synthBeepWAV(400, 880.0, 44100)

	mux := http.NewServeMux()
	serveStatic(mux, webDir, "", "", "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []string{"/alert.mp3", "/alert-up.mp3", "/alert-down.mp3", "/scalp.mp3"}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "audio/wav") {
				t.Fatalf("Content-Type = %q, want audio/wav", got)
			}
			if got := resp.Header.Get("Cache-Control"); got != "public, max-age=864000" {
				t.Fatalf("Cache-Control = %q, want public, max-age=864000", got)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !bytes.Equal(body, want) {
				t.Fatalf("fallback body mismatch for %s", path)
			}
		})
	}
}

func TestSynthBeepWAVHasExpectedSignalShapeAndAmplitude(t *testing.T) {
	wav := synthBeepWAV(400, 880.0, 44100)
	if len(wav) < 44 {
		t.Fatalf("wav too short: %d", len(wav))
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		t.Fatalf("invalid wav header")
	}

	sampleRate := binary.LittleEndian.Uint32(wav[24:28])
	if sampleRate != 44100 {
		t.Fatalf("sampleRate = %d, want 44100", sampleRate)
	}
	bitsPerSample := binary.LittleEndian.Uint16(wav[34:36])
	if bitsPerSample != 16 {
		t.Fatalf("bitsPerSample = %d, want 16", bitsPerSample)
	}
	dataSize := binary.LittleEndian.Uint32(wav[40:44])
	if int(dataSize) != len(wav)-44 {
		t.Fatalf("dataSize = %d, want %d", dataSize, len(wav)-44)
	}
	sampleCount := int(dataSize) / 2
	if sampleCount != 17640 { // 400ms @ 44.1kHz
		t.Fatalf("sampleCount = %d, want 17640", sampleCount)
	}

	maxAbs := 0
	for i := 44; i+1 < len(wav); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(wav[i : i+2])))
		if v < 0 {
			v = -v
		}
		if v > maxAbs {
			maxAbs = v
		}
	}
	if maxAbs < 2900 || maxAbs > 3000 {
		t.Fatalf("max amplitude = %d, want in [2900,3000]", maxAbs)
	}
}

func TestOdEngineTradeAlertsOnlyOnTrueBreakouts(t *testing.T) {
	et := mustET("America/New_York")
	start := time.Date(2026, time.March, 2, 9, 30, 0, 0, et)
	end := start.Add(6 * time.Hour)

	h := newHub(50)
	eng := newOdEngine(h, et, start, end, start, "hod", "lod", true)
	eng.setAllowed([]string{"ABC"})
	eng.upsertSymbol("ABC", "ABC Co", []string{"watchlist"})

	eng.trade("ABC", 10.00, start.Add(1*time.Second)) // initialize only
	eng.trade("ABC", 11.00, start.Add(2*time.Second)) // HOD alert
	eng.trade("ABC", 11.00, start.Add(3*time.Second)) // no duplicate
	eng.trade("ABC", 10.50, start.Add(4*time.Second)) // no alert
	eng.trade("ABC", 9.00, start.Add(5*time.Second))  // LOD alert
	eng.trade("ABC", 9.00, start.Add(6*time.Second))  // no duplicate
	eng.trade("ABC", 8.80, start.Add(7*time.Second))  // new LOD alert
	eng.trade("ABC", 12.00, start.Add(8*time.Second)) // new HOD alert

	alerts, _ := h.getHistory()
	if len(alerts) != 4 {
		t.Fatalf("alerts len = %d, want 4", len(alerts))
	}

	gotKinds := []string{alerts[0].Kind, alerts[1].Kind, alerts[2].Kind, alerts[3].Kind}
	wantKinds := []string{"hod", "lod", "lod", "hod"}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("kinds = %v, want %v", gotKinds, wantKinds)
	}
}

func TestOdEngineSeedHiLoRequiresTrueBreakout(t *testing.T) {
	et := mustET("America/New_York")
	start := time.Date(2026, time.March, 2, 9, 30, 0, 0, et)
	end := start.Add(6 * time.Hour)

	h := newHub(50)
	eng := newOdEngine(h, et, start, end, start, "hod", "lod", true)
	eng.setAllowed([]string{"ABC"})
	eng.upsertSymbol("ABC", "ABC Co", []string{"watchlist"})
	eng.seedHiLo("ABC", "ABC Co", []string{"watchlist"}, 9.0, 11.0)

	eng.trade("ABC", 11.00, start.Add(1*time.Second)) // at seed high, no alert
	eng.trade("ABC", 9.00, start.Add(2*time.Second))  // at seed low, no alert
	eng.trade("ABC", 11.01, start.Add(3*time.Second)) // breakout high
	eng.trade("ABC", 8.99, start.Add(4*time.Second))  // breakout low

	alerts, _ := h.getHistory()
	if len(alerts) != 2 {
		t.Fatalf("alerts len = %d, want 2", len(alerts))
	}
	if alerts[0].Kind != "hod" || alerts[1].Kind != "lod" {
		t.Fatalf("unexpected alert kinds: %#v", alerts)
	}
}

func TestNormalizeLevelsModeAndClockHelpers(t *testing.T) {
	levelTests := []struct {
		in   string
		want string
	}{
		{in: "local", want: "local"},
		{in: " LoCaL ", want: "local"},
		{in: "session", want: "session"},
		{in: "", want: "session"},
		{in: "anything-else", want: "session"},
	}
	for _, tc := range levelTests {
		if got := normalizeLevelsMode(tc.in); got != tc.want {
			t.Fatalf("normalizeLevelsMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	clockTests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "09:30", want: "09:30", wantOK: true},
		{in: "16:06", want: "16:06", wantOK: true},
		{in: "9:30", want: "09:30", wantOK: true},
		{in: "24:00", wantOK: false},
		{in: "", wantOK: false},
	}
	for _, tc := range clockTests {
		got, ok := normalizeClockHHMM(tc.in)
		if ok != tc.wantOK {
			t.Fatalf("normalizeClockHHMM(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
		}
		if tc.wantOK && got != tc.want {
			t.Fatalf("normalizeClockHHMM(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClockOnDateETUsesETCalendarDay(t *testing.T) {
	et := mustET("America/New_York")
	// 01:00 UTC on Mar 2 is still Mar 1 in ET.
	dateUTC := time.Date(2026, time.March, 2, 1, 0, 0, 0, time.UTC)

	got, ok := clockOnDateET(dateUTC, et, "09:30")
	if !ok {
		t.Fatalf("clockOnDateET returned ok=false")
	}
	want := time.Date(2026, time.March, 1, 9, 30, 0, 0, et)
	if !got.Equal(want) {
		t.Fatalf("clockOnDateET = %v, want %v", got, want)
	}
}

func TestOdEngineLocalModeAlertsAfterBoundaryWithLocalKinds(t *testing.T) {
	et := mustET("America/New_York")
	start := time.Date(2026, time.March, 2, 10, 0, 0, 0, et)
	end := time.Date(2026, time.March, 2, 20, 0, 0, 0, et)
	alertsAfter := time.Date(2026, time.March, 2, 10, 30, 0, 0, et)

	h := newHub(50)
	eng := newOdEngine(h, et, start, end, alertsAfter, "lhigh", "llow", true)
	eng.setAllowed([]string{"ABC"})
	eng.upsertSymbol("ABC", "ABC Co", []string{"watchlist"})

	// Before alertsAfter: update local hi/low state but never emit.
	eng.trade("ABC", 10.00, start.Add(5*time.Minute))
	eng.trade("ABC", 10.50, start.Add(10*time.Minute))
	eng.trade("ABC", 9.80, start.Add(20*time.Minute))
	eng.trade("ABC", 10.70, start.Add(29*time.Minute))

	// At/after alertsAfter: only true breakouts should emit local kinds.
	eng.trade("ABC", 10.70, start.Add(31*time.Minute)) // equal high, no alert
	eng.trade("ABC", 10.71, start.Add(32*time.Minute)) // local high alert
	eng.trade("ABC", 9.79, start.Add(33*time.Minute))  // local low alert

	alerts, _ := h.getHistory()
	if len(alerts) != 2 {
		t.Fatalf("alerts len = %d, want 2", len(alerts))
	}
	if alerts[0].Kind != "lhigh" || alerts[1].Kind != "llow" {
		t.Fatalf("unexpected local alert kinds: %#v", alerts)
	}
	if alerts[0].Name != "ABC Co" || len(alerts[0].Sources) != 1 || alerts[0].Sources[0] != "watchlist" {
		t.Fatalf("unexpected metadata on local high alert: %#v", alerts[0])
	}
}

func TestOdEngineLocalModeHonorsEnabledToggle(t *testing.T) {
	et := mustET("America/New_York")
	start := time.Date(2026, time.March, 2, 9, 30, 0, 0, et)
	end := start.Add(6 * time.Hour)

	h := newHub(50)
	eng := newOdEngine(h, et, start, end, start, "lhigh", "llow", false)
	eng.setAllowed([]string{"ABC"})
	eng.upsertSymbol("ABC", "ABC Co", []string{"watchlist"})

	eng.trade("ABC", 10.00, start.Add(1*time.Second))
	eng.trade("ABC", 11.00, start.Add(2*time.Second))

	eng.setEnabled(true)
	eng.trade("ABC", 12.00, start.Add(3*time.Second)) // initialize only once enabled
	eng.trade("ABC", 12.10, start.Add(4*time.Second)) // breakout -> alert

	alerts, _ := h.getHistory()
	if len(alerts) != 1 {
		t.Fatalf("alerts len = %d, want 1", len(alerts))
	}
	if alerts[0].Kind != "lhigh" {
		t.Fatalf("alert kind = %q, want lhigh", alerts[0].Kind)
	}
}

func TestRvolManagerOnAMRespectsCooldown(t *testing.T) {
	tmp := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	et := mustET("America/New_York")
	var cfg AppConfig
	cfg.Alert.CooldownSeconds = 10
	cfg.Rvol.DefaultThreshold = 2.0
	cfg.Rvol.DefaultMethod = "A"
	cfg.Rvol.BaselineMode = "single"

	h := newHub(50)
	m := newRvolManager(cfg, et, "", h)

	t1 := time.Date(2026, time.March, 2, 9, 31, 0, 0, et)
	t2 := t1.Add(1 * time.Minute)
	t3 := t2.Add(1 * time.Minute)
	b1 := rvolpkg.MinuteIndexFrom0400ET(t1, et)
	b2 := rvolpkg.MinuteIndexFrom0400ET(t2, et)
	b3 := rvolpkg.MinuteIndexFrom0400ET(t3, et)

	m.mu.Lock()
	m.active = true
	m.session = SessionRTH
	m.baselines["ABC"] = rvolpkg.Baselines{
		b1: {100},
		b2: {100},
		b3: {100},
	}
	m.lastMinute["ABC"] = t1.Add(-1 * time.Minute)
	m.lastClose["ABC"] = 9.50
	m.mu.Unlock()

	m.OnAM("ABC", mkAM("ABC", t1, 250, 10.00), 10.00) // alerts
	m.OnAM("ABC", mkAM("ABC", t2, 260, 10.20), 10.20) // blocked by cooldown

	m.mu.Lock()
	m.lastAlertAt["ABC"] = time.Now().Add(-11 * time.Second) // expire cooldown
	m.mu.Unlock()
	m.OnAM("ABC", mkAM("ABC", t3, 270, 10.40), 10.40) // alerts again

	_, rvols := h.getHistory()
	if len(rvols) != 2 {
		t.Fatalf("rvol alerts len = %d, want 2", len(rvols))
	}
	if rvols[0].Sym != "ABC" || rvols[1].Sym != "ABC" {
		t.Fatalf("unexpected symbols: %#v", rvols)
	}
	if rvols[0].RVOL < 2.0 || rvols[1].RVOL < 2.0 {
		t.Fatalf("expected RVOL >= threshold, got %#v", rvols)
	}
}

func TestWebSoundRoutingAndLoudnessContract(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("web", "app.js"))
	if err != nil {
		t.Fatalf("read web/app.js: %v", err)
	}
	src := string(js)

	requiredJS := []string{
		"let soundEnabled = true;",
		"function addIncomingAlert(a){",
		`a.kind === "lod" || a.kind === "llow"`,
		"playScalpSound();",
		"playDownSound();",
		"playUpSound();",
		"function addRvolAlert(msg) {",
		"msg.delta < 0",
	}
	for _, frag := range requiredJS {
		if !strings.Contains(src, frag) {
			t.Fatalf("web/app.js missing sound-routing contract fragment: %q", frag)
		}
	}

	forbiddenJS := []string{
		"createGain(",
		".volume =",
		"playbackRate =",
	}
	for _, frag := range forbiddenJS {
		if strings.Contains(src, frag) {
			t.Fatalf("web/app.js contains sound-altering fragment: %q", frag)
		}
	}

	html, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		t.Fatalf("read web/index.html: %v", err)
	}
	index := string(html)
	requiredHTML := []string{
		`<audio id="alertAudioUp" src="/alert.mp3"`,
		`<audio id="alertAudioDown" src="/alert-down.mp3"`,
	}
	for _, frag := range requiredHTML {
		if !strings.Contains(index, frag) {
			t.Fatalf("web/index.html missing audio fallback fragment: %q", frag)
		}
	}
}

func mkAM(sym string, endET time.Time, vol float64, close float64) poly.AggregateMinute {
	startET := endET.Add(-1 * time.Minute)
	return poly.AggregateMinute{
		Ev:  "AM",
		Sym: sym,
		V:   vol,
		C:   close,
		S:   startET.UnixMilli(),
		E:   endET.UnixMilli(),
	}
}

func mustWriteFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
