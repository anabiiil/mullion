package ui

// The PHP page's OPcache card.

import (
	"encoding/json"
	"net/http"

	"pm/internal/app"
	"pm/internal/phpver"
)

func registerOpcache(api apiRegistrar) {
	api("/api/php/opcache", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		return a.OpcacheInfo(in.Version)
	})
	api("/api/php/opcache/set", func(a *app.App, r *http.Request) (any, error) {
		var in struct {
			Version string
			Change  phpver.OpcacheChange
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if err := a.SetOpcache(in.Version, in.Change); err != nil {
			return nil, err
		}
		forgetSQLServerExt()
		return a.OpcacheInfo(in.Version)
	})
	api("/api/php/opcache/reset", func(a *app.App, r *http.Request) (any, error) {
		var in struct{ Version string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		if err := a.ResetOpcache(in.Version); err != nil {
			return nil, err
		}
		return a.OpcacheInfo(in.Version)
	})
}
