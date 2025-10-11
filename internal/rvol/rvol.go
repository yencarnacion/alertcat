// File: internal/rvol/rvol.go
package rvol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

type Method string

const (
	MethodA Method = "A" // mean
	MethodB Method = "B" // median
)

// Baselines holds, for each "minute index" since 04:00 ET, the slice of volumes across last N days.
// Index 0 => 04:00:00–04:00:59, index 1 => 04:01, ..., index (16*60) => 20:00 bucket not used, last is 19:59.
type Baselines map[int][]int64

type Config struct {
	Threshold    float64
	Method       Method // "A" or "B"
	LookbackDays int
	BaselineMode string // "cumulative" or "single"
}

// MinuteIndexFrom0400ET returns the minute-bucket index since 04:00 ET (0-based).
func MinuteIndexFrom0400ET(t time.Time, et *time.Location) int {
	tt := t.In(et)
	base := time.Date(tt.Year(), tt.Month(), tt.Day(), 4, 0, 0, 0, et)
    if tt.Before(base) {
        // Explicitly indicate "before 04:00 ET" so callers can ignore safely.
        return -1
    }
	return int(tt.Sub(base).Minutes())
}

// SessionStartIndex computes the minute index at which the session opens (pre or rth) for "day".
func SessionStartIndex(session string, et *time.Location, day time.Time) int {
	day = day.In(et)
	h, m := 4, 0
	if session == "rth" {
		h, m = 9, 30
	}
	base := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, et)
    return MinuteIndexFrom0400ET(base, et)
}

