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

// AllowedAuthIDsContextKey is the gin context key under which the credential IDs a
// restricted principal may use ([]string) are published for code that has no
// credential manager. It is derived from the restriction, never configured.
const AllowedAuthIDsContextKey = "accessAllowedAuthIDs"

// AllowedAuthIDsFromGin reports whether the authenticated request is restricted and, if
// so, the IDs of the credentials it may use. A restricted request whose IDs were never
// published gets an empty list, so a consumer that checks the list fails closed.
func AllowedAuthIDsFromGin(c *gin.Context) (ids []string, restricted bool) {
	if len(AllowedAuthsFromGin(c)) == 0 {
		return nil, false
	}
	value, exists := c.Get(AllowedAuthIDsContextKey)
	if !exists {
		return []string{}, true
	}
	published, _ := value.([]string)
	return append([]string{}, published...), true
}
