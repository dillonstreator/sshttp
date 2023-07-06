package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dillonstreator/sshttp/pkg/env"
	"github.com/gliderlabs/ssh"
	"github.com/justinas/alice"
	"github.com/rs/zerolog"
)

type config struct {
	LogLevel                    zerolog.Level
	SSHPort                     int
	HTTPPort                    int
	IDLength                    int
	SSHConnectionTimeoutSeconds int
	ShutdownTimeoutSeconds      int
	HTTPHealthCheckEndpoint     string
	BaseURL                     string
}

var cfg = config{
	LogLevel:                    env.Get("LOG_LEVEL", zerolog.InfoLevel, env.WithParser(zerolog.ParseLevel)),
	SSHPort:                     env.Get("SSH_PORT", 2222),
	HTTPPort:                    env.Get("HTTP_PORT", 8181),
	IDLength:                    env.Get("ID_LENGTH", 24),
	SSHConnectionTimeoutSeconds: env.Get("SSH_CONNECTION_TIMEOUT_SECONDS", 60*15),
	ShutdownTimeoutSeconds:      env.Get("SHUTDOWN_TIMEOUT_SECONDS", 15),
	HTTPHealthCheckEndpoint:     env.Get("HTTP_HEALTH_CHECK_ENDPOINT", "/health"),
	BaseURL:                     env.Get("BASE_URL", "http://localhost"),
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	zerolog.SetGlobalLevel(cfg.LogLevel)
	zerolog.TimeFieldFormat = time.RFC3339Nano
	logger := zerolog.New(os.Stdout)

	sshSrv := newSSHServer(ctx, logger)
	go func() {
		logger.Info().Msgf("listening for ssh at %s%s", cfg.BaseURL, sshSrv.Addr)
		if err := sshSrv.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			logger.Fatal().Err(err).Send()
		}
	}()

	httpSrv := newHTTPServer(logger)
	go func() {
		logger.Info().Msgf("listening for http at %s%s", cfg.BaseURL, httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal().Err(err).Send()
		}
	}()

	wait := make(chan os.Signal, 1)
	signal.Notify(wait, syscall.SIGINT, syscall.SIGTERM)

	logger.Info().Msg("waiting for shutdown signal")

	<-wait

	logger.Info().Msg("shutdown signal received")

	cancel()

	shutdownCtx, cancelShutdownCtx := context.WithTimeout(context.Background(), time.Second*time.Duration(cfg.ShutdownTimeoutSeconds))
	defer cancelShutdownCtx()

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := sshSrv.Shutdown(shutdownCtx); err != nil {
			logger.Fatal().Err(err).Send()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Fatal().Err(err).Send()
		}
	}()

	logger.Info().Msg("waiting for servers to shutdown")

	wg.Wait()

	logger.Info().Msg("goodbye")
}

type tunnel struct {
	w    io.Writer
	done chan error
}

var tunnels = make(map[string]chan *tunnel)

func newSSHServer(ctx context.Context, logger zerolog.Logger) *ssh.Server {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	srv := &ssh.Server{
		Addr: fmt.Sprintf(":%d", cfg.SSHPort),
		Handler: func(s ssh.Session) {
			defer s.Close()

			logger := logger.With().Str("user", s.User()).Str("remote", s.RemoteAddr().String()).Logger()

			b := make([]byte, hex.DecodedLen(cfg.IDLength))
			_, err := rnd.Read(b)
			if err != nil {
				logger.Error().Err(err).Msg("rand reading id")
				return
			}

			id := hex.EncodeToString(b)

			logger = logger.With().Str("id", id).Logger()

			_, err = s.Write([]byte(fmt.Sprintf("%s:%d?id=%s\n", cfg.BaseURL, cfg.HTTPPort, id)))
			if err != nil {
				logger.Error().Err(err).Msg("writing id")
				return
			}

			tunnels[id] = make(chan *tunnel)
			defer func() {
				delete(tunnels, id)
			}()

			timeoutDuration := time.Second * time.Duration(cfg.SSHConnectionTimeoutSeconds)
			timer := time.NewTimer(timeoutDuration)
			defer timer.Stop()

			select {
			case <-timer.C:
				logger.Info().Msgf("%s timeout reached", timeoutDuration.String())
				s.Write([]byte("timeout reached\n"))
				return

			case <-ctx.Done():
				logger.Info().Msg("parent context cancelled while waiting for tunnel")
				s.Write([]byte("server shutdown\n"))
				return

			case <-s.Context().Done():
				logger.Info().Err(s.Context().Err()).Msg("client connection context cancelled while waiting for tunnel")
				return

			case tunnel := <-tunnels[id]:
				n, err := io.Copy(tunnel.w, s)
				if err != nil {
					tunnel.done <- err
					logger.Error().Err(err).Msg("copying to tunnel")
					return
				}

				logger.Info().Msgf("wrote %d bytes", n)
				s.Write([]byte(fmt.Sprintf("wrote %d bytes\n", n)))
				tunnel.done <- nil
			}
		},
	}

	return srv
}

func newHTTPServer(logger zerolog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")

		logger := logger.With().Str("id", id).Logger()

		tCh, ok := tunnels[id]
		if !ok {
			logger.Warn().Msg("tunnel not found")
			w.WriteHeader(http.StatusNotFound)
			return
		}

		done := make(chan error)
		defer func() { close(done) }()
		tCh <- &tunnel{
			w:    w,
			done: done,
		}

		err := <-done
		if err != nil {
			logger.Error().Err(err).Send()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}))
	mux.Handle(cfg.HTTPHealthCheckEndpoint, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	handler := alice.New(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				logger := logger.With().Str("remote", r.RemoteAddr).Logger()
				r = r.WithContext(logger.WithContext(r.Context()))

				ww := &wrappedWriter{ResponseWriter: w}
				wbody := &wrappedBody{ReadCloser: r.Body}
				r.Body = wbody

				next.ServeHTTP(ww, r)

				logger.Info().
					Str("url", r.URL.String()).
					Str("method", r.Method).
					Int("code", ww.code).
					Int64("bytesWritten", ww.written).
					Int64("bytesRead", wbody.read).
					Send()
			})
		},
	).Then(mux)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler: handler,
	}

	return srv
}

type wrappedWriter struct {
	http.ResponseWriter
	code        int
	written     int64
	wroteHeader bool
}

func (w *wrappedWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *wrappedWriter) Write(buf []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	n, err := w.ResponseWriter.Write(buf)
	w.written += int64(n)
	return n, err
}

type wrappedBody struct {
	io.ReadCloser
	read int64
}

func (w *wrappedBody) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.read += int64(n)
	return n, err
}
