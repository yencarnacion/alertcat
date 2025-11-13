package scalps

import (
	"math"
	"sync"
	"time"
)

type Kind string
type Phase string

const (
	KindRubberband      Kind = "rubberband"
	KindRubberbandUp    Kind = "rubberband_up"
	KindBackside        Kind = "backside"
	KindFashionablyLate Kind = "fashionably_late"

	PhaseSetup   Phase = "setup"
	PhaseTrigger Phase = "trigger"
)

type OneMinBar struct {
	Time  time.Time
	Open  float64
	High  float64
	Low   float64
	Close float64
	Vol   float64
}

type Alert struct {
	Kind  Kind   `json:"kind"`
	Phase Phase  `json:"phase"`
	Sym   string `json:"sym"`
	Time  string `json:"time"`
	Price float64 `json:"price"`
	Info  string `json:"info,omitempty"`
}

// Config holds tunables; defaults chosen from user specs.
type Config struct {
	// VWAP bands are based on VWAP ± k * stddev of (typical price) from session start.
	// These scalars are roughly aligned with common "1st/2nd band" settings.
	Band1K float64 // e.g. 1.0
	Band2K float64 // e.g. 2.0

	// Rubberband down
	RbNetDropPct   float64 // e.g. 0.0075 (0.75%)
	RbLookbackBars int     // e.g. 3
	RbVolMult      float64 // 2x median(10)

	// Rubberband up (parabolic)
	RbUpNetRisePct   float64
	RbUpLookbackBars int
	RbUpVolMult      float64

	// Backside
	BacksideMinLenBars int     // >= 4
	BacksideMaxBoxPct  float64 // e.g. 0.004 (0.4%)
	BacksideBreakPct   float64 // 0.0005 (0.05%)
	BacksideVolMult    float64 // 1.5x consolidation median
	BacksideWindowMin  int     // 30 minutes from last capitulation

	// Fashionably late (18-SMA cross)
	SmaPeriod int // 18
}

func DefaultConfig() Config {
	return Config{
		Band1K:             1.0,
		Band2K:             2.0,
		RbNetDropPct:       0.0075,
		RbLookbackBars:     3,
		RbVolMult:          2.0,
		// Upside rubberband (parabolic)
		RbUpNetRisePct:     0.0075,
		RbUpLookbackBars:   3,
		RbUpVolMult:        2.0,
		BacksideMinLenBars: 4,
		BacksideMaxBoxPct:  0.004,
		BacksideBreakPct:   0.0005,
		BacksideVolMult:    1.5,
		BacksideWindowMin:  30,
		SmaPeriod:          18,
	}
}

// Detector is per-app, keyed by symbol internally.
type Detector struct {
	mu  sync.Mutex
	cfg Config
	et  *time.Location
	// per symbol
	syms map[string]*symState
	// session
	sessionStart time.Time
}

type symState struct {
	Symbol string

	// rolling history (recent 40+ bars is enough)
	Bars []OneMinBar

	// VWAP state from session start, using typical price ((H+L+C)/3) * Vol
	VwapSumPV float64
	VwapSumV  float64

	// For volume-weighted stddev bands:
	// VWAP = sum(tp * Vol) / sum(Vol)
	// Var_vw(tp) = sum(tp^2 * Vol) / sum(Vol) - VWAP^2
	VwapSumTP2V float64 // sum(tp^2 * Vol)

	// 18-SMA state
	SmaSum   float64
	// track previous SMA for cross
	PrevSMA  float64
	PrevVWAP float64

	// capitulation time (for backside window)
	LastCapitulationAt time.Time

	// current backside box (purely structural; no cooldown)
	BoxActive bool
	BoxStart  int // index in Bars slice
	BoxEnd    int
	BoxHigh   float64
	BoxLow    float64
}

func NewDetector(et *time.Location, cfg Config) *Detector {
	if et == nil {
		et = time.FixedZone("UTC", 0)
	}
	if cfg.Band1K <= 0 {
		cfg.Band1K = 1.0
	}
	if cfg.Band2K <= 0 {
		cfg.Band2K = 2.0
	}
	if cfg.SmaPeriod <= 0 {
		cfg.SmaPeriod = 18
	}
	return &Detector{
		cfg:          cfg,
		et:           et,
		syms:         make(map[string]*symState),
		sessionStart: time.Time{},
	}
}

func (d *Detector) get(sym string) *symState {
	s := d.syms[sym]
	if s == nil {
		s = &symState{Symbol: sym}
		d.syms[sym] = s
	}
	return s
}

// SetSessionStart sets the expected session start time in ET.
// Call this before seeding or live bars when a new stream/session starts.
func (d *Detector) SetSessionStart(start time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionStart = start.In(d.et)
	// reset all symbol state for new session
	d.syms = make(map[string]*symState)
}

