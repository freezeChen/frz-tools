package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/adapters/sqlite"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"
)

const maxRequestBytes = 1 << 20

type Server struct {
	service   *application.Service
	store     *sqlite.Store
	workers   int
	startedAt time.Time
	logger    *slog.Logger
}

func NewServer(service *application.Service, store *sqlite.Store, workers int, logger *slog.Logger) *Server {
	return &Server{
		service:   service,
		store:     store,
		workers:   workers,
		startedAt: time.Now().UTC(),
		logger:    logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/operations", s.handleCreateOperation)
	mux.HandleFunc("GET /api/v1/operations/{id}", s.handleGetOperation)
	mux.HandleFunc("POST /api/v1/operations/{id}/cancel", s.handleCancelOperation)
	mux.HandleFunc("POST /api/v1/operations/{id}/retry", s.handleRetryOperation)
	mux.HandleFunc("GET /api/v1/operations/{id}/logs", s.handleOperationLogs)
	mux.HandleFunc("/", s.handleNotFound)

	return recoverPanic(s.logger, logRequests(s.logger, mux))
}

// Serve 在 ln 上运行 HTTP 服务，直到 ctx 被取消，然后在 grace 内优雅关闭。
func (s *Server) Serve(ctx context.Context, ln net.Listener, grace time.Duration) error {
	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	err := httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

// ListenUnix 为 unix socket 监听器准备路径：拒绝顶掉正在运行的守护进程，
// 并清理崩溃后遗留的失效 socket。
func ListenUnix(path string, mode os.FileMode) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err == nil {
		if err := os.Chmod(path, mode); err != nil {
			ln.Close()
			return nil, err
		}
		return ln, nil
	}

	if !isAddrInUse(err) {
		return nil, err
	}
	if conn, dialErr := net.DialTimeout("unix", path, 500*time.Millisecond); dialErr == nil {
		conn.Close()
		return nil, errors.New("another opsd instance is already listening on " + path)
	}
	if removeErr := os.Remove(path); removeErr != nil {
		return nil, err
	}
	ln, err = net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		s.logger.Error("health check failed", "error", err)
		writeError(w, s.logger, domain.NewError(v1.CodeInternal, "database unreachable"))
		return
	}
	writeJSON(w, http.StatusOK, v1.HealthResponse{
		APIVersion: v1.APIVersion,
		Daemon:     "ok",
		Database:   "ok",
		Workers:    s.workers,
		UptimeMS:   time.Since(s.startedAt).Milliseconds(),
	})
}

func (s *Server) handleCreateOperation(w http.ResponseWriter, r *http.Request) {
	var req v1.CreateOperationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger, err)
		return
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}

	op, created, err := s.service.Create(r.Context(), req)
	if err != nil {
		writeError(w, s.logger, err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleCancelOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.service.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger, err)
		return
	}
	writeJSON(w, http.StatusOK, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleRetryOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.service.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleOperationLogs(w http.ResponseWriter, r *http.Request) {
	cursor, err := intQuery(r, "cursor", 0)
	if err != nil {
		writeError(w, s.logger, err)
		return
	}
	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger, err)
		return
	}

	entries, err := s.service.Logs(r.Context(), r.PathValue("id"), int64(cursor), limit)
	if err != nil {
		writeError(w, s.logger, err)
		return
	}

	items := make([]v1.LogEntry, 0, len(entries))
	for _, entry := range entries {
		items = append(items, v1.LogEntry{
			ID:      entry.ID,
			Level:   entry.Level,
			Time:    entry.Time,
			Phase:   entry.Phase,
			Message: entry.Message,
			Fields:  entry.Fields,
		})
	}
	next := int64(cursor)
	if len(items) > 0 {
		next = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, v1.LogsResponse{
		APIVersion: v1.APIVersion,
		Operation:  r.PathValue("id"),
		Items:      items,
		NextCursor: next,
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, s.logger, domain.NewError(v1.CodeOperationNotFound, "no route matches %s %s", r.Method, r.URL.Path))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.NewError(v1.CodeInvalidRequest, "invalid JSON request body: %v", err)
	}
	return nil
}

func intQuery(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, domain.NewError(v1.CodeInvalidRequest, "query parameter %q must be an integer", name)
	}
	return value, nil
}

func operationDTO(op *domain.Operation) v1.Operation {
	return v1.Operation{
		ID:             op.ID,
		Kind:           op.Kind,
		Resource:       op.Resource,
		Status:         string(op.Status),
		Phase:          op.Phase,
		DryRun:         op.DryRun,
		IdempotencyKey: op.IdempotencyKey,
		RequestHash:    op.RequestHash,
		RetryOf:        op.RetryOf,
		ExitCode:       op.ExitCode,
		ErrorCode:      op.ErrorCode,
		ErrorMessage:   op.ErrorMessage,
		Spec:           op.Spec,
		CreatedAt:      op.CreatedAt,
		StartedAt:      op.StartedAt,
		FinishedAt:     op.FinishedAt,
		CreatedBy:      op.CreatedBy,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, logger *slog.Logger, err error) {
	code := domain.CodeOf(err)
	response := v1.ErrorResponse{
		APIVersion: v1.APIVersion,
		Code:       code,
		Message:    domain.MessageOf(err),
	}
	var coded *domain.CodedError
	if errors.As(err, &coded) {
		response.Details = coded.Details
	}
	if code == v1.CodeInternal {
		logger.Error("request failed", "error", err)
	}
	writeJSON(w, v1.HTTPStatus(code), response)
}
