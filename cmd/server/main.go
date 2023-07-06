package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/justinas/alice"
	"github.com/rs/zerolog"
)

func main() {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	logger := zerolog.New(os.Stdout)

	logLevel := zerolog.InfoLevel
	if l, ok := os.LookupEnv("LOG_LEVEL"); ok {
		var err error
		logLevel, err = zerolog.ParseLevel(l)
		if err != nil {
			logger.Fatal().Err(err).Send()
		}
	}
	zerolog.SetGlobalLevel(logLevel)

	sshSrv := newSSHServer(logger)
	go func() {
		logger.Info().Msgf("listening for ssh at %s", sshSrv.Addr)
		if err := sshSrv.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			logger.Fatal().Err(err).Send()
		}
	}()

	httpSrv := newHTTPServer(logger)
	go func() {
		logger.Info().Msgf("listening for http at %s", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal().Err(err).Send()
		}
	}()

	wait := make(chan os.Signal, 1)
	signal.Notify(wait, syscall.SIGINT, syscall.SIGTERM)

	logger.Info().Msgf("waiting for shutdown signal")

	<-wait

	logger.Info().Msgf("shutdown signal received")

	shutdownCtx, cancelShutdownCtx := context.WithTimeout(context.Background(), time.Second*15)
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

func newSSHServer(logger zerolog.Logger) *ssh.Server {
	port := "2222"
	if p, ok := os.LookupEnv("SSH_PORT"); ok {
		port = p
	}

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	idLen := 32
	if l, ok := os.LookupEnv("ID_LENGTH"); ok {
		var err error
		idLen, err = strconv.Atoi(l)
		if err != nil {
			logger.Fatal().Err(err).Msgf("converting ID_LENGTH env to int")
		}
	}

	timeoutSeconds := 60 * 15
	if l, ok := os.LookupEnv("SSH_CONNECTION_TIMEOUT_SECONDS"); ok {
		var err error
		idLen, err = strconv.Atoi(l)
		if err != nil {
			logger.Fatal().Err(err).Msgf("converting SSH_CONNECTION_TIMEOUT_SECONDS env to int")
		}
	}

	srv := &ssh.Server{
		Addr: fmt.Sprintf(":%s", port),
		Handler: func(s ssh.Session) {
			defer s.Close()

			logger := logger.With().Str("remote", s.RemoteAddr().String()).Logger()

			b := make([]byte, base64.RawURLEncoding.DecodedLen(idLen))
			_, err := rnd.Read(b)
			if err != nil {
				logger.Error().Err(err).Msg("rand reading id")
				return
			}

			id := base64.RawURLEncoding.EncodeToString(b)

			logger = logger.With().Str("id", id).Logger()

			_, err = s.Write([]byte(fmt.Sprintf("%s\n", id)))
			if err != nil {
				logger.Error().Err(err).Msg("writing id")
				return
			}

			tunnels[id] = make(chan *tunnel)
			defer func() {
				delete(tunnels, id)
			}()

			timer := time.NewTimer(time.Second * time.Duration(timeoutSeconds))
			defer timer.Stop()

			select {
			case <-timer.C:
				logger.Info().Msgf("timeout reached")
				s.Write([]byte("timeout reached\n"))
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
	port := "8181"
	if p, ok := os.LookupEnv("HTTP_PORT"); ok {
		port = p
	}

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
	mux.Handle("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Addr:    fmt.Sprintf(":%s", port),
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