// SeedVWAP seeds VWAP (and SMA/Bars) from historical 1m bars between session start and now.
// Bars MUST be contiguous minute bars from true session open for correctness.
func (d *Detector) SeedVWAP(sym string, bars []OneMinBar) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(bars) == 0 {
		return
	}
	s := d.get(sym)
	// reset for safety in case of reuse
	*s = symState{Symbol: sym}

	for _, b := range bars {
		bt := b.Time.In(d.et)
		// enforce non-negative volume
		if b.Vol < 0 {
			b.Vol = 0
		}
		b.Time = bt
		s.Bars = append(s.Bars, b)
		// Typical price
		tp := (b.High + b.Low + b.Close) / 3.0
		// VWAP: sum(tp * Vol) / sum(Vol)
		if b.Vol > 0 {
			s.VwapSumPV += tp * b.Vol
			s.VwapSumV += b.Vol
			// For volume-weighted stddev bands
			s.VwapSumTP2V += tp * tp * b.Vol
		}
		// SMA18 running sum of closes
		s.SmaSum += b.Close
		if len(s.Bars) > d.cfg.SmaPeriod {
			s.SmaSum -= s.Bars[len(s.Bars)-1-d.cfg.SmaPeriod].Close
		}
	}

	// Initialize PrevSMA/PrevVWAP to last seeded values so first live bar cross is correct.
	if len(s.Bars) > 0 {
		if s.VwapSumV > 0 {
			s.PrevVWAP = s.VwapSumPV / s.VwapSumV
		}
		if len(s.Bars) >= d.cfg.SmaPeriod {
			s.PrevSMA = s.SmaSum / float64(d.cfg.SmaPeriod)
		}
	}
}

// OnBar ingests one completed 1m bar and returns zero or more scalp alerts for this bar.
func (d *Detector) OnBar(sym string, t time.Time, o, h, l, c, v float64) []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()

	s := d.get(sym)

	bar := OneMinBar{
		Time:  t.In(d.et),
		Open:  o,
		High:  h,
		Low:   l,
		Close: c,
		Vol:   v,
	}
	s.Bars = append(s.Bars, bar)
	if len(s.Bars) > 400 {
		// safe cap
		s.Bars = s.Bars[len(s.Bars)-400:]
	}

	// Typical price for this bar
	tp := (h + l + c) / 3.0

	// Update VWAP from session start
	if v > 0 {
		s.VwapSumPV += tp * v
		s.VwapSumV += v
		// Update weighted tp^2 sum for stddev
		s.VwapSumTP2V += tp * tp * v
	}

	var vwap float64
	if s.VwapSumV > 0 {
		vwap = s.VwapSumPV / s.VwapSumV
	}

	// Update SMA(18) on closes
	s.SmaSum += c
	if len(s.Bars) > d.cfg.SmaPeriod {
		s.SmaSum -= s.Bars[len(s.Bars)-1-d.cfg.SmaPeriod].Close
	}
	var sma18 float64
	if len(s.Bars) >= d.cfg.SmaPeriod {
		sma18 = s.SmaSum / float64(d.cfg.SmaPeriod)
	}

	var alerts []Alert

	// 1) Rubberband (down and up)
	rbDownAlerts, capitulation := d.detectRubberbandDown(s, bar, vwap)
	if capitulation {
		s.LastCapitulationAt = bar.Time
	}
	alerts = append(alerts, rbDownAlerts...)

	rbUpAlerts := d.detectRubberbandUp(s, bar, vwap)
	alerts = append(alerts, rbUpAlerts...)

	// 2) Backside (depends on LastCapitulationAt)
	bsAlerts := d.detectBackside(s, bar, vwap)
	alerts = append(alerts, bsAlerts...)

	// 3) Fashionably late (18-SMA cross above VWAP with price above VWAP)
	flAlerts := d.detectFashionablyLate(s, bar, vwap, sma18)
	alerts = append(alerts, flAlerts...)

	// Track prev SMA/VWAP for next bar cross detection
	s.PrevSMA = sma18
	s.PrevVWAP = vwap

	return alerts
}

/* ===== Helpers ===== */

func (d *Detector) medianVolLastN(bars []OneMinBar, n int) float64 {
	if n <= 0 || len(bars) == 0 {
		return 0
	}
	if n > len(bars) {
		n = len(bars)
	}
	tmp := make([]float64, n)
	for i := 0; i < n; i++ {
		tmp[i] = bars[len(bars)-1-i].Vol
	}
	// simple insertion sort; n is tiny (<=10)
	for i := 1; i < n; i++ {
		j := i
		for j > 0 && tmp[j-1] > tmp[j] {
			tmp[j-1], tmp[j] = tmp[j], tmp[j-1]
			j--
		}
	}
	if n%2 == 1 {
		return tmp[n/2]
	}
	return 0.5 * (tmp[n/2-1] + tmp[n/2])
}

