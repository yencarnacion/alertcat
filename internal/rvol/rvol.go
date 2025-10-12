// File: internal/rvol/rvol.go
package rvol

import (
    "context"
    "errors"
    "fmt"
    "math"
    "math/rand"
    "net/http"
    "sort"
    "time"

    polygonrest "github.com/polygon-io/client-go/rest"
    rmodels     "github.com/polygon-io/client-go/rest/models"
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
    s := stringsLower(session)
    if s == "rth" {
        h, m = 9, 30
    } else if s == "pm" {
        h, m = 16, 0
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
    if httpClient == nil {
        httpClient = &http.Client{Timeout: 30 * time.Second}
    }

    // Use official client-go REST with chunked windows and retries.
    rest := polygonrest.NewWithClient(polygonKey, httpClient)

    // Query a wider CALENDAR window, then derive the most recent N TRADING days (strictly BEFORE anchorDate).
    day := anchorDate.In(et)
    from := day.AddDate(0, 0, -(lookbackDays*2 + 10))
    to := day

    type aggRow struct{ ts int64; vol int64 }
    rows := make([]aggRow, 0, 50000)

    // Chunk into <=10 calendar-day windows with an EXCLUSIVE upper bound (midnight of end + 24h).
    chunkDays := 10
    baseBackoff := time.Second
    jitterMax := 250 * time.Millisecond
    maxRetries := 3

    // Walk by days; include 'end' day by pushing the 'To' bound to midnight(end)+24h.
    start := from
    for !start.After(to) {
        end := start.AddDate(0, 0, chunkDays-1)
        if end.After(to) { end = to }
        // IMPORTANT:
        // - client-go expects From/To as instants.
        // - Use midnight UTC bounds; make 'To' EXCLUSIVE by adding 24h.
        fromUTC := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
        toUTC   := time.Date(end.Year(),   end.Month(),   end.Day(),   0, 0, 0, 0, time.UTC).Add(24 * time.Hour)

        params := &rmodels.ListAggsParams{
            Ticker:     symbol,
            Timespan:   rmodels.Minute,
            Multiplier: 1,
            From:       rmodels.Millis(fromUTC),
            To:         rmodels.Millis(toUTC),
        }
        lim := 50000
        asc := rmodels.Asc
        adj := true
        params.Limit = &lim
        params.Order = &asc
        params.Adjusted = &adj

        var lastErr error
        for attempt := 0; attempt < maxRetries; attempt++ {
            iter := rest.ListAggs(ctx, params)
            for iter.Next() {
                a := iter.Item() // models.Agg (value)
                // a.Timestamp is models.Millis (underlying type is time.Time).
                // Convert to epoch milliseconds for internal processing.
                tsMs := time.Time(a.Timestamp).UnixMilli()
                vol := int64(math.Round(a.Volume))
                rows = append(rows, aggRow{ts: tsMs, vol: vol})
            }
            if err := iter.Err(); err != nil {
                lastErr = err
            } else {
                lastErr = nil
                break
            }
            if ctx.Err() != nil { return nil, ctx.Err() }
            sleep := baseBackoff * (1 << attempt)
            jitter := time.Duration(rand.Int63n(int64(jitterMax)))
            time.Sleep(sleep + jitter)
        }
        if lastErr != nil {
            return nil, fmt.Errorf("aggs window %s..%s: %w", start.Format("2006-01-02"), end.Format("2006-01-02"), lastErr)
        }
        // Advance to the next calendar day
        start = end.AddDate(0, 0, 1)
    }

    if len(rows) == 0 {
        return nil, errors.New("no aggregates returned")
    }

    // Collect unique trading days present in results (chronological as returned asc).
    type key struct{ y, m, d int }
    seenDays := make(map[key]struct{})
    orderedDays := make([]key, 0, lookbackDays*2+10)
    for _, r := range rows {
        ts := time.Unix(0, r.ts*int64(time.Millisecond)).In(et)
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
        if k.y == day.Year() && k.m == int(day.Month()) && k.d == day.Day() { continue }
        filtered = append(filtered, k)
    }
    // Reverse to oldest..newest
    for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 { filtered[i], filtered[j] = filtered[j], filtered[i] }
    dayIndex := make(map[key]int, len(filtered))
    for i, k := range filtered { dayIndex[k] = i }

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

    // Fill from aggregated rows
    for _, r := range rows {
        ts := time.Unix(0, r.ts*int64(time.Millisecond)).In(et)
        k := key{ts.Year(), int(ts.Month()), ts.Day()}
        pos, ok := dayIndex[k]
        if !ok { continue }
        idx := MinuteIndexFrom0400ET(ts, et)
        if idx < 0 || idx >= (16*60) { continue }
        row := getSlot(idx)
        row[pos] = r.vol // missing minutes remain 0
    }

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
			// Accumulate per day: sum baselines[k][day] for k in [startIdx..bucketIdx].
			// Determine day count from any available row in range (some buckets may be completely missing).
			days := 0
			for k := startIdx; k <= bucketIdx; k++ {
				if row, ok := baselines[k]; ok && len(row) > days {
					days = len(row)
				}
			}
			if days == 0 {
				return 0, 0
			}
			perDaySums := make([]int64, days)
			for k := startIdx; k <= bucketIdx; k++ {
				if row, ok := baselines[k]; ok {
					// Missing minutes are treated as zero; only add what exists.
					for d := 0; d < len(row); d++ {
						perDaySums[d] += row[d]
					}
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
