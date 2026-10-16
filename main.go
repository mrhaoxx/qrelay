// qrelay: share the QR code in front of your camera with anyone who has the link.
//
// The publisher page detects a QR code in the camera stream, crops it
// losslessly and sends it over a WebSocket. The server regenerates a clean
// QR code from the decoded text and fans both out to viewers, who can toggle
// between a pixel-exact replica and the re-parsed version.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed static
var staticFS embed.FS

// envOr returns the environment variable `key` if set, else def. Every flag
// below can also be set through QRELAY_* so containers need no command line.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch envOr(key, "") {
	case "1", "true", "TRUE", "yes", "on":
		return true
	}
	return false
}

func main() {
	addr := flag.String("addr", envOr("QRELAY_ADDR", ":8080"), "listen address (env QRELAY_ADDR)")
	certFile := flag.String("cert", envOr("QRELAY_CERT", ""), "TLS certificate file, enables HTTPS (env QRELAY_CERT)")
	keyFile := flag.String("key", envOr("QRELAY_KEY", ""), "TLS private key file (env QRELAY_KEY)")
	selfSigned := flag.Bool("self-signed", envBool("QRELAY_SELF_SIGNED"), "serve HTTPS with an in-memory self-signed certificate; browsers only allow camera access on HTTPS or localhost (env QRELAY_SELF_SIGNED)")
	secret := flag.String("secret", envOr("QRELAY_SECRET", ""), "secret used to derive publisher keys; set it to keep share links valid across restarts, default random per start (env QRELAY_SECRET)")
	histDefault, _ := strconv.Atoi(envOr("QRELAY_HISTORY", "50"))
	maxHistory := flag.Int("history", histDefault, "distinct decoded contents to remember per room, 0 = unlimited (env QRELAY_HISTORY)")
	healthcheck := flag.Bool("healthcheck", false, "probe the running server at -addr and exit 0 if healthy (used by the Docker HEALTHCHECK)")
	flag.Parse()

	useTLS := *selfSigned || (*certFile != "" && *keyFile != "")
	if *healthcheck {
		os.Exit(runHealthcheck(*addr, useTLS))
	}

	hub := newHub(*secret, *maxHistory)
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", servePage(static, "index.html"))
	mux.HandleFunc("GET /share/{id}", requireRoomID(servePage(static, "share.html")))
	mux.HandleFunc("GET /watch/{id}", requireRoomID(servePage(static, "watch.html")))
	mux.HandleFunc("POST /api/rooms", hub.handleCreateRoom)
	mux.HandleFunc("GET /api/rooms/{id}", hub.handleRoomInfo)
	mux.HandleFunc("GET /api/rooms/{id}/history", hub.handleHistory)
	mux.HandleFunc("GET /ws/share/{id}", hub.handleShareWS)
	mux.HandleFunc("GET /ws/watch/{id}", hub.handleWatchWS)
	mux.HandleFunc("GET /qr.png", handleQRPNG)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	scheme := "http"
	if useTLS {
		scheme = "https"
		if *selfSigned {
			cert, err := selfSignedCert()
			if err != nil {
				log.Fatal(err)
			}
			srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		}
	}

	_, port, _ := net.SplitHostPort(*addr)
	log.Printf("qrelay listening on %s", *addr)
	for _, ip := range localIPs() {
		log.Printf("  %s://%s", scheme, net.JoinHostPort(ip.String(), port))
	}
	if !useTLS {
		log.Printf("note: camera access requires HTTPS unless you open http://localhost:%s; use -self-signed for LAN testing", port)
	}

	if useTLS {
		log.Fatal(srv.ListenAndServeTLS(*certFile, *keyFile))
	}
	log.Fatal(srv.ListenAndServe())
}

// runHealthcheck GETs /healthz on the local server and returns a process exit code.
func runHealthcheck(addr string, useTLS bool) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad addr:", err)
		return 2
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}

func servePage(static fs.FS, name string) http.HandlerFunc {
	b, err := fs.ReadFile(static, name)
	if err != nil {
		log.Fatal(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	}
}

func requireRoomID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !roomIDRe.MatchString(r.PathValue("id")) {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// handleQRPNG renders arbitrary text as a QR PNG; the publisher page uses it
// to show the watch link as a scannable code.
func handleQRPNG(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" || len(data) > 2000 {
		http.Error(w, "data required (max 2000 bytes)", http.StatusBadRequest)
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if size < 64 || size > 1024 {
		size = 256
	}
	png, err := qrcode.Encode(data, qrcode.Medium, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(png)
}

func localIPs() []net.IP {
	ips := []net.IP{net.IPv4(127, 0, 0, 1)}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				ips = append(ips, ipn.IP)
			}
		}
	}
	return ips
}

func selfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "qrelay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  append(localIPs(), net.IPv6loopback),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	fmt.Println("self-signed certificate generated; accept the browser warning once")
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}
