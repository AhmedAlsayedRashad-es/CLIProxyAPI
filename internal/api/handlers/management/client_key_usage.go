package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clientusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
)

// GetClientKeyUsage returns request and token totals per client API key,
// the keys listed under access.api-keys that clients send to the proxy.
func (h *Handler) GetClientKeyUsage(c *gin.Context) {
	snap := clientusage.Default().Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"enabled": redisqueue.UsageStatisticsEnabled(),
		"since":   snap.Since,
		"keys":    snap.Keys,
	})
}

type clientKeyUsageResetRequest struct {
	Key string `json:"key"`
}

// DeleteClientKeyUsage resets the totals of one client API key, given in the
// JSON body as {"key": "..."}, or of every key when the body is empty.
// The key goes in the body, not the URL, so it does not appear in access logs.
func (h *Handler) DeleteClientKeyUsage(c *gin.Context) {
	var req clientKeyUsageResetRequest
	if c.Request.Body != nil {
		if errBind := c.ShouldBindJSON(&req); errBind != nil && !errors.Is(errBind, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}
	key := strings.TrimSpace(req.Key)
	removed := clientusage.Default().Reset(key)
	if key != "" && !removed {
		c.JSON(http.StatusNotFound, gin.H{"error": "key has no usage"})
		return
	}
	if errFlush := clientusage.Default().Flush(); errFlush != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errFlush.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
