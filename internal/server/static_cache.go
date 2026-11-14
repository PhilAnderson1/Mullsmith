package server

import "net/http"

// assetCacheWriter grants freshness only to successful static responses.
// Middleware errors never reach this writer and retain the no-store default.
type assetCacheWriter struct {
	http.ResponseWriter
	manifest  bool
	immutable bool
	committed bool
}

func (w *assetCacheWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	if status >= 200 {
		w.committed = true
		policy := "no-store"
		if status < 300 || status == http.StatusNotModified {
			policy = "public, max-age=3600"
			if w.immutable {
				policy = "public, max-age=2592000, immutable"
			}
			if w.manifest {
				policy = "no-cache"
			}
		}
		w.Header().Set("Cache-Control", policy)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *assetCacheWriter) Write(body []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *assetCacheWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
