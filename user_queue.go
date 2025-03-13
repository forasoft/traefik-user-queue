// Package traefik_user_queue provides a concurrency limiting middleware for Traefik
package traefik_user_queue

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// Config holds the plugin configuration
type Config struct {
	// Existing fields
	MaxUsers              int    `json:"maxUsers"`
	SessionTimeoutSeconds int    `json:"sessionTimeoutSeconds"`
	QueueFullMessage      string `json:"queueFullMessage"`
	TrustForwardedFor     bool   `json:"trustForwardedFor"`
	// New field
	QueueFullPagePath string `json:"queueFullPagePath"`
}

// UserSession tracks a user's activity
type UserSession struct {
	IP           string
	LastActivity time.Time
}

// UserQueue represents the user queue middleware
type UserQueue struct {
	// Existing fields
	next              http.Handler
	name              string
	maxUsers          int
	sessionTimeout    time.Duration
	queueFullMessage  string
	queueFullPagePath string
	trustXFF          bool
	activeUsers       map[string]*UserSession
	mutex             sync.Mutex
	lastCleanup       time.Time
}

// CreateConfig creates the default plugin configuration
func CreateConfig() *Config {
	return &Config{
		MaxUsers:              100,
		SessionTimeoutSeconds: 300, // 5 minutes
		QueueFullMessage:      "The website is currently at maximum capacity. Please try again later.",
		TrustForwardedFor:     false,
		QueueFullPagePath:     "", // Empty string means use default message
	}
}

// New creates a new user queue plugin
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	return &UserQueue{
		next:              next,
		name:              name,
		maxUsers:          config.MaxUsers,
		sessionTimeout:    time.Duration(config.SessionTimeoutSeconds) * time.Second,
		queueFullMessage:  config.QueueFullMessage,
		queueFullPagePath: config.QueueFullPagePath,
		trustXFF:          config.TrustForwardedFor,
		activeUsers:       make(map[string]*UserSession),
		lastCleanup:       time.Now(),
	}, nil
}

// getClientIP extracts the client IP address from the request
func (q *UserQueue) getClientIP(req *http.Request) string {
	var ip string

	// Try X-Forwarded-For header if trusted
	if q.trustXFF {
		xff := req.Header.Get("X-Forwarded-For")
		if xff != "" {
			// X-Forwarded-For can contain multiple IPs, take the first one
			xffParts := splitHeaderValue(xff)
			if len(xffParts) > 0 {
				return xffParts[0]
			}
		}
	}

	// Fall back to direct connection IP
	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		// If there's an error, just use RemoteAddr directly
		return req.RemoteAddr
	}

	return ip
}

// splitHeaderValue splits a comma-separated header value
func splitHeaderValue(v string) []string {
	var values []string
	current := ""

	for _, c := range v {
		if c == ',' {
			values = append(values, current)
			current = ""
		} else if c != ' ' {
			current += string(c)
		}
	}

	if current != "" {
		values = append(values, current)
	}

	return values
}

// cleanupInactiveSessions removes sessions that have been inactive for longer than the timeout
func (q *UserQueue) cleanupInactiveSessions() {
	now := time.Now()

	// Only run cleanup every minute to reduce overhead
	if now.Sub(q.lastCleanup) < time.Minute {
		return
	}

	q.mutex.Lock()
	defer q.mutex.Unlock()

	q.lastCleanup = now

	// Find and remove inactive sessions
	for ip, session := range q.activeUsers {
		if now.Sub(session.LastActivity) > q.sessionTimeout {
			delete(q.activeUsers, ip)
		}
	}
}

// ServeHTTP implements the http.Handler interface
func (q *UserQueue) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	// Run cleanup of inactive sessions
	q.cleanupInactiveSessions()

	clientIP := q.getClientIP(req)
	now := time.Now()

	q.mutex.Lock()

	// Check if this user is already in our active users
	session, exists := q.activeUsers[clientIP]

	if exists {
		// User already has a session, update last activity time
		session.LastActivity = now
		q.mutex.Unlock()
		q.next.ServeHTTP(rw, req)
		return
	}

	// User doesn't have a session, check if we can add them
	currentUsers := len(q.activeUsers)

	// If we haven't reached the max users limit, add this user
	if currentUsers < q.maxUsers {
		q.activeUsers[clientIP] = &UserSession{
			IP:           clientIP,
			LastActivity: now,
		}
		q.mutex.Unlock()

		// Add headers to inform about queue status
		rw.Header().Set("X-Queue-Position", "active")
		rw.Header().Set("X-Queue-Max-Users", string(q.maxUsers))
		rw.Header().Set("X-Queue-Current-Users", string(currentUsers+1))

		q.next.ServeHTTP(rw, req)
		return
	}

	// We're at max capacity
	q.mutex.Unlock()

	// Add headers to inform about queue status
	rw.Header().Set("X-Queue-Position", "waiting")
	rw.Header().Set("X-Queue-Max-Users", string(q.maxUsers))
	rw.Header().Set("X-Queue-Current-Users", string(currentUsers))

	if q.queueFullPagePath != "" {
		// Redirect to the queue full page
		http.Redirect(rw, req, q.queueFullPagePath, http.StatusTemporaryRedirect)
		return
	}

	// Return 503 Service Unavailable
	rw.Header().Set("Retry-After", "60")
	rw.WriteHeader(http.StatusServiceUnavailable)
	rw.Write([]byte(q.queueFullMessage))
}
