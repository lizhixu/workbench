package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Middleware returns a gin middleware that validates the JWT bearer token
// and stores the claims in the context.
//
// The token is read from the Authorization header ("Bearer <token>") first,
// and falls back to the "token" query parameter. The query-param fallback
// supports browser-based media/asset fetches that cannot set headers (e.g.
// asciinema-player loading a recording via { url }).
//
// Routes registered before this middleware is applied are PUBLIC (e.g.
// /auth/login, /host/install_script, /agent/binary). Routes after it require
// authentication. Use RequireRole for finer-grained role checks.
func Middleware(store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := ""
		if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tokenStr = strings.TrimPrefix(h, "Bearer ")
		} else if q := c.Query("token"); q != "" {
			tokenStr = q
		}
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid Authorization header"})
			return
		}
		claims, err := store.Validate(tokenStr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		c.Set("username", claims.Username)
		c.Set("role", string(claims.Role))
		c.Next()
	}
}

// RequireRole returns middleware that aborts with 403 if the caller's role
// is not in the allowed set.
func RequireRole(roles ...Role) gin.HandlerFunc {
	allowed := map[Role]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		r := Role(c.GetString("role"))
		if !allowed[r] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			return
		}
		c.Next()
	}
}

// Username retrieves the authenticated user's name from the context.
func Username(c *gin.Context) string {
	return c.GetString("username")
}

// RoleOf retrieves the authenticated user's role from the context.
func RoleOf(c *gin.Context) Role {
	return Role(c.GetString("role"))
}