package staticserver

import (
	"log/slog"
	"net/http"
	"time"
)

func Server(address, directory string, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           http.FileServer(http.Dir(directory)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}
