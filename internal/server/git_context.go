package server

import (
	gocontext "context"
	"net/http"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// gitRequestContext bounds all the git a read handler runs by one git timeout
// in total. Without it each git call gets its own timeout, and a handler that
// runs five of them against an unreadable repo takes five times as long.
func gitRequestContext(r *http.Request) (gocontext.Context, gocontext.CancelFunc) {
	return gocontext.WithTimeout(r.Context(), gitexec.Timeout())
}

// gitErrorStatus is 504 when git ran out of time and 500 otherwise.
func gitErrorStatus(err error) int {
	if gitexec.IsTimeout(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}
