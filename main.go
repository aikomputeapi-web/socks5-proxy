package main

import (
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

var (
	lastScrapeTime time.Time
	nextScrapeTime time.Time
	scrapeMu       sync.RWMutex
	refreshChan    = make(chan struct{}, 1) // manual refresh trigger
)

func getScrapeTimes() (last, next time.Time) {
	scrapeMu.RLock()
	defer scrapeMu.RUnlock()
	return lastScrapeTime, nextScrapeTime
}

func main() {
	cfg := ParseConfig()

	log.Printf("socks5-pool starting...")
	log.Printf("  listen:   %s", cfg.ListenAddr)
	log.Printf("  status:   %s", cfg.StatusAddr)
	log.Printf("  sources:  %s", cfg.ScrapeURLs)
	log.Printf("  scrape:   every %s", cfg.ScrapeInterval)

	pool := NewProxyPool()

	// Initial scrape + check in background so servers can start listening immediately
	go func() {
		refreshPool(cfg, pool)
		if pool.Size() == 0 {
			log.Printf("[warn] no alive proxies found, will retry on next scrape cycle")
		}
	}()

	// Background: periodic scrape + manual refresh
	go func() {
		ticker := time.NewTicker(cfg.ScrapeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				refreshPool(cfg, pool)
			case <-refreshChan:
				log.Printf("[main] manual refresh triggered")
				refreshPool(cfg, pool)
				ticker.Reset(cfg.ScrapeInterval)
			}
		}
	}()

	// Background: random proxy rotation every 3-6 minutes
	// If pool is empty, trigger immediate refresh instead of rotating
	go func() {
		for {
			delay := 3*time.Minute + time.Duration(rand.Intn(4))*time.Minute
			time.Sleep(delay)
			if pool.Size() == 0 {
				log.Printf("[main] pool empty, triggering immediate refresh")
				TriggerRefresh()
			} else if pool.Size() > 1 {
				pool.SwitchNext()
			}
		}
	}()

	// Background: status dashboard
	go func() {
		status := NewStatusServer(pool)
		log.Printf("[status] dashboard at http://%s", cfg.StatusAddr)
		if err := status.Start(cfg.StatusAddr); err != nil {
			log.Printf("[status] failed to start: %v", err)
		}
	}()

	// Start SOCKS5 server (blocks)
	server := NewServer(cfg.ListenAddr, pool)
	log.Fatal(server.Start())
}

func refreshPool(cfg *Config, pool *ProxyPool) {
	urls := strings.Split(cfg.ScrapeURLs, ",")
	var allProxies []Proxy
	seenGlobal := make(map[string]bool)
	
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		proxies, err := Scrape(u)
		if err != nil {
			log.Printf("[error] scrape failed for %s: %v", u, err)
			continue
		}
		for _, p := range proxies {
			if !seenGlobal[p.Addr()] {
				seenGlobal[p.Addr()] = true
				allProxies = append(allProxies, p)
			}
		}
	}

	alive := CheckProxies(allProxies, cfg.CheckTimeout, cfg.MaxConcurrent)
	pool.Update(alive)

	scrapeMu.Lock()
	lastScrapeTime = time.Now()
	nextScrapeTime = lastScrapeTime.Add(cfg.ScrapeInterval)
	scrapeMu.Unlock()

	log.Printf("[main] pool refreshed: %d alive proxies", pool.Size())
}

// TriggerRefresh sends a manual refresh signal (non-blocking).
func TriggerRefresh() {
	select {
	case refreshChan <- struct{}{}:
	default:
		// already pending
	}
}
