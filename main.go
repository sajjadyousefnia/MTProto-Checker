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
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
)

const (
	defaultPort     = 3000
	testAppID       = 6
	testAppHash     = "eb06d4abfb49dc3eeb1aeb98ae0f581e"
	maxBodySize     = 50 * 1024 * 1024
	maxConcurrency  = 50
	defaultTimeout  = 8
	minTimeout      = 3
	maxTimeout      = 30
	shutdownTimeout = 5 * time.Second
)

type CheckRequest struct {
	Server  string `json:"server"`
	Port    int    `json:"port"`
	Secret  string `json:"secret"`
	Timeout int    `json:"timeout,omitempty"`
}

type CheckResponse struct {
	OK   bool  `json:"ok"`
	Ping int64 `json:"ping,omitempty"`
}

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

func checkProxy(ctx context.Context, server string, port int, secret string, timeoutSec int) (int64, error) {
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

	timeout := time.Duration(timeoutSec) * time.Second
	var ping int64
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
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

type concurrentLimiter struct {
	sem chan struct{}
}

func newLimiter(n int) *concurrentLimiter {
	return &concurrentLimiter{sem: make(chan struct{}, n)}
}

func (l *concurrentLimiter) Acquire() { l.sem <- struct{}{} }
func (l *concurrentLimiter) Release() { <-l.sem }

func batchCheck(ctx context.Context, proxies []CheckRequest, limit int, onResult func(int, CheckResponse)) {
	limiter := newLimiter(limit)
	var wg sync.WaitGroup

	for i, p := range proxies {
		wg.Add(1)
		go func(idx int, proxy CheckRequest) {
			defer wg.Done()
			limiter.Acquire()
			defer limiter.Release()

			timeout := proxy.Timeout
			if timeout < minTimeout {
				timeout = defaultTimeout
			}
			ping, err := checkProxy(ctx, proxy.Server, proxy.Port, proxy.Secret, timeout)
			if err != nil {
				onResult(idx, CheckResponse{OK: false})
			} else {
				onResult(idx, CheckResponse{OK: true, Ping: ping})
			}
		}(i, p)
	}

	wg.Wait()
}

func jsonResponse(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	port := defaultPort
	if p := os.Getenv("PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

		var req CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResponse(w, http.StatusBadRequest, CheckResponse{OK: false})
			return
		}

		timeout := req.Timeout
		if timeout < minTimeout || timeout > maxTimeout {
			timeout = defaultTimeout
		}

		start := time.Now()
		ping, err := checkProxy(r.Context(), req.Server, req.Port, req.Secret, timeout)
		elapsed := time.Since(start)

		if err != nil {
			log.Printf("CHECK FAIL %s:%d timeout=%ds (%v)", req.Server, req.Port, timeout, elapsed)
			jsonResponse(w, http.StatusOK, CheckResponse{OK: false})
		} else {
			log.Printf("CHECK OK   %s:%d %dms timeout=%ds (%v)", req.Server, req.Port, ping, timeout, elapsed)
			jsonResponse(w, http.StatusOK, CheckResponse{OK: true, Ping: ping})
		}
	})

	mux.HandleFunc("/check-batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

		var reqs []CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			jsonResponse(w, http.StatusBadRequest, nil)
			return
		}

		limit := 10
		if l := r.Header.Get("X-Concurrency"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}
		if limit < 1 {
			limit = 1
		}
		if limit > maxConcurrency {
			limit = maxConcurrency
		}

		timeout := defaultTimeout
		if len(reqs) > 0 && reqs[0].Timeout >= minTimeout && reqs[0].Timeout <= maxTimeout {
			timeout = reqs[0].Timeout
		}

		log.Printf("BATCH START %d proxies, concurrency=%d, timeout=%ds", len(reqs), limit, timeout)
		start := time.Now()

		results := make([]CheckResponse, len(reqs))
		batchCheck(r.Context(), reqs, limit, func(idx int, res CheckResponse) {
			results[idx] = res
		})

		working := 0
		for _, r := range results {
			if r.OK {
				working++
			}
		}
		log.Printf("BATCH DONE  %d/%d working (%v)", working, len(reqs), time.Since(start))

		jsonResponse(w, http.StatusOK, results)
	})

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

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-done
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Shutdown error: %v", err)
	}
	log.Println("Server stopped")
}
