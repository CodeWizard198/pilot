package gateway

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"pilot/internal/discovery"
	"strings"

	"pilot/internal/router"
	"time"
)

type HTTPConfig struct {
	Addr           string        `mapstructure:"addr"`
	ReadTimeout    time.Duration `mapstructure:"read_timeout"`
	WriteTimeout   time.Duration `mapstructure:"write_timeout"`
	MaxHeaderBytes int           `mapstructure:"max_header_bytes"`
	MaxBodyBytes   int           `mapstructure:"max_body_bytes"`
}

type EtcdConfig struct {
	Endpoints             []string      `mapstructure:"endpoints"`
	ServiceMetadataPrefix string        `mapstructure:"service_metadata_prefix"`
	ServerDiscoveryPrefix string        `mapstructure:"server_discovery_prefix"`
	DialTimeout           time.Duration `mapstructure:"dail_timeout"`
}

type Config struct {
	HTTP HTTPConfig `mapstructure:"http"`
	Etcd EtcdConfig `mapstructure:"etcd"`
}

// crosMiddleware 跨域支持
func crosMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")

		reqHeaders := r.Header.Get("Access-Control-Request-Headers")
		if reqHeaders == "" {
			reqHeaders = "Content-Type,Authorization,X-Requested-With,X-Csrf-Token"
		}
		// 规范空白与大小写
		reqHeaders = strings.Join(func(items []string) []string {
			out := make([]string, 0, len(items))
			for _, h := range items {
				h = strings.TrimSpace(h)
				if h != "" {
					out = append(out, h)
				}
			}
			return out
		}(strings.Split(reqHeaders, ",")), ",")
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)

		// 缓存预检结果减少预检请求次数
		w.Header().Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// bodyLimitMiddleware 限制请求体大小
func bodyLimitMiddleware(next http.Handler, maxBytes int) http.Handler {
	if maxBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxBytes))
		next.ServeHTTP(w, r)
	})
}

func DefaultConfig() *Config {
	return &Config{
		HTTP: HTTPConfig{
			Addr:           ":8080",
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,  // 1MB header
			MaxBodyBytes:   10 << 20, // 10MB body
		},
		Etcd: EtcdConfig{
			Endpoints:             []string{"localhost:2379"},
			DialTimeout:           5 * time.Second,
			ServiceMetadataPrefix: "/services/",
			ServerDiscoveryPrefix: "/discovery/",
		},
	}
}

type HTTPGateway struct {
	config  *Config
	router  *router.HTTPRouter
	watcher *discovery.Watcher
	server  *http.Server
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewHTTPGateway(config *Config) (*HTTPGateway, error) {
	if config == nil {
		config = DefaultConfig()
	}

	ctx, cancel := context.WithCancel(context.Background())

	// 创建路由树
	r := router.NewHTTPRouter()

	// 创建etcd watcher
	watcher, err := discovery.NewWatcher(
		config.Etcd.Endpoints,
		config.Etcd.DialTimeout,
		config.Etcd.ServiceMetadataPrefix,
		config.Etcd.ServerDiscoveryPrefix,
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create watcher: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		r.ServeHTTP(w, req)
	})

	var handler http.Handler = mux
	handler = bodyLimitMiddleware(handler, config.HTTP.MaxBodyBytes)
	handler = crosMiddleware(handler)

	server := &http.Server{
		Addr:           config.HTTP.Addr,
		Handler:        handler,
		ReadTimeout:    config.HTTP.ReadTimeout,
		WriteTimeout:   config.HTTP.WriteTimeout,
		MaxHeaderBytes: config.HTTP.MaxHeaderBytes,
	}

	g := &HTTPGateway{
		config:  config,
		router:  r,
		watcher: watcher,
		server:  server,
		ctx:     ctx,
		cancel:  cancel,
	}

	return g, nil
}

// Start 启动 HTTP 网关
func (g *HTTPGateway) Start() error {
	// 启动etcd监听器
	if err := g.watcher.Start(); err != nil {
		return fmt.Errorf("failed to start watcher: %w", err)
	}

	// 开启协程处理事件
	go g.processEvents()

	// 开启http服务
	log.Printf("Starting HTTP gateway on %s", g.config.HTTP.Addr)
	log.Printf("Watching etcd endpoints: %v", g.config.Etcd.Endpoints)
	log.Printf("Service metadata path: %s", g.config.Etcd.ServiceMetadataPrefix)
	log.Printf("Service discovery path: %s", g.config.Etcd.ServerDiscoveryPrefix)

	go func() {
		if err := g.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	return nil
}

// processEvents 处理服务发现事件
func (g *HTTPGateway) processEvents() {
	for {
		select {
		case <-g.ctx.Done():
			return
		case event := <-g.watcher.Events():
			g.handleEvent(event)
		}
	}
}

// handleEvent 处理服务发现事件
func (g *HTTPGateway) handleEvent(event *discovery.ServiceEvent) {

	switch event.Type {
	case discovery.EventAdd:
		if err := g.router.RegisterService(event.Service); err != nil {
			log.Printf("Failed to register service %s: %v", event.Service.ServiceName, err)
		}

	case discovery.EventUpdate:
		if err := g.router.RegisterService(event.Service); err != nil {
			log.Printf("Failed to update service %s: %v", event.Service.ServiceName, err)
		}

	case discovery.EventDelete:
		if err := g.router.UnRegisterService(event.Service); err != nil {
			log.Printf("Failed to unregister service %s: %v", event.Service.ServiceName, err)
		}
	}
}

// Stop 优雅停止网关
func (g *HTTPGateway) Stop() error {
	log.Println("Stopping HTTP gateway...")

	// 停止处理事件
	g.cancel()

	// 停止http服务
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := g.server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// 停止etcd监听器
	if err := g.watcher.Stop(); err != nil {
		log.Printf("Watcher stop error: %v", err)
	}

	// 关闭路由器
	g.router.Close()

	log.Println("HTTP gateway stopped")
	return nil
}
