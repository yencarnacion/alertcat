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
- Default watchlist behavior: `go run .` (loads `watchlist.yaml`).
- Optional multi-watchlist mode: `go run . -watchlists above-upper-band.yaml,below-upper-band.yaml`
  - Tickers are merged across all provided files.
  - Alert cards show source tags from filename stems (for example `below-upper-band.yaml` -> `below-upper-band`).
- Ticker clicks open a new tab to `${ui.chart_opener_base_url}/api/open-chart/{TICKER}/{YYYY-MM-DD}` (default base: `http://localhost:8081`).
- Open http://localhost:8089
- Start monitoring with controls.
- Session times: Pre‑market (04:00 ET), Regular (09:30 ET), **PM (16:00 ET)**.
- Session HOD/LOD anchors:
  - Pre: **04:06 ET** (first 6 minutes ignored)
  - RTH: **09:30 ET**
  - PM: **16:06 ET** (first 6 minutes ignored)
- `Levels Mode` is mutually exclusive:
  - `Session HOD/LOD` mode: only `HOD`/`LOD` alerts are active.
  - `Local High/Low` mode: only `Local High`/`Local Low` alerts are active.
  - You do **not** get both sets at the same time.
- Local mode uses `Local Anchor Time (ET)` (`HH:MM`) as the starting point for local high/low.
- Quick-entry controls for scalping:
  - Session quick buttons: `04:00`, `09:30`, `16:00`
  - Local time presets: `04:06`, `09:30`, `16:06`, `Prev 1/2h`, `Prev hour`, `Now`
  - `Prev 1/2h` rounds to the current half-hour block start (for example `13:40` -> `13:30`, `13:11` -> `13:00`).
  - `Prev hour` rounds to the current hour start (for example `13:40` -> `13:00`).
- `Apply Live` updates the running stream immediately (no stop/start) for:
  - session
  - date
  - levels mode (`Session` vs `Local`)
  - local anchor time
  - local enable state
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
