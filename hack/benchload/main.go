// Command benchload sends HTTP requests at a fixed rate for the runtime
// comparison in hack/bench_runtime.py.
//
// It never waits for a previous response before scheduling the next request
// (no coordinated omission), never retries, and opens a new TCP connection per
// request so an update shows the service's real routing behavior. One JSON
// row per request is written so server latency and late client dispatch can
// be told apart afterwards. The 512 in-flight slots exceed 200 requests per
// second times the 2 second timeout (400) plus scheduling margin.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type row struct {
	Index     int     `json:"index"`
	Offset    float64 `json:"offset_s"`
	Lag       float64 `json:"dispatch_lag_ms"`
	Latency   float64 `json:"latency_ms"`
	Scheduled float64 `json:"scheduled_latency_ms"`
	Status    int     `json:"status"`
	Bytes     int     `json:"bytes"`
	Version   string  `json:"version"`
	Error     string  `json:"error,omitempty"`
}

func main() {
	urlsArg := flag.String("urls", "", "comma separated URLs, requests rotate across them")
	out := flag.String("out", "", "raw JSONL output path")
	duration := flag.Duration("duration", 30*time.Second, "schedule duration")
	rate := flag.Int("rate", 200, "total requests per second")
	slots := flag.Int("slots", 512, "maximum requests in flight")
	bodySize := flag.Int("body-size", 1024, "expected response body size")
	flag.Parse()
	if *urlsArg == "" || *out == "" || *rate <= 0 || *slots <= 0 || *bodySize < 3 {
		fmt.Fprintln(os.Stderr, "usage: benchload --urls=URL[,URL...] --out=FILE [--duration=30s --rate=200]")
		os.Exit(2)
	}
	urls := strings.Split(*urlsArg, ",")
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	var mu sync.Mutex
	write := func(r row) {
		mu.Lock()
		defer mu.Unlock()
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
			DialContext:       (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		},
		Timeout: 2 * time.Second,
	}
	padding := strings.Repeat("x", *bodySize-3)
	sem := make(chan struct{}, *slots)
	var wg sync.WaitGroup
	begin := time.Now()
	n := int(duration.Seconds() * float64(*rate))
	for i := 0; i < n; i++ {
		at := begin.Add(time.Duration(float64(i) * float64(time.Second) / float64(*rate)))
		time.Sleep(time.Until(at))
		now := time.Now()
		r := row{Index: i, Offset: now.Sub(begin).Seconds(), Lag: ms(now.Sub(at))}
		select {
		case sem <- struct{}{}:
			wg.Add(1)
			go func(url string, at time.Time, r row) {
				defer wg.Done()
				defer func() { <-sem }()
				start := time.Now()
				check(client, url, padding, *bodySize, &r)
				r.Latency = ms(time.Since(start))
				r.Scheduled = ms(time.Since(at))
				write(r)
			}(urls[i%len(urls)], at, r)
		default:
			r.Error = "generator-concurrency-limit"
			write(r)
		}
	}
	wg.Wait()
}

func check(client *http.Client, url, padding string, size int, r *row) {
	resp, err := client.Get(url)
	if err != nil {
		r.Error = err.Error()
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(size)+1))
	resp.Body.Close()
	r.Status = resp.StatusCode
	r.Bytes = len(body)
	if len(body) >= 2 {
		r.Version = string(body[:2])
	}
	switch {
	case err != nil:
		r.Error = err.Error()
	case resp.StatusCode != http.StatusOK:
		r.Error = "http-status"
	case len(body) != size || (r.Version != "v1" && r.Version != "v2") || string(body) != r.Version+" "+padding:
		r.Error = "wrong-body"
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
