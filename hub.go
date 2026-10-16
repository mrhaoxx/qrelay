package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"
	qrcode "github.com/skip2/go-qrcode"
)

// Wire protocol
//
// Text frames are JSON objects with a "type" field. Binary frames carry an
// image: a big-endian uint32 header length, a JSON header, then raw bytes.
//
//	publisher -> server   {"type":"scan","found":bool,"text","dim","modules","version","mirrored"}
//	                          modules = base64 of row-major, MSB-first bit-packed QR modules (1 = dark)
//	                          sent when the result changes, plus a 1 Hz keepalive
//	                      [hdr {"type":"preview"}] + JPEG   only while some viewer asked for it
//	server -> publisher   {"type":"status","online","viewers"}
//	                      {"type":"demand","preview":bool}
//	server -> viewer      {"type":"status",…}, {"type":"scan",…,"seq","ts"}
//	                      [hdr {"type":"qr","text"}] + PNG  regenerated once per distinct text
//	                      [hdr {"type":"qr","text","err"}]  when regeneration failed
//	                      preview messages forwarded unchanged
//	viewer -> server      {"type":"want","preview":bool}

type header struct {
	Type     string `json:"type"`
	Found    bool   `json:"found,omitempty"`
	Text     string `json:"text,omitempty"`
	Dim      int    `json:"dim,omitempty"`
	Modules  string `json:"modules,omitempty"`
	Version  int    `json:"version,omitempty"`
	Mirrored bool   `json:"mirrored,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`
	TS       int64  `json:"ts,omitempty"`
	Err      string `json:"err,omitempty"`
}

type statusMsg struct {
	Type    string `json:"type"`
	Online  bool   `json:"online"`
	Viewers int    `json:"viewers"`
}

type demandMsg struct {
	Type    string `json:"type"`
	Preview bool   `json:"preview"`
}

type wantMsg struct {
	Type    string `json:"type"`
	Preview bool   `json:"preview"`
}

// histItem is one distinct decoded content seen in a room.
type histItem struct {
	Text     string `json:"text"`
	Dim      int    `json:"dim"`
	Modules  string `json:"modules"`
	Version  int    `json:"version"`
	Mirrored bool   `json:"mirrored,omitempty"`
	First    int64  `json:"first"` // unix ms, first seen
	Last     int64  `json:"last"`  // unix ms, most recently seen
	Count    int    `json:"count"` // how many times it (re)appeared
}

type historyMsg struct {
	Type  string     `json:"type"`
	Items []histItem `json:"items"`
}

// pack builds a binary message: u32 header length + JSON header + payload.
func pack(h header, payload []byte) []byte {
	hb, _ := json.Marshal(h)
	out := make([]byte, 4+len(hb)+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(hb)))
	copy(out[4:], hb)
	copy(out[4+len(hb):], payload)
	return out
}

func unpack(b []byte) (header, []byte, error) {
	var h header
	if len(b) < 4 {
		return h, nil, errors.New("short message")
	}
	n := int(binary.BigEndian.Uint32(b))
	if n < 0 || 4+n > len(b) || n > 64<<10 {
		return h, nil, errors.New("bad header length")
	}
	if err := json.Unmarshal(b[4:4+n], &h); err != nil {
		return h, nil, err
	}
	return h, b[4+n:], nil
}

// viewer holds a per-connection outbox that keeps only the newest message
// of each kind, so a slow viewer never queues up stale data.
type viewer struct {
	mu          sync.Mutex
	pending     map[string][]byte
	order       []string
	wake        chan struct{}
	wantPreview bool
}

func newViewer() *viewer {
	return &viewer{pending: map[string][]byte{}, wake: make(chan struct{}, 1)}
}

func (v *viewer) offer(kind string, b []byte) {
	v.mu.Lock()
	if _, ok := v.pending[kind]; !ok {
		v.order = append(v.order, kind)
	}
	v.pending[kind] = b
	v.mu.Unlock()
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *viewer) drain() [][]byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([][]byte, 0, len(v.order))
	for _, k := range v.order {
		out = append(out, v.pending[k])
	}
	v.order = v.order[:0]
	clear(v.pending)
	return out
}

type Room struct {
	id string

	mu         sync.Mutex
	publisher  *websocket.Conn
	viewers    map[*viewer]struct{}
	lastScan   []byte // last scan message (JSON)
	lastQR     []byte // last regenerated QR message (binary)
	lastQRText string
	history    []histItem // newest first, distinct by text
	maxHistory int
	demand     demandMsg
	seq        uint64
	lastActive time.Time
}

type Hub struct {
	secret     []byte
	maxHistory int

	mu    sync.Mutex
	rooms map[string]*Room
}

var roomIDRe = regexp.MustCompile(`^[a-z2-7]{8,16}$`)

func newHub(secret string, maxHistory int) *Hub {
	h := &Hub{rooms: map[string]*Room{}, maxHistory: maxHistory}
	if secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			log.Fatal(err)
		}
		h.secret = b
	} else {
		h.secret = []byte(secret)
	}
	go h.gc()
	return h
}

