package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// RedisKeyPrefix is the Redis key prefix for storing SSE server addresses
	RedisKeyPrefix = "sse_server_addr:"

	// RedisKeyExpiration is the expiration time for Redis keys
	RedisKeyExpiration = 10 * time.Second

	// RedisKeyRenewalInterval is the renewal interval for Redis keys
	RedisKeyRenewalInterval = RedisKeyExpiration / 2
)

// DistributedSSEServer supports multi-instance distributed deployment of MCP services
// via Redis to record sessionId -> instance address
type DistributedSSEServer struct {
	sseServer   *SSEServer
	redisClient redis.UniversalClient
	serverAddr  string
}

// NewDistributedServer creates a new distributed server
func NewDistributedServer(sseServer *SSEServer) *DistributedSSEServer {
	return &DistributedSSEServer{
		sseServer: sseServer,
	}
}

// WithRedisClient sets the redis client
func (d *DistributedSSEServer) WithRedisClient(client redis.UniversalClient) *DistributedSSEServer {
	d.redisClient = client
	return d
}

// WithServerAddr sets the current instance http server address
func (d *DistributedSSEServer) WithServerAddr(addr string) *DistributedSSEServer {
	d.serverAddr = addr
	return d
}

// getRedisKey returns the Redis key name for a given sessionID
func getRedisKey(sessionID string) string {
	log.Println("getRedisKey, sessionID=", sessionID)
	return RedisKeyPrefix + sessionID
}

// getLocalServerAddr returns the current instance's server address
func (d *DistributedSSEServer) getLocalServerAddr() (string, error) {
	if d.serverAddr != "" {
		return d.serverAddr, nil
	}

	if d.sseServer != nil && d.sseServer.srv != nil && d.sseServer.srv.Addr != "" {
		return d.sseServer.srv.Addr, nil
	}

	return "", fmt.Errorf("server address not set")
}

// SSEHandler returns the http.Handler for distributed SSE
func (d *DistributedSSEServer) SSEHandler() http.Handler {
	return http.HandlerFunc(d.handleSSE)
}

// MessageHandler returns the http.Handler for distributed messages
func (d *DistributedSSEServer) MessageHandler() http.Handler {
	return http.HandlerFunc(d.handleMessage)
}

// handleSSE overrides the SSE connection establishment process to support distributed session registration
func (d *DistributedSSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	// get or create sessionID
	sessionID := d.sseServer.getSessionID(r, true)
	if sessionID == "" {
		http.Error(w, "Failed to get or create sessionID", http.StatusInternalServerError)
		return
	}

	// set to header to use in SSEServer.handleSSE()
	r.Header.Set("Mcp-Session-Id", sessionID)

	// get current instance address
	addr, err := d.getLocalServerAddr()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// write server address to Redis, key is sse_server_addr:{sessionID}
	ctx := r.Context()
	redisKey := getRedisKey(sessionID)
	if err := d.redisClient.Set(ctx, redisKey, addr, RedisKeyExpiration).Err(); err != nil {
		http.Error(w, "Failed to write session addr to redis", http.StatusInternalServerError)
		return
	}

	// start goroutine to periodically renew Redis key
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(RedisKeyRenewalInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				d.redisClient.Expire(ctx, redisKey, RedisKeyExpiration)
			case <-stopCh:
				return
			}
		}
	}()

	// exit automatically stop and clean up Redis key on exit
	defer func() {
		close(stopCh)
		wg.Wait()
		d.redisClient.Del(ctx, redisKey)
	}()

	// execute original handleSSE() logic
	if d.sseServer != nil {
		d.sseServer.handleSSE(w, r)
	}
}

// handleMessage overrides message handling to support distributed forwarding
func (d *DistributedSSEServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	// get sessionId from request
	sessionID := d.sseServer.getSessionID(r, false)
	if sessionID == "" {
		d.sseServer.writeJSONRPCError(w, nil, mcp.INVALID_PARAMS, "Missing sessionId")
		return
	}

	// get sse instance address from redis
	ctx := r.Context()
	redisKey := getRedisKey(sessionID)
	targetAddr, err := d.redisClient.Get(ctx, redisKey).Result()
	if err != nil {
		d.sseServer.writeJSONRPCError(w, nil, mcp.INVALID_PARAMS, "Invalid session ID")
		return
	}

	// get local server address
	localAddr, err := d.getLocalServerAddr()
	if err == nil && targetAddr == localAddr {
		// local processing, check if session exists
		_, ok := d.sseServer.server.sessions.Load(sessionID)
		if ok {
			// call original handleMessage() logic
			if d.sseServer != nil {
				log.Println("local handleMessage")
				d.sseServer.handleMessage(w, r)
				return
			}
		} else {
			d.sseServer.writeJSONRPCError(w, nil, mcp.INVALID_PARAMS, "Invalid session ID")
			return
		}
	} else {
		// via reverse proxy forward request to target instance
		proxyURL := "http://" + targetAddr + r.URL.Path + "?" + r.URL.RawQuery
		proxyReq, err := http.NewRequestWithContext(ctx, r.Method, proxyURL, r.Body)
		if err != nil {
			d.sseServer.writeJSONRPCError(w, nil, mcp.INTERNAL_ERROR, "Failed to create proxy request")
			return
		}
		proxyReq.Header = r.Header.Clone()
		client := &http.Client{Timeout: 10 * time.Second}
		log.Println("sourceAddr=", localAddr, "targetAddr=", targetAddr, "proxyURL=", proxyURL)
		resp, err := client.Do(proxyReq)
		if err != nil {
			d.sseServer.writeJSONRPCError(w, nil, mcp.INTERNAL_ERROR, "Proxy request failed")
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}
