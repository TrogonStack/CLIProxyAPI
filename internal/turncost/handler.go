package turncost

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	maxRequestBytes = 1 << 20
	maxQueryTurns   = 500
)

type queryRequest struct {
	TurnIDs []string `json:"turn_ids"`
}

type queryResponse struct {
	Turns []TurnCost `json:"turns"`
}

// Handler serves POST /v1/analytics/codex/turn-costs. When the feature is disabled it
// answers 404, which makes Codex stop polling.
func (s *Service) Handler(c *gin.Context) {
	if !s.Enabled() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	var req queryRequest
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ids := req.TurnIDs
	if len(ids) > maxQueryTurns {
		ids = ids[:maxQueryTurns]
	}
	c.JSON(http.StatusOK, queryResponse{Turns: s.Query(c.Request.Context(), ids)})
}
