// Command benchmark-proxy is a small TCP fault proxy for Redis and Milvus.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type fault struct {
	Mode    string `json:"mode"`
	DelayMS int    `json:"delay_ms"`
}
type proxy struct {
	sync.RWMutex
	fault    fault
	upstream string
	active   map[net.Conn]struct{}
}

func (p *proxy) current() fault { p.RLock(); defer p.RUnlock(); return p.fault }
func main() {
	redis := flag.String("redis", "redis:6379", "redis upstream")
	milvus := flag.String("milvus", "milvus-standalone:19530", "milvus upstream")
	flag.Parse()
	proxies := map[string]*proxy{"redis": {upstream: *redis, active: map[net.Conn]struct{}{}}, "milvus": {upstream: *milvus, active: map[net.Conn]struct{}{}}}
	for name, addr := range map[string]string{"redis": ":16379", "milvus": ":19530"} {
		go serveTCP(addr, proxies[name])
	}
	http.HandleFunc("/fault/", faultHandler(proxies))
	log.Fatal(http.ListenAndServe(":8099", nil))
}
func faultHandler(proxies map[string]*proxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := proxies[r.URL.Path[len("/fault/"):]]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == "POST" {
			var v fault
			if json.NewDecoder(r.Body).Decode(&v) != nil || !(v.Mode == "" || v.Mode == "drop" || v.Mode == "delay") || v.DelayMS < 0 {
				http.Error(w, "invalid fault", 400)
				return
			}
			p.Lock()
			p.fault = v
			if v.Mode == "drop" {
				for c := range p.active {
					_ = c.Close()
				}
			}
			p.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.current())
	}
}
func serveTCP(addr string, p *proxy) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		client, err := listener.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go forward(client, p)
	}
}
func forward(client net.Conn, p *proxy) {
	defer client.Close()
	p.Lock()
	p.active[client] = struct{}{}
	f := p.fault
	p.Unlock()
	defer func() { p.Lock(); delete(p.active, client); p.Unlock() }()
	if f.Mode == "drop" {
		return
	}
	if f.Mode == "delay" && f.DelayMS > 0 {
		time.Sleep(time.Duration(f.DelayMS) * time.Millisecond)
	}
	upstream, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); done <- struct{}{} }()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-done
}