func (s *symState) bands(cfg Config) (vwap,
	band1Lower, band2Lower,
	band1Upper, band2Upper float64,
) {
	if s.VwapSumV <= 0 {
		return 0, 0, 0, 0, 0
	}
	vwap = s.VwapSumPV / s.VwapSumV

	// volume-weighted variance of typical price
	meanTP2 := s.VwapSumTP2V / s.VwapSumV
	variance := meanTP2 - vwap*vwap
	if variance < 0 {
		variance = 0
	}
	std := math.Sqrt(variance)
	if std == 0 {
		return vwap, 0, 0, 0, 0
	}

	k1 := cfg.Band1K
	if k1 <= 0 {
		k1 = 1.0
	}
	k2 := cfg.Band2K
	if k2 <= 0 {
		k2 = 2.0
	}

	band1Lower = vwap - k1*std
	band2Lower = vwap - k2*std
	band1Upper = vwap + k1*std
	band2Upper = vwap + k2*std
	return
}

/* ===== Rubberband ===== */

func (d *Detector) detectRubberbandDown(s *symState, bar OneMinBar, vwap float64) (alerts []Alert, capitulation bool) {
	if vwap <= 0 || len(s.Bars) < d.cfg.RbLookbackBars+1 {
		return
	}
	v, b1, b2, _, _ := s.bands(d.cfg)
	if v <= 0 || b1 == 0 || b2 == 0 {
		return
	}
	c := bar.Close

	// Downside stretch zone: between band2Lower and band1Lower, closer to band2Lower.
	if !(c >= b2 && c <= b1) {
		return
	}
	if math.Abs(c-b2) <= math.Abs(c-b1) {
		// closer (or equal) to band2Lower than band1Lower (strictly focuses on deep stretch)
	} else {
		return
	}

	n := d.cfg.RbLookbackBars
	start := s.Bars[len(s.Bars)-1-n].Close
	end := c
	if start <= 0 {
		return
	}
	netDrop := (start - end) / start
	if netDrop < d.cfg.RbNetDropPct {
		return
	}

	med10 := d.medianVolLastN(s.Bars, 10)
	if med10 <= 0 || bar.Vol < d.cfg.RbVolMult*med10 {
		return
	}

	alerts = append(alerts, Alert{
		Kind:  KindRubberband,
		Phase: PhaseSetup,
		Sym:   s.Symbol,
		Time:  bar.Time.Format("15:04:05 ET"),
		Price: c,
		Info:  "rubberband down: capitulation near lower VWAP band2",
	})
	capitulation = true
	return
}

func (d *Detector) detectRubberbandUp(s *symState, bar OneMinBar, vwap float64) (alerts []Alert) {
	if vwap <= 0 || len(s.Bars) < d.cfg.RbUpLookbackBars+1 {
		return
	}
	v, _, _, b1u, b2u := s.bands(d.cfg)
	if v <= 0 || b1u == 0 || b2u == 0 {
		return
	}
	c := bar.Close

	// Upside stretch zone: between band1Upper and band2Upper, closer to band2Upper.
	if !(c >= b1u && c <= b2u) {
		return
	}
	if math.Abs(c-b2u) <= math.Abs(c-b1u) {
		// ok: closer to band2Upper
	} else {
		return
	}

	n := d.cfg.RbUpLookbackBars
	start := s.Bars[len(s.Bars)-1-n].Close
	end := c
	if start <= 0 {
		return
	}
	netRise := (end - start) / start
	if netRise < d.cfg.RbUpNetRisePct {
		return
	}

	med10 := d.medianVolLastN(s.Bars, 10)
	if med10 <= 0 || bar.Vol < d.cfg.RbUpVolMult*med10 {
		return
	}

	alerts = append(alerts, Alert{
		Kind:  KindRubberbandUp,
		Phase: PhaseSetup,
		Sym:   s.Symbol,
		Time:  bar.Time.Format("15:04:05 ET"),
		Price: c,
		Info:  "rubberband up: parabolic near upper VWAP band2",
	})
	return
}

// For simplicity and given "no cooldown" + "alert a lot", we implement trigger as:
// when a bar in stretch zone after capitulation is a strong green with >= RbVolMult*median vol.
// That is encoded here and does not depend on stored flags beyond LastCapitulationAt.
func (d *Detector) detectRubberbandTrigger(s *symState, bar OneMinBar, vwap float64) (Alert, bool) {
	// NOTE: currently unused; we embed trigger into backside/follow-up logic if needed.
	return Alert{}, false
}

/* ===== Backside ===== */

