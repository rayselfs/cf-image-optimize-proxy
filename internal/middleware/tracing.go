package middleware

import (
	"net/http"

	"github.com/rayselfs/cf-image-optimize-proxy/internal/tracing"
)

func Tracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spanName := r.Method + " " + r.URL.Path
		if r.Pattern != "" {
			spanName = r.Method + " " + r.Pattern
		}

		ctx, span := tracing.Tracer().Start(r.Context(), spanName)
		defer span.End()

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
