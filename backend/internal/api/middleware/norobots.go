package middleware

import "net/http"

// NoRobots sets the X-Robots-Tag header on every response to
// instruct crawlers not to index or follow links on the page.
// This should be applied to the entire registry API since the
// knowledge files it serves (facts, concepts, decompositions)
// are not meant for public search engine consumption.
//
// Usage:
//
//	r.Use(middleware.NoRobots)
func NoRobots(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}
