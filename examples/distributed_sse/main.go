package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	instanceCount = 3    // number of server instances
	basePort      = 8080 // base port number
	proxyPort     = 9000 // ReverseProxy listen port
)

func main() {
	// Start a local mock Redis server
	mr, err := miniredis.Run()
	if err != nil {
		log.Fatalf("Failed to start embedded Redis server: %v", err)
	}
	defer mr.Close()
	log.Printf("Embedded Redis server started at %s", mr.Addr())

	// Connect to the local mock Redis
	redisClient := redis.NewClient(&redis.Options{
		Addr: mr.Addr(), // Use miniredis address
	})

	// Test Redis connection
	ctx := context.Background()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Printf("Connected to Redis server")

	var (
		servers    []*serverInstance
		serverURLs []*url.URL
		shutdownWg sync.WaitGroup
	)

	// Create and start multiple server instances
	for i := 0; i < instanceCount; i++ {
		port := basePort + i
		addr := fmt.Sprintf("localhost:%d", port)
		instance := createServerInstance(i, addr, redisClient)

		shutdownWg.Add(1)
		go func(inst *serverInstance) {
			defer shutdownWg.Done()
			if err := inst.start(); err != nil && err != http.ErrServerClosed {
				log.Printf("Instance %d failed to start: %v", inst.id, err)
			}
		}(instance)

		serverURL, _ := url.Parse(fmt.Sprintf("http://%s", addr))
		serverURLs = append(serverURLs, serverURL)
		servers = append(servers, instance)
		log.Printf("Instance %d started at %s", i, addr)
	}

	time.Sleep(1 * time.Second) // Wait for all servers to start

	// Create ReverseProxy as a unified entry
	proxyAddr := fmt.Sprintf(":%d", proxyPort)
	proxy := createReverseProxy(serverURLs)
	proxyServer := &http.Server{
		Addr:    proxyAddr,
		Handler: proxy,
	}

	// Start ReverseProxy
	shutdownWg.Add(1)
	go func() {
		defer shutdownWg.Done()
		log.Printf("ReverseProxy started at %s", proxyAddr)
		if err := proxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("ReverseProxy failed to start: %v", err)
		}
	}()

	time.Sleep(1 * time.Second) // Wait for ReverseProxy to start

	// Use mcp-go client to test connection and make 10 tool calls
	testWithClient(fmt.Sprintf("http://localhost:%d", proxyPort))

	// Gracefully shutdown ReverseProxy
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxyServer.Shutdown(ctx)
	log.Println("ReverseProxy shut down")

	// Gracefully shutdown all servers
	for _, s := range servers {
		s.shutdown()
	}

	shutdownWg.Wait()
	log.Println("All servers shut down")
}

// 创建带有轮询策略的ReverseProxy
func createReverseProxy(targets []*url.URL) http.Handler {
	var current int64
	mutex := &sync.Mutex{}

	director := func(req *http.Request) {
		mutex.Lock()
		target := targets[current%int64(len(targets))]
		current++
		mutex.Unlock()

		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path, req.URL.RawPath = joinURLPath(target, req.URL)
		if target.RawQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = target.RawQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// 显式禁用User-Agent避免默认值
			req.Header.Set("User-Agent", "")
		}
	}

	return &httputil.ReverseProxy{Director: director}
}

// 辅助函数：连接URL路径
func joinURLPath(a, b *url.URL) (path, rawpath string) {
	if a.RawPath == "" && b.RawPath == "" {
		return singleJoiningSlash(a.Path, b.Path), ""
	}
	apath := a.EscapedPath()
	bpath := b.EscapedPath()
	return singleJoiningSlash(apath, bpath), singleJoiningSlash(a.RawPath, b.RawPath)
}

// 辅助函数：确保只有一个斜杠连接两个路径
func singleJoiningSlash(a, b string) string {
	aslash := len(a) > 0 && a[len(a)-1] == '/'
	bslash := len(b) > 0 && b[0] == '/'
	if aslash && bslash {
		return a + b[1:]
	}
	if !aslash && !bslash {
		return a + "/" + b
	}
	return a + b
}

// 使用mcp-go客户端进行测试
func testWithClient(baseURL string) {
	log.Printf("Starting client test: %s", baseURL)
	time.Sleep(1 * time.Second) // Ensure server is fully started

	// Create SSE client
	sseClient, err := client.NewSSEMCPClient(baseURL + "/sse")
	if err != nil {
		log.Printf("Failed to create client: %v", err)
		return
	}

	// Start client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = sseClient.Start(ctx)
	if err != nil {
		log.Printf("Failed to start client: %v", err)
		return
	}
	defer sseClient.Close()

	// Initialize client
	_, err = sseClient.Initialize(ctx, mcp.InitializeRequest{
		Request: mcp.Request{
			Method: string(mcp.MethodInitialize),
		},
	})
	if err != nil {
		log.Printf("Failed to initialize client: %v", err)
		return
	}

	log.Printf("Client connection successful, starting tool call test")

	// Make 10 tool calls
	for i := 0; i < 10; i++ {
		// Use CallTool method
		request := mcp.CallToolRequest{
			Request: mcp.Request{
				Method: "tools/call",
			},
		}
		request.Params.Name = "hello"
		result, err := sseClient.CallTool(ctx, request)

		if err != nil {
			log.Printf("Tool call %d failed: %v", i+1, err)
			continue
		}

		// Output result
		log.Printf("Tool call %d successful, result: %s", i+1, result.Content)
	}

	log.Printf("Client test completed")
}

// serverInstance represents a server instance
type serverInstance struct {
	id         int
	addr       string
	mcp        *server.MCPServer
	sse        *server.SSEServer
	dist       *server.DistributedSSEServer
	httpServer *http.Server
}

// Create new server instance
func createServerInstance(id int, addr string, redisClient redis.UniversalClient) *serverInstance {
	// Create MCP server
	mcpServer := server.NewMCPServer(fmt.Sprintf("MCP instance-%d", id), "1.0")

	// Add ping tool
	mcpServer.AddTool(
		mcp.Tool{
			Name:        "ping",
			Description: "Ping test tool",
		},
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Result: mcp.Result{},
			}, nil
		},
	)

	// Add hello tool
	mcpServer.AddTool(
		mcp.Tool{
			Name:        "hello",
			Description: "Hello World test tool",
		},
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Result: mcp.Result{},
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: "Hello world from instance " + fmt.Sprintf("%d", id) +
							"! Request time: " + time.Now().Format(time.RFC3339),
					},
				},
			}, nil
		},
	)

	// Create SSE server
	sseServer := server.NewSSEServer(mcpServer,
		server.WithSSEEndpoint("/sse"),
		server.WithMessageEndpoint("/message"),
	)

	// Create distributed server (handles both SSE and messages)
	distServer := server.NewDistributedServer(sseServer).
		WithRedisClient(redisClient).
		WithServerAddr(addr)

	// Create HTTP server
	mux := http.NewServeMux()
	mux.Handle("/sse", distServer.SSEHandler())
	mux.Handle("/message", distServer.MessageHandler())

	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return &serverInstance{
		id:         id,
		addr:       addr,
		mcp:        mcpServer,
		sse:        sseServer,
		dist:       distServer,
		httpServer: httpServer,
	}
}

// Start server
func (s *serverInstance) start() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown server
func (s *serverInstance) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.httpServer.Shutdown(ctx)
}
