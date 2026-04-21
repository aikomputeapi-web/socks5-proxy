package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Removed blockedCountries map as we are now strictly filtering for US proxies only

// CheckProxies concurrently checks a list of proxies.
// Strictly filters for US-based IPs and tests Google connectivity.
func CheckProxies(proxies []Proxy, timeout time.Duration, maxConcurrent int) []Proxy {
	var (
		mu    sync.Mutex
		alive []Proxy
		wg    sync.WaitGroup
		sem   = make(chan struct{}, maxConcurrent)
	)

	for _, p := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func(px Proxy) {
			defer wg.Done()
			defer func() { <-sem }()

			// First, immediately test if the proxy is alive and fast!
			if !checkGoogle(px, timeout) {
				return // Dead or too slow, skip it.
			}

			// Proxy is alive and fast! ONLY NOW do we check its geolocation
			country, city := LookupGeo(px.IP, timeout)
			px.Country = strings.TrimSpace(country)
			px.City = strings.TrimSpace(city)

			countryLower := strings.ToLower(px.Country)
			if countryLower != "united states" && countryLower != "us" {
				log.Printf("[checker] %s alive but skipped (%s - not US)", px.Addr(), px.Country)
				return
			}

			log.Printf("[checker] %s OK (%s %s)", px.Addr(), px.Country, px.City)
			mu.Lock()
			alive = append(alive, px)
			mu.Unlock()
		}(p)
	}

	wg.Wait()
	log.Printf("[checker] %d/%d proxies alive (Google-verified, US-only)", len(alive), len(proxies))
	return alive
}

// checkGoogle connects through the proxy to Google's 204 endpoint.
func checkGoogle(p Proxy, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", p.Addr(), timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	// SOCKS5 greeting
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return false
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || buf[0] != 0x05 {
		return false
	}

	// Connect to www.google.com:80 through proxy
	target := "www.google.com"
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(target))}
	req = append(req, []byte(target)...)
	req = append(req, 0x00, 0x50) // port 80

	if _, err := conn.Write(req); err != nil {
		return false
	}

	resp := make([]byte, 256)
	n, err := conn.Read(resp)
	if err != nil || n < 2 || resp[1] != 0x00 {
		return false
	}

	// Send HTTP request to Google's generate_204 endpoint
	httpReq := "GET /generate_204 HTTP/1.1\r\nHost: www.google.com\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return false
	}

	respBuf := make([]byte, 512)
	n, err = conn.Read(respBuf)
	if err != nil || n < 12 {
		return false
	}

	// Check we got HTTP response (200 or 204 both fine)
	return string(respBuf[:4]) == "HTTP"
}

// LookupGeo queries ip-api.com for IP geolocation.
func LookupGeo(ip string, timeout time.Duration) (country, city string) {
	conn, err := net.DialTimeout("tcp", "ip-api.com:80", timeout)
	if err != nil {
		return "Unknown", ""
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	req := fmt.Sprintf("GET /csv/%s?fields=country,city HTTP/1.1\r\nHost: ip-api.com\r\nConnection: close\r\n\r\n", ip)
	conn.Write([]byte(req))

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return "Unknown", ""
	}

	body := string(buf[:n])
	for i := 0; i < len(body)-3; i++ {
		if body[i:i+4] == "\r\n\r\n" {
			body = body[i+4:]
			break
		}
	}

	for i, c := range body {
		if c == ',' {
			return body[:i], body[i+1:]
		}
	}
	return body, ""
}
