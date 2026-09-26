package limiter

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestLimiter_BurstAndRejection(t *testing.T) {
	// 1 token per second, burst of 3
	l := New(1.0, 3, 0, time.Minute)
	defer l.Stop()

	key := "test-client"

	// First 3 requests should succeed
	for i := 0; i < 3; i++ {
		allowed, _ := l.Allow(key)
		if !allowed {
			t.Fatalf("request %d should have been allowed", i+1)
		}
	}

	// 4th request exceeds burst capacity and should be rejected
	allowed, retryAfter := l.Allow(key)
	if allowed {
		t.Fatalf("4th request should have been rejected")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter duration, got %v", retryAfter)
	}
}

func TestLimiter_Refill(t *testing.T) {
	// 5 tokens per second, burst of 2
	l := New(5.0, 2, 0, time.Minute)
	defer l.Stop()

	key := "refill-client"

	// Consume both tokens
	l.Allow(key)
	l.Allow(key)

	allowed, _ := l.Allow(key)
	if allowed {
		t.Fatalf("immediate 3rd request should be rejected")
	}

	// Wait 250ms -> 5 * 0.25 = 1.25 tokens refilled
	time.Sleep(250 * time.Millisecond)

	allowed, _ = l.Allow(key)
	if !allowed {
		t.Fatalf("request after refill should be allowed")
	}
}

func TestLimiter_Cleanup(t *testing.T) {
	// TTL of 50ms
	l := New(1.0, 5, 0, 50*time.Millisecond)
	defer l.Stop()

	l.Allow("client-1")
	l.Allow("client-2")

	if count := l.Count(); count != 2 {
		t.Fatalf("expected 2 clients, got %d", count)
	}

	time.Sleep(80 * time.Millisecond)
	l.Cleanup()

	if count := l.Count(); count != 0 {
		t.Fatalf("expected 0 clients after cleanup, got %d", count)
	}
}

func TestLimiter_Middleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	l := New(1.0, 2, 0, time.Minute)
	defer l.Stop()

	r.Use(l.Middleware(IPKeyExtractor))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 1st request -> 200
	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w1.Code)
	}

	// 2nd request -> 200
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w2.Code)
	}

	// 3rd request -> 429
	req3 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", w3.Code)
	}
	if w3.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header to be set")
	}
}

func TestLimiter_Middleware_BypassEmptyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Strict limiter: 1 token capacity, burst of 1
	l := New(1.0, 1, 0, time.Minute)
	defer l.Stop()

	// Extractor returns empty string (simulating unauthenticated client with IP restriction removed)
	emptyExtractor := func(c *gin.Context) string {
		return ""
	}

	r.Use(l.Middleware(emptyExtractor))
	r.GET("/unrestricted", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Fire multiple requests - none should be rate limited or blocked
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/unrestricted", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d was restricted (status %d), expected 200 OK", i+1, w.Code)
		}
	}
}

func TestLimiter_UserOrIPKeyExtractor_NoIPRestriction(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Case 1: Unauthenticated request -> should return empty string (no IP restriction)
	c1, _ := gin.CreateTestContext(httptest.NewRecorder())
	c1.Request = httptest.NewRequest(http.MethodGet, "/api/test", nil)
	c1.Request.RemoteAddr = "192.168.1.100:1234"
	if key := UserOrIPKeyExtractor(c1); key != "" {
		t.Fatalf("expected empty key for unauthenticated request without IP restriction, got %q", key)
	}

	// Case 2: Authenticated via userID in context -> should return "user:<uid>"
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request = httptest.NewRequest(http.MethodGet, "/api/test", nil)
	c2.Set("userID", "user-uuid-1234")
	if key := UserOrIPKeyExtractor(c2); key != "user:user-uuid-1234" {
		t.Fatalf("expected 'user:user-uuid-1234', got %q", key)
	}

	// Case 3: Authenticated via Bearer token in header -> should return "tok:<token>"
	c3, _ := gin.CreateTestContext(httptest.NewRecorder())
	c3.Request = httptest.NewRequest(http.MethodGet, "/api/test", nil)
	c3.Request.Header.Set("Authorization", "Bearer sample-jwt-token")
	if key := UserOrIPKeyExtractor(c3); key != "tok:sample-jwt-token" {
		t.Fatalf("expected 'tok:sample-jwt-token', got %q", key)
	}

	// Case 4: Authenticated via token query parameter -> should return "tok:<token>"
	c4, _ := gin.CreateTestContext(httptest.NewRecorder())
	c4.Request = httptest.NewRequest(http.MethodGet, "/api/test?token=query-jwt-token", nil)
	if key := UserOrIPKeyExtractor(c4); key != "tok:query-jwt-token" {
		t.Fatalf("expected 'tok:query-jwt-token', got %q", key)
	}

	// Case 5: UserKeyExtractor behaves identically
	if key := UserKeyExtractor(c1); key != "" {
		t.Fatalf("expected empty key from UserKeyExtractor, got %q", key)
	}
}