func (d *Detector) detectBackside(s *symState, bar OneMinBar, vwap float64) (alerts []Alert) {
	if vwap <= 0 || len(s.Bars) < d.cfg.BacksideMinLenBars {
		return
	}
	// require recent capitulation
	if s.LastCapitulationAt.IsZero() {
		return
	}
	if bar.Time.Sub(s.LastCapitulationAt) > time.Duration(d.cfg.BacksideWindowMin)*time.Minute {
		return
	}

	// 1) Try to (re)identify a consolidation box ending at previous bar.
	// We scan backwards for the tightest last >=MinLen window that meets box constraints.
	n := len(s.Bars)
	minLen := d.cfg.BacksideMinLenBars
	bestStart, bestEnd := -1, -1
	for start := n - minLen; start >= 0; start-- {
		end := n - 1
		if end-start+1 < minLen {
			break
		}
		boxHigh := s.Bars[start].High
		boxLow := s.Bars[start].Low
		for i := start + 1; i <= end; i++ {
			if s.Bars[i].High > boxHigh {
				boxHigh = s.Bars[i].High
			}
			if s.Bars[i].Low < boxLow {
				boxLow = s.Bars[i].Low
			}
		}
		mid := 0.5 * (boxHigh + boxLow)
		if mid <= 0 {
			continue
		}
		boxPct := (boxHigh - boxLow) / mid
		if boxPct <= d.cfg.BacksideMaxBoxPct {
			bestStart, bestEnd = start, end
			break
		}
	}
	if bestStart >= 0 {
		// define/refresh box
		s.BoxActive = true
		s.BoxStart = bestStart
		s.BoxEnd = bestEnd
		s.BoxHigh = s.Bars[bestStart].High
		s.BoxLow = s.Bars[bestStart].Low
		for i := bestStart + 1; i <= bestEnd; i++ {
			if s.Bars[i].High > s.BoxHigh {
				s.BoxHigh = s.Bars[i].High
			}
			if s.Bars[i].Low < s.BoxLow {
				s.BoxLow = s.Bars[i].Low
			}
		}
		// emit a setup on each detection (no cooldown as requested)
		alerts = append(alerts, Alert{
			Kind:  KindBackside,
			Phase: PhaseSetup,
			Sym:   s.Symbol,
			Time:  s.Bars[bestEnd].Time.Format("15:04:05 ET"),
			Price: s.Bars[bestEnd].Close,
			Info:  "post-capitulation consolidation",
		})
	}

	// 2) Breakout trigger on CURRENT bar when a box is known
	if s.BoxActive {
		// breakout through box high with 0.05% margin
		triggerLevel := s.BoxHigh * (1 + d.cfg.BacksideBreakPct)
		if bar.Close >= triggerLevel {
			// volume vs consolidation median
			consBars := s.Bars[s.BoxStart : s.BoxEnd+1]
			med := medianVol(consBars)
			if med > 0 && bar.Vol >= d.cfg.BacksideVolMult*med {
				alerts = append(alerts, Alert{
					Kind:  KindBackside,
					Phase: PhaseTrigger,
					Sym:   s.Symbol,
					Time:  bar.Time.Format("15:04:05 ET"),
					Price: bar.Close,
					Info:  "backside breakout toward VWAP",
				})
				// do not deactivate box; allow further triggers if price chops around.
			}
		}
	}
	return
}

func medianVol(bars []OneMinBar) float64 {
	n := len(bars)
	if n == 0 {
		return 0
	}
	tmp := make([]float64, n)
	for i := 0; i < n; i++ {
		tmp[i] = bars[i].Vol
	}
	// insertion sort
	for i := 1; i < n; i++ {
		j := i
		for j > 0 && tmp[j-1] > tmp[j] {
			tmp[j-1], tmp[j] = tmp[j], tmp[j-1]
			j--
		}
	}
	if n%2 == 1 {
		return tmp[n/2]
	}
	return 0.5 * (tmp[n/2-1] + tmp[n/2])
}

/* ===== Fashionably Late ===== */

func (d *Detector) detectFashionablyLate(s *symState, bar OneMinBar, vwap, sma18 float64) (alerts []Alert) {
	if vwap <= 0 || sma18 <= 0 {
		return
	}
	// Upward cross: previously below, now >=, and price above VWAP.
	if s.PrevSMA > 0 && s.PrevVWAP > 0 &&
		s.PrevSMA < s.PrevVWAP &&
		sma18 >= vwap &&
		bar.Close >= vwap {
		alerts = append(alerts, Alert{
			Kind:  KindFashionablyLate,
			Phase: PhaseTrigger,
			Sym:   s.Symbol,
			Time:  bar.Time.Format("15:04:05 ET"),
			Price: bar.Close,
			Info:  "SMA(18) crossed above VWAP with price above VWAP",
		})
	}
	return
}