func randomID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return lower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:8])
}

// keyFor derives the publisher key for a room from the server secret, so key
// checks are stateless and survive restarts when -secret is fixed.
func (h *Hub) keyFor(id string) string {
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:22]
}

func (h *Hub) checkKey(id, key string) bool {
	return hmac.Equal([]byte(h.keyFor(id)), []byte(key))
}

func (h *Hub) room(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[id]
	if !ok {
		r = &Room{id: id, viewers: map[*viewer]struct{}{}, lastActive: time.Now(), demand: demandMsg{Type: "demand"}, maxHistory: h.maxHistory}
		h.rooms[id] = r
	}
	return r
}

func (h *Hub) peek(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
}

func (h *Hub) gc() {
	for range time.Tick(time.Minute) {
		h.mu.Lock()
		for id, r := range h.rooms {
			r.mu.Lock()
			idle := r.publisher == nil && len(r.viewers) == 0 && time.Since(r.lastActive) > 30*time.Minute
			r.mu.Unlock()
			if idle {
				delete(h.rooms, id)
			}
		}
		h.mu.Unlock()
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// ---- HTTP API ----

func (h *Hub) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	id := randomID()
	writeJSON(w, map[string]string{
		"id":    id,
		"key":   h.keyFor(id),
		"share": "/share/" + id + "?k=" + h.keyFor(id),
		"watch": "/watch/" + id,
	})
}

func (h *Hub) handleRoomInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomIDRe.MatchString(id) {
		http.Error(w, "bad room id", http.StatusBadRequest)
		return
	}
	online, viewers := false, 0
	if room := h.peek(id); room != nil {
		room.mu.Lock()
		online = room.publisher != nil
		viewers = len(room.viewers)
		room.mu.Unlock()
	}
	writeJSON(w, map[string]any{"id": id, "online": online, "viewers": viewers})
}

// handleHistory returns the room's distinct decoded contents, newest first.
func (h *Hub) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomIDRe.MatchString(id) {
		http.Error(w, "bad room id", http.StatusBadRequest)
		return
	}
	items := []histItem{}
	if room := h.peek(id); room != nil {
		room.mu.Lock()
		items = append(items, room.history...)
		room.mu.Unlock()
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename=\"qrelay-"+id+"-history.json\"")
	}
	writeJSON(w, items)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- Publisher WebSocket ----

func (h *Hub) handleShareWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomIDRe.MatchString(id) {
		http.Error(w, "bad room id", http.StatusBadRequest)
		return
	}
	if !h.checkKey(id, r.URL.Query().Get("k")) {
		http.Error(w, "bad key", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(2 << 20)
	defer conn.CloseNow()

	room := h.room(id)
	room.mu.Lock()
	if old := room.publisher; old != nil {
		old.Close(websocket.StatusPolicyViolation, "replaced by a new publisher")
	}
	room.publisher = conn
	room.lastActive = time.Now()
	room.broadcastStatusLocked(nil)
	room.sendDemandLocked()
	if len(room.history) > 0 {
		room.writePublisherLocked(room.historyMsg())
	}
	room.mu.Unlock()
	log.Printf("room %s: publisher connected from %s", id, r.RemoteAddr)

	ctx := r.Context()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		switch typ {
		case websocket.MessageText:
			var hd header
			if json.Unmarshal(data, &hd) != nil {
				continue
			}
			switch {
			case hd.Type == "scan" && validScan(hd):
				room.publishScan(hd)
			case hd.Type == "clearHistory":
				room.clearHistory()
			}
		case websocket.MessageBinary:
			if hd, _, err := unpack(data); err == nil && hd.Type == "preview" {
				room.publishPreview(data)
			}
		}
	}

	room.mu.Lock()
	if room.publisher == conn {
		room.publisher = nil
	}
	room.lastActive = time.Now()
	room.broadcastStatusLocked(nil)
	room.mu.Unlock()
	log.Printf("room %s: publisher disconnected", id)
}

// validScan checks that the module bitmap matches the declared dimension.
func validScan(hd header) bool {
	if !hd.Found {
		return true
	}
	if hd.Dim < 21 || hd.Dim > 177 || (hd.Dim-17)%4 != 0 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(hd.Modules)
	return err == nil && len(raw) == (hd.Dim*hd.Dim+7)/8
}

// publishScan relays the decode result and, when the text changed,
// regenerates a clean QR code and sends it to viewers once.
func (r *Room) publishScan(hd header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	hd.Seq, hd.TS = r.seq, time.Now().UnixMilli()
	r.lastActive = time.Now()
	b, _ := json.Marshal(hd)
	r.lastScan = b
	for v := range r.viewers {
		v.offer("scan", b)
	}
	if hd.Found && hd.Text != "" && hd.Text != r.lastQRText {
		r.lastQRText = hd.Text
		r.lastQR = regenerateQR(hd.Text)
		for v := range r.viewers {
			v.offer("qr", r.lastQR)
		}
		r.recordHistoryLocked(hd)
	}
}

