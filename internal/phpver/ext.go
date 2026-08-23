package phpver

import "strings"

// Ext is one extension of an installed PHP version.
type Ext struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Zend    bool   `json:"zend"`
}

func normalizeExtName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "php_")
	return strings.TrimSuffix(name, ".dll")
}
