package access

import "github.com/gin-gonic/gin"

// AllowedAuthsContextKey is the gin context key under which the credential
// restriction of the authenticated principal ([]string) is stored.
const AllowedAuthsContextKey = "accessAllowedAuths"

// AllowedAuthsFromGin returns the credential restriction recorded for the
// authenticated request, or nil when the principal is unrestricted.
func AllowedAuthsFromGin(c *gin.Context) []string {
	if c == nil {
		return nil
	}
	value, exists := c.Get(AllowedAuthsContextKey)
	if !exists {
		return nil
	}
	refs, _ := value.([]string)
	if len(refs) == 0 {
		return nil
	}
	return append([]string(nil), refs...)
}