// recordHistoryLocked adds or refreshes the history entry for a newly seen
// text (called only when the text changed) and pushes the list to everyone.
func (r *Room) recordHistoryLocked(hd header) {
	now := hd.TS
	item := histItem{Text: hd.Text, Dim: hd.Dim, Modules: hd.Modules, Version: hd.Version, Mirrored: hd.Mirrored,
		First: now, Last: now, Count: 1}
	for i, it := range r.history {
		if it.Text == hd.Text {
			item.First, item.Count = it.First, it.Count+1
			r.history = append(r.history[:i], r.history[i+1:]...)
			break
		}
	}
	r.history = append([]histItem{item}, r.history...)
	if r.maxHistory > 0 && len(r.history) > r.maxHistory {
		r.history = r.history[:r.maxHistory]
	}
	r.broadcastHistoryLocked()
}

func (r *Room) clearHistory() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history = nil
	r.lastQRText = "" // so the current code is recorded again if still in view
	r.broadcastHistoryLocked()
}

func (r *Room) historyMsg() []byte {
	items := r.history
	if items == nil {
		items = []histItem{}
	}
	b, _ := json.Marshal(historyMsg{Type: "history", Items: items})
	return b
}

func (r *Room) broadcastHistoryLocked() {
	b := r.historyMsg()
	for v := range r.viewers {
		v.offer("history", b)
	}
	r.writePublisherLocked(b)
}

// publishPreview forwards a camera preview to the viewers that asked for it.
func (r *Room) publishPreview(msg []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastActive = time.Now()
	for v := range r.viewers {
		if v.wantPreview {
			v.offer("preview", msg)
		}
	}
}

// regenerateQR encodes text into a fresh, clean QR code message.
func regenerateQR(text string) []byte {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return pack(header{Type: "qr", Text: text, Err: err.Error()}, nil)
	}
	png, err := q.PNG(512)
	if err != nil {
		return pack(header{Type: "qr", Text: text, Err: err.Error()}, nil)
	}
	return pack(header{Type: "qr", Text: text}, png)
}

func (r *Room) status() []byte {
	b, _ := json.Marshal(statusMsg{Type: "status", Online: r.publisher != nil, Viewers: len(r.viewers)})
	return b
}

// broadcastStatusLocked sends the current status to every viewer except
// `except` and to the publisher. Caller holds r.mu.
func (r *Room) broadcastStatusLocked(except *viewer) {
	b := r.status()
	for v := range r.viewers {
		if v != except {
			v.offer("status", b)
		}
	}
	r.writePublisherLocked(b)
}

// recomputeDemandLocked aggregates what viewers want and tells the publisher
// when it changes. Caller holds r.mu.
func (r *Room) recomputeDemandLocked() {
	d := demandMsg{Type: "demand"}
	for v := range r.viewers {
		d.Preview = d.Preview || v.wantPreview
	}
	if d != r.demand {
		r.demand = d
		r.sendDemandLocked()
	}
}

func (r *Room) sendDemandLocked() {
	b, _ := json.Marshal(r.demand)
	r.writePublisherLocked(b)
}

func (r *Room) writePublisherLocked(b []byte) {
	if r.publisher == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.publisher.Write(ctx, websocket.MessageText, b)
}

// ---- Viewer WebSocket ----

func (h *Hub) handleWatchWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomIDRe.MatchString(id) {
		http.Error(w, "bad room id", http.StatusBadRequest)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	defer conn.CloseNow()

	room := h.room(id)
	v := newViewer()

	room.mu.Lock()
	room.viewers[v] = struct{}{}
	v.offer("status", room.status())
	if room.lastScan != nil {
		v.offer("scan", room.lastScan)
	}
	if room.lastQR != nil {
		v.offer("qr", room.lastQR)
	}
	if len(room.history) > 0 {
		v.offer("history", room.historyMsg())
	}
	room.broadcastStatusLocked(v)
	room.mu.Unlock()

	defer func() {
		room.mu.Lock()
		delete(room.viewers, v)
		room.lastActive = time.Now()
		room.broadcastStatusLocked(nil)
		room.recomputeDemandLocked()
		room.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reader: viewers only send "want" messages.
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var wm wantMsg
			if json.Unmarshal(data, &wm) != nil || wm.Type != "want" {
				continue
			}
			room.mu.Lock()
			v.wantPreview = wm.Preview
			room.recomputeDemandLocked()
			room.mu.Unlock()
		}
	}()

	for {
		for _, b := range v.drain() {
			if err := writeWithTimeout(ctx, conn, b); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-v.wake:
		}
	}
}

func writeWithTimeout(ctx context.Context, c *websocket.Conn, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	typ := websocket.MessageText
	if len(b) > 0 && b[0] != '{' {
		typ = websocket.MessageBinary
	}
	return c.Write(ctx, typ, b)
}
