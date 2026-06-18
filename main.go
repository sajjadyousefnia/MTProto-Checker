package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
)

const (
	defaultPort = 3000
	testAppID   = 6
	testAppHash = "eb06d4abfb49dc3eeb1aeb98ae0f581e"
)

// CheckRequest represents the JSON body of POST /check
type CheckRequest struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	Secret string `json:"secret"`
}

// CheckResponse represents the JSON response from POST /check
type CheckResponse struct {
	OK   bool  `json:"ok"`
	Ping int64 `json:"ping,omitempty"`
}

// parseProxyLink parses tg://proxy?server=...&port=...&secret=... links
func parseProxyLink(link string) (server string, port int, secret string, err error) {
	cleanLink := strings.TrimSpace(link)
	cleanLink = strings.Replace(cleanLink, ".&", "&", 1)

	u, err := url.Parse(cleanLink)
	if err != nil {
		return "", 0, "", errors.Wrap(err, "parse link")
	}

	q := u.Query()
	server = q.Get("server")
	portStr := q.Get("port")
	secret = q.Get("secret")

	if server == "" || portStr == "" || secret == "" {
		return "", 0, "", errors.New("missing server, port, or secret")
	}

	var p int
	if _, err := fmt.Sscanf(portStr, "%d", &p); err != nil {
		return "", 0, "", errors.Wrap(err, "parse port")
	}
	if p <= 0 || p > 65535 {
		return "", 0, "", errors.Errorf("invalid port %d", p)
	}

	return server, p, secret, nil
}

// decodeSecret decodes hex or base64url secret
func decodeSecret(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.Errorf("unable to decode secret %q as hex or base64url", s)
}

// checkProxy performs a real MTProto handshake through the MTProxy
func checkProxy(ctx context.Context, server string, port int, secret string) (int64, error) {
	addr := net.JoinHostPort(server, fmt.Sprintf("%d", port))

	decodedSecret, err := decodeSecret(secret)
	if err != nil {
		return 0, errors.Wrap(err, "decode secret")
	}

	resolver, err := dcs.MTProxy(addr, decodedSecret, dcs.MTProxyOptions{})
	if err != nil {
		return 0, errors.Wrap(err, "create MTProxy resolver")
	}

	client := telegram.NewClient(testAppID, testAppHash, telegram.Options{
		Resolver: resolver,
	})

	var ping int64
	checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	err = client.Run(checkCtx, func(ctx context.Context) error {
		start := time.Now()
		_, err := client.API().HelpGetNearestDC(ctx)
		if err != nil {
			return errors.Wrap(err, "help.getNearestDC")
		}
		ping = time.Since(start).Milliseconds()
		return nil
	})

	if err != nil {
		return 0, err
	}
	return ping, nil
}

// concurrentLimiter limits concurrent proxy checks
type concurrentLimiter struct {
	sem chan struct{}
}

func newLimiter(n int) *concurrentLimiter {
	return &concurrentLimiter{sem: make(chan struct{}, n)}
}

func (l *concurrentLimiter) Acquire() { l.sem <- struct{}{} }
func (l *concurrentLimiter) Release() { <-l.sem }

// batchCheck checks multiple proxies concurrently with a limit
func batchCheck(ctx context.Context, proxies []CheckRequest, limit int, onResult func(CheckRequest, CheckResponse)) {
	limiter := newLimiter(limit)
	var wg sync.WaitGroup

	for _, p := range proxies {
		wg.Add(1)
		go func(proxy CheckRequest) {
			defer wg.Done()
			limiter.Acquire()
			defer limiter.Release()

			ping, err := checkProxy(ctx, proxy.Server, proxy.Port, proxy.Secret)
			if err != nil {
				onResult(proxy, CheckResponse{OK: false})
			} else {
				onResult(proxy, CheckResponse{OK: true, Ping: ping})
			}
		}(p)
	}

	wg.Wait()
}

func main() {
	port := defaultPort
	if p := os.Getenv("PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}

	mux := http.NewServeMux()

	// POST /check — check a single proxy
	mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(CheckResponse{OK: false})
			return
		}

		ping, err := checkProxy(r.Context(), req.Server, req.Port, req.Secret)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(CheckResponse{OK: false})
		} else {
			json.NewEncoder(w).Encode(CheckResponse{OK: true, Ping: ping})
		}
	})

	// POST /check-batch — check multiple proxies concurrently
	mux.HandleFunc("/check-batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var reqs []CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		limit := 10
		if l := r.Header.Get("X-Concurrency"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}
		if limit < 1 {
			limit = 1
		}
		if limit > 50 {
			limit = 50
		}

		results := make([]CheckResponse, len(reqs))
		batchCheck(r.Context(), reqs, limit, func(p CheckRequest, res CheckResponse) {
			for i, req := range reqs {
				if req.Server == p.Server && req.Port == p.Port && req.Secret == p.Secret {
					results[i] = res
					break
				}
			}
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})

	// Serve static files from public/
	publicDir := filepath.Join(".", "public")
	fs := http.FileServer(http.Dir(publicDir))
	mux.Handle("/", fs)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Server running at http://localhost:%d", port)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-done
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Shutdown error: %v", err)
	}
	log.Println("Server stopped")
}