// Backfill fetches last N days of 1‑minute aggregates from Polygon REST and builds per-minute baselines.
// Missing minutes are treated as zero (per your instruction).
func Backfill(ctx context.Context, httpClient *http.Client, polygonKey, symbol string, anchorDate time.Time, lookbackDays int, et *time.Location) (Baselines, error) {
	if polygonKey == "" {
		return nil, fmt.Errorf("missing polygon key")
	}
	if lookbackDays <= 0 {
		lookbackDays = 14
	}
    // Query a wider CALENDAR window, then derive the most recent N TRADING days from actual results.
    day := anchorDate.In(et)
    from := day.AddDate(0, 0, -(lookbackDays*2 + 10))
	fromStr := fmt.Sprintf("%d-%02d-%02d", from.Year(), from.Month(), from.Day())
	toStr := fmt.Sprintf("%d-%02d-%02d", day.Year(), day.Month(), day.Day())

	// Polygon aggregates endpoint
	u := fmt.Sprintf("https://api.polygon.io/v2/aggs/ticker/%s/range/1/minute/%s/%s?adjusted=true&sort=asc&limit=50000&apiKey=%s",
		symbol, fromStr, toStr, polygonKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("polygon aggs http %d", resp.StatusCode)
	}
	var payload struct {
		Results []struct {
			T int64 `json:"t"` // ms
			V int64 `json:"v"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

    // Collect unique trading days present in results (chronological due to sort=asc).
    type key struct{ y, m, d int }
    seenDays := make(map[key]struct{})
    orderedDays := make([]key, 0, lookbackDays*2+10)
    for _, r := range payload.Results {
        ts := time.Unix(0, r.T*int64(time.Millisecond)).In(et)
        k := key{ts.Year(), int(ts.Month()), ts.Day()}
        if _, ok := seenDays[k]; !ok {
            seenDays[k] = struct{}{}
            orderedDays = append(orderedDays, k)
        }
    }
    // Keep the most recent N trading days strictly before (excluding) anchorDate.
    filtered := make([]key, 0, lookbackDays)
    for i := len(orderedDays) - 1; i >= 0 && len(filtered) < lookbackDays; i-- {
        k := orderedDays[i]
        if k.y == day.Year() && k.m == int(day.Month()) && k.d == day.Day() {
            continue
        }
        filtered = append(filtered, k)
    }
    // Reverse to oldest..newest
    for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
        filtered[i], filtered[j] = filtered[j], filtered[i]
    }
    dayIndex := make(map[key]int, len(filtered))
    for i, k := range filtered {
        dayIndex[k] = i
    }
    baselines := make(Baselines)

	// Pre-size slices for each bucket when needed
    getSlot := func(idx int) []int64 {
		row, ok := baselines[idx]
        if !ok || len(row) != len(filtered) {
            row = make([]int64, len(filtered))
			baselines[idx] = row
		}
		return row
	}

	// Fill from Polygon results
	for _, r := range payload.Results {
        ts := time.Unix(0, r.T*int64(time.Millisecond)).In(et)
        k := key{ts.Year(), int(ts.Month()), ts.Day()}
        pos, ok := dayIndex[k]
		if !ok {
			continue
		}
        idx := MinuteIndexFrom0400ET(ts, et)
		if idx < 0 || idx >= (16*60) { // 04:00..19:59
			continue
		}
		row := getSlot(idx)
		row[pos] = r.V // missing mins remain 0
	}

	// Normalize baseline order to be oldest..newest and ensure deterministic map iteration (not required but nice)
	return baselines, nil
}

func mean(xs []int64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s int64
	for _, v := range xs {
		s += v
	}
	return float64(s) / float64(len(xs))
}
func median(xs []int64) float64 {
	if n := len(xs); n == 0 {
		return 0
	} else {
		cp := append([]int64(nil), xs...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		m := n / 2
		if n%2 == 1 {
			return float64(cp[m])
		}
		return float64(cp[m-1]+cp[m]) / 2.0
	}
}

// ComputeRVOL calculates RVOL and baseline given baselines (per-minute rows across days).
// - If baselineMode == "single", it uses only the current bucket.
// - If "cumulative", it sums per-day volumes from <session open> to current minute and then mean/median over days.
func ComputeRVOL(baselines Baselines, bucketIdx int, curVol int64, method Method, baselineMode string, session string, et *time.Location) (rvol float64, baseline float64) {
	if bucketIdx < 0 {
		return 0, 0
	}
	// Figure out per-day cumulative if needed
	switch stringsLower(baselineMode) {
	case "cumulative":
		// Find session start index (using today's date just to compute clock minute; the per-day sum is index-based anyway)
		now := time.Now().In(et)
        startIdx := SessionStartIndex(session, et, now)
		if startIdx < 0 {
			startIdx = 0
		}
		if bucketIdx < startIdx {
			// before session start: cumulative is zero baseline and zero current unless user specifically wants pre bars; keep safe:
			if curVol == 0 {
				return 0, 0
			}
		}
		// Accumulate per day: sum baselines[k][day] for k in [startIdx..bucketIdx]
		// First determine how many days we have
		var days int
		if row, ok := baselines[bucketIdx]; ok {
			days = len(row)
		}
		if days == 0 {
			return 0, 0
		}
		perDaySums := make([]int64, days)
		for k := startIdx; k <= bucketIdx; k++ {
			row, ok := baselines[k]
			if !ok {
				continue
			}
			for d := 0; d < days; d++ {
				perDaySums[d] += row[d] // missing minutes were 0 by construction
			}
		}
		switch method {
		case MethodB:
			baseline = median(perDaySums)
		default:
			baseline = mean(perDaySums)
		}
		// current cumulative is "curVol" only if caller provided cumulative, but our caller supplies current-minute bar volume.
		// In cumulative mode, the caller should pass the *cumulative* so RVOL is correct. If they passed single minute, RVOL will be off.
		// The server implementation ensures cumulative is passed in cumulative mode.
		if baseline <= 0 {
			return 0, 0
		}
		return float64(curVol) / baseline, baseline

	default: // "single"
		row := baselines[bucketIdx]
		if len(row) == 0 {
			return 0, 0
		}
		switch method {
		case MethodB:
			baseline = median(row)
		default:
			baseline = mean(row)
		}
		if baseline <= 0 {
			return 0, 0
		}
		return float64(curVol) / baseline, baseline
	}
}

func stringsLower(s string) string {
	r := make([]rune, 0, len(s))
	for _, ch := range s {
		if ch >= 'A' && ch <= 'Z' {
			ch = ch - 'A' + 'a'
		}
		r = append(r, ch)
	}
	return string(r)
}
