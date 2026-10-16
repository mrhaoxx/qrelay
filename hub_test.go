package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newTestServer(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	hub := newHub("test-secret", 50)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/rooms", hub.handleCreateRoom)
	mux.HandleFunc("GET /api/rooms/{id}", hub.handleRoomInfo)
	mux.HandleFunc("GET /api/rooms/{id}/history", hub.handleHistory)
	mux.HandleFunc("GET /ws/share/{id}", hub.handleShareWS)
	mux.HandleFunc("GET /ws/watch/{id}", hub.handleWatchWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hub
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

// readMsg returns either the parsed JSON of a text frame, or the header of a
// binary frame plus its payload.
func readMsg(t *testing.T, ctx context.Context, c *websocket.Conn) (map[string]any, []byte) {
	t.Helper()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ == websocket.MessageBinary {
		h, payload, err := unpack(b)
		if err != nil {
			t.Fatalf("unpack: %v", err)
		}
		var m map[string]any
		hb, _ := json.Marshal(h)
		_ = json.Unmarshal(hb, &m)
		return m, payload
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m, nil
}

func sendJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func fakeModules(dim int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, (dim*dim+7)/8))
}

func TestRelayFlow(t *testing.T) {
	srv, hub := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := http.Post(srv.URL+"/api/rooms", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var room map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&room)
	resp.Body.Close()
	if !hub.checkKey(room["id"], room["key"]) {
		t.Fatal("returned key does not verify")
	}

	// Wrong key is rejected.
	_, badResp, err := websocket.Dial(ctx, wsURL(srv, "/ws/share/"+room["id"]+"?k=nope"), nil)
	if err == nil || badResp == nil || badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for bad key, got err=%v resp=%v", err, badResp)
	}

	// Viewer joins before the publisher.
	v1, _, err := websocket.Dial(ctx, wsURL(srv, "/ws/watch/"+room["id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v1.CloseNow()
	st, _ := readMsg(t, ctx, v1)
	if st["type"] != "status" || st["online"] != false {
		t.Fatalf("unexpected first message: %v", st)
	}

	// Publisher connects: gets status + demand (no preview wanted).
	pub, _, err := websocket.Dial(ctx, wsURL(srv, "/ws/share/"+room["id"]+"?k="+room["key"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.CloseNow()
	ps, _ := readMsg(t, ctx, pub)
	pd, _ := readMsg(t, ctx, pub)
	if ps["type"] != "status" || pd["type"] != "demand" || pd["preview"] != false {
		t.Fatalf("publisher initial messages: %v %v", ps, pd)
	}
	st, _ = readMsg(t, ctx, v1)
	if st["online"] != true {
		t.Fatalf("viewer should see publisher online: %v", st)
	}

	// Publisher sends a scan: viewer gets the scan and a regenerated QR.
	scan := map[string]any{"type": "scan", "found": true, "text": "https://example.com/hello",
		"dim": 25, "modules": fakeModules(25), "version": 2}
	sendJSON(t, ctx, pub, scan)
	got, _ := readMsg(t, ctx, v1)
	if got["type"] != "scan" || got["text"] != scan["text"] || got["modules"] != scan["modules"] || got["seq"].(float64) != 1 {
		t.Fatalf("viewer got wrong scan: %v", got)
	}
	qrh, qrPayload := readMsg(t, ctx, v1)
	if qrh["type"] != "qr" || qrh["text"] != scan["text"] || len(qrPayload) < 100 || string(qrPayload[1:4]) != "PNG" {
		t.Fatalf("expected regenerated QR PNG, got %v (%d bytes)", qrh, len(qrPayload))
	}
	// New text: history is pushed to the viewer and the publisher.
	hm, _ := readMsg(t, ctx, v1)
	if hm["type"] != "history" || len(hm["items"].([]any)) != 1 {
		t.Fatalf("expected history with 1 item, got %v", hm)
	}
	if ph, _ := readMsg(t, ctx, pub); ph["type"] != "history" {
		t.Fatalf("publisher expected history, got %v", ph)
	}

	// Same text again: scan relayed, no new QR. A scan with a bad bitmap is dropped.
	sendJSON(t, ctx, pub, map[string]any{"type": "scan", "found": true, "text": "bad", "dim": 25, "modules": "AAAA"})
	sendJSON(t, ctx, pub, scan)
	got, _ = readMsg(t, ctx, v1)
	if got["type"] != "scan" || got["text"] != scan["text"] || got["seq"].(float64) != 2 {
		t.Fatalf("expected relayed scan seq 2, got %v", got)
	}

	// Late viewer gets status, cached scan and cached QR.
	v2, _, err := websocket.Dial(ctx, wsURL(srv, "/ws/watch/"+room["id"]), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.CloseNow()
	m1, _ := readMsg(t, ctx, v2)
	m2, _ := readMsg(t, ctx, v2)
	m3, _ := readMsg(t, ctx, v2)
	m4, _ := readMsg(t, ctx, v2)
	if m1["type"] != "status" || m1["online"] != true || m2["type"] != "scan" || m3["type"] != "qr" || m4["type"] != "history" {
		t.Fatalf("late viewer got %v, %v, %v, %v", m1, m2, m3, m4)
	}
	ps, _ = readMsg(t, ctx, pub)
	if ps["type"] != "status" || ps["viewers"].(float64) != 2 {
		t.Fatalf("publisher status after second viewer: %v", ps)
	}

	// v2 asks for the preview: publisher is told, and only v2 receives it.
	sendJSON(t, ctx, v2, map[string]any{"type": "want", "preview": true})
	pd, _ = readMsg(t, ctx, pub)
	if pd["type"] != "demand" || pd["preview"] != true {
		t.Fatalf("expected demand preview=true, got %v", pd)
	}
	if err := pub.Write(ctx, websocket.MessageBinary, pack(header{Type: "preview"}, []byte("jpegdata"))); err != nil {
		t.Fatal(err)
	}
	ph, pp := readMsg(t, ctx, v2)
	if ph["type"] != "preview" || string(pp) != "jpegdata" {
		t.Fatalf("v2 preview: %v %q", ph, pp)
	}
	// v1 must not have received it: the next thing v1 sees is a status change.
	sendJSON(t, ctx, v2, map[string]any{"type": "want", "preview": false})
	pd, _ = readMsg(t, ctx, pub)
	if pd["preview"] != false {
		t.Fatalf("expected demand preview=false, got %v", pd)
	}

	// History API shows the one distinct text; the publisher can clear it.
	hresp, err := http.Get(srv.URL + "/api/rooms/" + room["id"] + "/history")
	if err != nil {
		t.Fatal(err)
	}
	var items []histItem
	_ = json.NewDecoder(hresp.Body).Decode(&items)
	hresp.Body.Close()
	if len(items) != 1 || items[0].Text != scan["text"] || items[0].Count != 1 || items[0].Dim != 25 {
		t.Fatalf("history API: %+v", items)
	}
	sendJSON(t, ctx, pub, map[string]any{"type": "clearHistory"})
	if ph, _ := readMsg(t, ctx, pub); ph["type"] != "history" || len(ph["items"].([]any)) != 0 {
		t.Fatalf("publisher expected empty history after clear, got %v", ph)
	}

	// Publisher leaves: v1 sees status/history updates only (never the
	// preview), including the cleared history, and finally the offline status.
	pub.Close(websocket.StatusNormalClosure, "")
	sawClear := false
	for {
		m, _ := readMsg(t, ctx, v1)
		switch m["type"] {
		case "history":
			sawClear = len(m["items"].([]any)) == 0
		case "status":
			if m["online"] == false {
				if !sawClear {
					t.Fatal("v1 never saw the cleared history")
				}
				return
			}
		default:
			t.Fatalf("v1 should only receive status/history messages, got %v", m)
		}
	}
}

func TestHistoryDedupAndCap(t *testing.T) {
	r := &Room{viewers: map[*viewer]struct{}{}, maxHistory: 2}
	for i, txt := range []string{"a", "b", "a", "c"} {
		r.publishScan(header{Type: "scan", Found: true, Text: txt, Dim: 21, Modules: fakeModules(21), Version: 1 + i})
	}
	// Order newest first: c, a (count 2); b evicted by the cap.
	if len(r.history) != 2 || r.history[0].Text != "c" || r.history[1].Text != "a" || r.history[1].Count != 2 {
		t.Fatalf("history: %+v", r.history)
	}
	if r.history[1].Version != 3 {
		t.Fatalf("entry should carry the most recent modules/version, got %d", r.history[1].Version)
	}
}

func TestRegenerateQR(t *testing.T) {
	h, payload, err := unpack(regenerateQR("hello qrelay"))
	if err != nil || h.Err != "" || len(payload) < 100 {
		t.Fatalf("regenerateQR: %v %q %d", err, h.Err, len(payload))
	}
	h, _, _ = unpack(regenerateQR(strings.Repeat("x", 5000)))
	if h.Err == "" {
		t.Fatal("expected error for oversized payload")
	}
}

func TestViewerOutboxCoalesces(t *testing.T) {
	v := newViewer()
	v.offer("scan", []byte("s1"))
	v.offer("preview", []byte("p1"))
	v.offer("scan", []byte("s2"))
	v.offer("preview", []byte("p2"))
	got := v.drain()
	if len(got) != 2 || string(got[0]) != "s2" || string(got[1]) != "p2" {
		t.Fatalf("expected [s2 p2], got %q", got)
	}
	if len(v.drain()) != 0 {
		t.Fatal("drain should be empty after draining")
	}
}

func TestValidScan(t *testing.T) {
	if !validScan(header{Found: true, Dim: 21, Modules: fakeModules(21)}) {
		t.Fatal("21x21 should be valid")
	}
	if validScan(header{Found: true, Dim: 22, Modules: fakeModules(22)}) {
		t.Fatal("22 is not a QR dimension")
	}
	if validScan(header{Found: true, Dim: 25, Modules: fakeModules(21)}) {
		t.Fatal("bitmap length mismatch should be invalid")
	}
}
