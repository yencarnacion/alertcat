// File: internal/polygon/polygon.go
package polygon

// NOTE: This broker is intentionally permissive and local-dev friendly per spec.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultWS = "wss://socket.polygon.io/stocks"
)

type Trade struct {
	Ev  string  `json:"ev"`  // "T"
	Sym string  `json:"sym"` // "AAPL"
	P   float64 `json:"p"`   // price
	S   int64   `json:"s"`   // size
	T   int64   `json:"t"`   // SIP ms
}

type AggregateMinute struct {
	Ev  string  `json:"ev"`  // "AM"
	Sym string  `json:"sym"`
    V   float64 `json:"v"`   // volume may be non-integer
	O   float64 `json:"o"`
	H   float64 `json:"h"`
	L   float64 `json:"l"`
	C   float64 `json:"c"`
	S   int64   `json:"s"`   // start ms
	E   int64   `json:"e"`   // end ms
}

type Subscription struct {
	Symbol    string
	Trades    chan Trade
	Minutes   chan AggregateMinute
	done      chan struct{}
	closeOnce sync.Once
}

// Done returns a channel closed when the subscription is closed.
func (s *Subscription) Done() <-chan struct{} {
    return s.done
}
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

type Broker struct {
	apiKey     string
	wsURL      string
	mu         sync.RWMutex
	conn       *websocket.Conn
	dialing    bool
	subscribed map[string]struct{} // symbols we asked Polygon for
	watchers   map[string]map[*Subscription]struct{}
	outbound   chan any
	closed     chan struct{}
}

func NewBroker(apiKey string, wsOptional ...string) *Broker {
	u := defaultWS
	if len(wsOptional) > 0 && strings.TrimSpace(wsOptional[0]) != "" {
		u = wsOptional[0]
	}
	return &Broker{
		apiKey:     apiKey,
		wsURL:      u,
		subscribed: make(map[string]struct{}),
        watchers:   make(map[string]map[*Subscription]struct{}),
        outbound:   make(chan any, 1024),
		closed:     make(chan struct{}),
	}
}

func (b *Broker) Subscribe(symbol string) *Subscription {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return nil
	}
	sub := &Subscription{
		Symbol:  s,
		Trades:  make(chan Trade, 256),
		Minutes: make(chan AggregateMinute, 256),
		done:    make(chan struct{}),
	}
	b.mu.Lock()
	if _, ok := b.watchers[s]; !ok {
		b.watchers[s] = make(map[*Subscription]struct{})
	}
	b.watchers[s][sub] = struct{}{}
    if _, ok := b.subscribed[s]; !ok {
        // mark needed
        b.subscribed[s] = struct{}{}
        // If the WS loop is already running, enqueue a subscribe.
        // Non-blocking so a large initial watchlist never deadlocks startup.
        select {
        case b.outbound <- subscribeMsgFor(s):
        default:
        }
    }
	b.mu.Unlock()
	return sub
}

func (b *Broker) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	sub.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	ws := b.watchers[sub.Symbol]
	if ws != nil {
		delete(ws, sub)
		if len(ws) == 0 {
			delete(b.watchers, sub.Symbol)
		}
	}
}

type wsMsg struct {
	Action string `json:"action"`
	Params string `json:"params,omitempty"`
}

func authMsg(key string) wsMsg      { return wsMsg{Action: "auth", Params: key} }
func subscribeMsgFor(sym string) wsMsg {
	// subscribe to trades + minute aggregates
	return wsMsg{Action: "subscribe", Params: "T." + sym + ",AM." + sym}
}
func unsubscribeMsgFor(sym string) wsMsg {
    return wsMsg{Action: "unsubscribe", Params: "T." + sym + ",AM." + sym}
}

func (b *Broker) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := b.runOnce(ctx); err != nil {
			log.Printf("[polygon] disconnected: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			// exponential up to 30s
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

func (b *Broker) runOnce(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: true,
	}
	conn, _, err := dialer.DialContext(ctx, b.wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()

	// auth
	if err := conn.WriteJSON(authMsg(b.apiKey)); err != nil {
		return fmt.Errorf("auth write: %w", err)
	}

	// ask for current subscriptions
	b.mu.RLock()
	for s := range b.subscribed {
		_ = conn.WriteJSON(subscribeMsgFor(s))
	}
	b.mu.RUnlock()

	// ping
	ping := time.NewTicker(45 * time.Second)
	defer ping.Stop()

	errCh := make(chan error, 1)
	go func() {
		for {
			var msgs []json.RawMessage
			if err := conn.ReadJSON(&msgs); err != nil {
				errCh <- err
				return
			}
			for _, raw := range msgs {
				// peek ev
				var ev struct{ Ev string `json:"ev"` }
				_ = json.Unmarshal(raw, &ev)
				switch ev.Ev {
				case "T":
					var t Trade
					if err := json.Unmarshal(raw, &t); err == nil {
						b.dispatchTrade(t)
					}
				case "AM":
					var a AggregateMinute
					if err := json.Unmarshal(raw, &a); err == nil {
						b.dispatchAM(a)
					}
				default:
					// ignore "status" and others
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ping.C:
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
		case msg := <-b.outbound:
			_ = conn.WriteJSON(msg)
		case err := <-errCh:
			return err
		}
	}
}

func (b *Broker) dispatchTrade(t Trade) {
	b.mu.RLock()
	ws := b.watchers[t.Sym]
	b.mu.RUnlock()
	for sub := range ws {
		select {
		case <-sub.done:
			// skip closed
		case sub.Trades <- t:
		default:
			// drop if slow consumer
		}
	}
}

func (b *Broker) dispatchAM(a AggregateMinute) {
	b.mu.RLock()
	ws := b.watchers[a.Sym]
	b.mu.RUnlock()
	for sub := range ws {
		select {
		case <-sub.done:
		case sub.Minutes <- a:
		default:
		}
	}
}

// RemoveSymbol drops a symbol from the auto-resubscribe set and enqueues an unsubscribe
// to the live connection (if connected). Future reconnects will not resubscribe it.
func (b *Broker) RemoveSymbol(symbol string) {
    s := strings.ToUpper(strings.TrimSpace(symbol))
    if s == "" {
        return
    }
    b.mu.Lock()
    delete(b.subscribed, s)
    // If no watchers remain, clean map entry
    if ws := b.watchers[s]; ws != nil && len(ws) == 0 {
        delete(b.watchers, s)
    }
    b.mu.Unlock()
    // Try to send unsubscribe without blocking (if buffer is full, reconnect path will not re-add it anyway).
    select {
    case b.outbound <- unsubscribeMsgFor(s):
    default:
    }
}
