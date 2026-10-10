package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Only Addr and APIToken are user configuration. Other fields are internal
// protocol constants/test seams, not environment-configurable options.
type config struct {
	Addr, APIToken                                                                string
	AuthAddr, BaseURL, AuthBaseURL, AuthURL, ClientID, IDEVersion, IDEVersionCode string
	MaxConcurrent                                                                 int
	QueueTimeout, RequestTimeout, ShutdownTimeout                                 time.Duration
}

func defaultConfig() config {
	return config{Addr: "127.0.0.1:8723", AuthAddr: "127.0.0.1:8765", BaseURL: "https://trae-api-cn.mchost.guru", AuthBaseURL: "https://api.trae.cn", AuthURL: "https://www.trae.cn/authorization", ClientID: "ono9krqynydwx5", IDEVersion: "0.1.52", IDEVersionCode: "20260811", MaxConcurrent: 2, QueueTimeout: time.Minute, RequestTimeout: 5 * time.Minute, ShutdownTimeout: 30 * time.Second}
}
func loadConfig() (config, error) {
	c := defaultConfig()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TRAE_API_") && key != "TRAE_API_ADDR" && key != "TRAE_API_TOKEN" {
			return c, fmt.Errorf("%s is unsupported; only TRAE_API_ADDR and TRAE_API_TOKEN are supported", key)
		}
	}
	if addr := os.Getenv("TRAE_API_ADDR"); addr != "" {
		c.Addr = addr
	}
	c.APIToken = os.Getenv("TRAE_API_TOKEN")
	host, port, err := net.SplitHostPort(c.Addr)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("TRAE_API_ADDR must be host:port with port 1..65535")
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return c, fmt.Errorf("TRAE_API_ADDR must use localhost or a literal IP")
	}
	if !isLoopbackAddr(c.Addr) && c.APIToken == "" {
		return c, fmt.Errorf("TRAE_API_TOKEN is required for non-loopback listening")
	}
	if n == 8765 {
		return c, fmt.Errorf("TRAE_API_ADDR conflicts with the fixed authorization port 8765")
	}
	return c, nil
}
func isLoopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	return err == nil && (h == "localhost" || net.ParseIP(h).IsLoopback())
}
