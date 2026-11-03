package tunnel

import (
	"context"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	scanDialTimeout    = 3 * time.Second
	scanTestTimeout    = 8 * time.Second
	scanParallelism    = 16
	scanWantedRoutes   = 3
	scanResolveTimeout = 3 * time.Second
)

// Route is one way to reach Apps Script: a Google IP and the SNI the network sees.
type Route struct {
	GoogleIP    string        `json:"googleIP"`
	FrontDomain string        `json:"frontDomain"`
	Latency     time.Duration `json:"latency"`
}

// Google front-end addresses to try. Any of them can serve script.google.com behind another Google SNI.
var scanGoogleIPs = []string{
	"216.239.38.120", "216.239.36.120", "216.239.34.120", "216.239.32.120",
	"142.250.185.4", "142.250.186.36", "142.250.74.100", "142.250.179.196",
	"142.251.36.4", "142.251.39.100", "172.217.16.132", "172.217.18.4",
	"172.217.21.164", "172.217.169.100", "74.125.24.103", "74.125.200.99",
	"64.233.160.99", "108.177.14.99", "173.194.73.99", "209.85.233.99",
}

// SNIs to show the network, most common first.
var scanFrontDomains = []string{
	"www.google.com", "google.com", "www.gstatic.com", "fonts.gstatic.com", "www.googleapis.com",
	"play.google.com", "docs.google.com", "drive.google.com", "accounts.google.com", "mail.google.com",
}

// Hosts whose current DNS answers add nearby Google front ends to the candidates.
var scanResolveHosts = []string{"www.google.com", "www.gstatic.com", "www.googleapis.com"}

// ScanRoutes looks for Google IP and SNI pairs that reach the given mailbox from this network and returns
// the working ones, fastest first. progress, if set, is called after each test.
func ScanRoutes(ctx context.Context, mailbox MailboxConfig, progress func(done, total int)) []Route {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ips := reachableIPs(ctx, candidateIPs(ctx))
	type candidate struct{ ip, sni string }
	var candidates []candidate
	for _, sni := range scanFrontDomains {
		for _, ip := range ips {
			candidates = append(candidates, candidate{ip, sni})
		}
	}

	var (
		mutex  sync.Mutex
		routes []Route
		done   atomic.Int32
		next   atomic.Int32
		group  sync.WaitGroup
	)
	for range scanParallelism {
		group.Add(1)
		go func() {
			defer group.Done()
			for ctx.Err() == nil {
				index := int(next.Add(1)) - 1
				if index >= len(candidates) {
					return
				}
				route := Route{GoogleIP: candidates[index].ip, FrontDomain: candidates[index].sni}
				if latency, ok := testRoute(ctx, mailbox, route); ok {
					route.Latency = latency
					mutex.Lock()
					routes = append(routes, route)
					if len(routes) >= scanWantedRoutes {
						cancel()
					}
					mutex.Unlock()
				}
				if progress != nil {
					progress(int(done.Add(1)), len(candidates))
				}
			}
		}()
	}
	group.Wait()

	sort.Slice(routes, func(i, j int) bool { return routes[i].Latency < routes[j].Latency })
	return routes
}

// TestRoute checks one route with a real request to the mailbox.
func TestRoute(ctx context.Context, mailbox MailboxConfig, route Route) (time.Duration, bool) {
	return testRoute(ctx, mailbox, route)
}

func testRoute(ctx context.Context, mailbox MailboxConfig, route Route) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(ctx, scanTestTimeout)
	defer cancel()
	mailbox.GoogleIP, mailbox.FrontDomain = route.GoogleIP, route.FrontDomain
	started := time.Now()
	if err := NewMailbox(mailbox).Ping(ctx); err != nil {
		return 0, false
	}
	return time.Since(started), true
}

// candidateIPs merges current DNS answers for Google hosts with the built-in list, without duplicates.
func candidateIPs(ctx context.Context) []string {
	seen := map[string]bool{}
	var ips []string
	add := func(ip string) {
		if !seen[ip] {
			seen[ip] = true
			ips = append(ips, ip)
		}
	}
	resolveCtx, cancel := context.WithTimeout(ctx, scanResolveTimeout)
	defer cancel()
	for _, host := range scanResolveHosts {
		addresses, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
		if err != nil {
			continue
		}
		for _, address := range addresses {
			// Poisoned answers in Iran point to private addresses.
			if ip := address.IP.To4(); ip != nil && !ip.IsPrivate() && !ip.IsLoopback() {
				add(ip.String())
			}
		}
	}
	for _, ip := range scanGoogleIPs {
		add(ip)
	}
	return ips
}

// reachableIPs keeps the IPs that accept a TCP connection on port 443, fastest first.
func reachableIPs(ctx context.Context, ips []string) []string {
	type result struct {
		ip      string
		latency time.Duration
	}
	results := make(chan result, len(ips))
	var group sync.WaitGroup
	for _, ip := range ips {
		group.Add(1)
		go func() {
			defer group.Done()
			started := time.Now()
			conn, err := (&net.Dialer{Timeout: scanDialTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
			if err == nil {
				conn.Close()
				results <- result{ip, time.Since(started)}
			}
		}()
	}
	group.Wait()
	close(results)
	var reachable []result
	for found := range results {
		reachable = append(reachable, found)
	}
	sort.Slice(reachable, func(i, j int) bool { return reachable[i].latency < reachable[j].latency })
	ips = ips[:0]
	for _, found := range reachable {
		ips = append(ips, found.ip)
	}
	return ips
}
