# alertcat (Polygon + Web UI) + RVOL Surge Detector

**What it does**
- Monitors watchlist tickers via Polygon websocket.
- Detects real-time volume surges based on RVOL (relative volume).
- Alerts with sound and UI when surge detected.
- Displays intraday charts, recent news, SEC filings.
- Backfills historical bars for RVOL baselines.

## Setup
1. `cp env.example .env` and set `POLYGON_API_KEY`.
2. Configure `config.yaml` and `watchlist.yaml`.
3. `go mod tidy`
4. `make run`

## Usage
- Open http://localhost:8089
- Start monitoring with controls.
- Session times: Pre‑market (04:00 ET), Regular (09:30 ET), **PM (16:00 ET)**.
- For PM: **HOD/LOD start at 16:06 ET** (first 6 minutes ignored for HOD/LOD calculations).
- Defaults: **Pre‑market (04:00 ET)** window, **HOD shown by default** (LOD hidden),
  RVOL **Method B (Median)** with a **single‑bar baseline** (minute vs same minute over the last 14 trading days).
- RVOL: Compares current minute volume to historical mean/median over 14 days.

## RVOL Explanation
- Method A: Mean-based.
- Method B: Median-based.
- Cumulative mode sums volumes up to current bucket.
- Buckets: 1-minute from 04:00 ET.

## Build/Test Targets

### Makefile

```
run:
	go run .

test:
	go test ./... -v
```
