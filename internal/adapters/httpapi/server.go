package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/sqlite"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const maxRequestBytes = 1 << 20

// Dependencies 把 API 需要的端口显式列出，避免处理器直接依赖具体实现。
type Dependencies struct {
	Service   *application.Service
	Artifacts *application.ArtifactService
	Catalogs  *application.CatalogService
	Specs     *application.SpecService
	Hosts     *application.HostService
	Runtimes  *application.RuntimeService
	Schedules *application.ScheduleService
	Store     *sqlite.Store
	Workers   int
	Logger    *slog.Logger
}

type Server struct {
	deps      Dependencies
	startedAt time.Time
}

func NewServer(deps Dependencies) *Server {
	return &Server{deps: deps, startedAt: time.Now().UTC()}
}

func (s *Server) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s.deps.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/operations", s.handleCreateOperation)
	mux.HandleFunc("GET /api/v1/operations/{id}", s.handleGetOperation)
	mux.HandleFunc("POST /api/v1/operations/{id}/cancel", s.handleCancelOperation)
	mux.HandleFunc("POST /api/v1/operations/{id}/retry", s.handleRetryOperation)
	mux.HandleFunc("GET /api/v1/operations/{id}/logs", s.handleOperationLogs)

	mux.HandleFunc("POST /api/v1/artifacts", s.handleUploadArtifact)
	mux.HandleFunc("GET /api/v1/artifacts", s.handleListArtifacts)
	mux.HandleFunc("POST /api/v1/artifacts/gc", s.handleCollectArtifacts)
	mux.HandleFunc("GET /api/v1/artifacts/{id}", s.handleGetArtifact)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/content", s.handleDownloadArtifact)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/verify", s.handleVerifyArtifact)
	mux.HandleFunc("DELETE /api/v1/artifacts/{id}", s.handleDeleteArtifact)

	mux.HandleFunc("POST /api/v1/applications", s.handleCreateApplication)
	mux.HandleFunc("GET /api/v1/applications", s.handleListApplications)
	mux.HandleFunc("GET /api/v1/applications/{id}", s.handleGetApplication)
	mux.HandleFunc("GET /api/v1/applications/{id}/releases", s.handleListApplicationReleases)
	mux.HandleFunc("PUT /api/v1/applications/{id}/spec", s.handlePutApplicationSpec)
	mux.HandleFunc("GET /api/v1/applications/{id}/spec", s.handleGetApplicationSpec)
	mux.HandleFunc("POST /api/v1/applications/{id}/runtime/validate", s.handleValidateRuntime)
	mux.HandleFunc("POST /api/v1/applications/{id}/runtime/prepare", s.handlePrepareRuntime)
	mux.HandleFunc("POST /api/v1/applications/{id}/runtime/start", s.handleStartRuntime)
	mux.HandleFunc("POST /api/v1/applications/{id}/runtime/stop", s.handleStopRuntime)
	mux.HandleFunc("GET /api/v1/applications/{id}/runtime/health", s.handleRuntimeHealth)
	mux.HandleFunc("POST /api/v1/releases", s.handleCreateRelease)
	mux.HandleFunc("GET /api/v1/releases/{id}", s.handleGetRelease)

	mux.HandleFunc("GET /api/v1/hosts", s.handleListHosts)
	mux.HandleFunc("POST /api/v1/hosts", s.handleCreateHost)
	mux.HandleFunc("GET /api/v1/hosts/{id}", s.handleGetHost)
	mux.HandleFunc("GET /api/v1/environments", s.handleListEnvironments)
	mux.HandleFunc("POST /api/v1/environments", s.handleCreateEnvironment)
	mux.HandleFunc("GET /api/v1/environments/{id}", s.handleGetEnvironment)

	mux.HandleFunc("POST /api/v1/schedules", s.handleCreateSchedule)
	mux.HandleFunc("GET /api/v1/schedules", s.handleListSchedules)
	mux.HandleFunc("GET /api/v1/schedules/{id}", s.handleGetSchedule)
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.handleDeleteSchedule)
	mux.HandleFunc("POST /api/v1/schedules/{id}/enable", s.handleEnableSchedule)
	mux.HandleFunc("POST /api/v1/schedules/{id}/disable", s.handleDisableSchedule)
	mux.HandleFunc("GET /api/v1/schedules/{id}/runs", s.handleScheduleRuns)

	mux.HandleFunc("/", s.handleNotFound)

	logger := s.logger()
	return recoverPanic(logger, logRequests(logger, mux))
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
			s.logger().Error("graceful shutdown failed", "error", err)
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
	if err := s.deps.Store.Ping(r.Context()); err != nil {
		s.logger().Error("health check failed", "error", err)
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "database unreachable"))
		return
	}
	writeJSON(w, http.StatusOK, v1.HealthResponse{
		APIVersion: v1.APIVersion,
		Daemon:     "ok",
		Database:   "ok",
		Workers:    s.deps.Workers,
		UptimeMS:   time.Since(s.startedAt).Milliseconds(),
	})
}

func (s *Server) handleCreateOperation(w http.ResponseWriter, r *http.Request) {
	var req v1.CreateOperationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}

	op, created, err := s.deps.Service.Create(r.Context(), req)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.deps.Service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleCancelOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.deps.Service.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleRetryOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.deps.Service.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusAccepted, v1.OperationResponse{APIVersion: v1.APIVersion, Operation: operationDTO(op)})
}

func (s *Server) handleOperationLogs(w http.ResponseWriter, r *http.Request) {
	cursor, err := intQuery(r, "cursor", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	limit, err := intQuery(r, "limit", 0)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	entries, err := s.deps.Service.Logs(r.Context(), r.PathValue("id"), int64(cursor), limit)
	if err != nil {
		writeError(w, s.logger(), err)
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
	writeError(w, s.logger(), domain.NewError(v1.CodeOperationNotFound, "no route matches %s %s", r.Method, r.URL.Path))
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
	dto := v1.Operation{
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

	// attempt / maxAttempts 始终有值：未声明重试的操作是 1 / 1，让调用方不必区分
	// 「字段缺失」与「就执行这一次」。
	dto.Attempt = op.Attempt
	if dto.Attempt < 1 {
		dto.Attempt = 1
	}
	dto.MaxAttempts = op.RetryPolicy.MaxAttempts
	if dto.MaxAttempts < 1 {
		dto.MaxAttempts = 1
	}
	// nextAttemptAt 只在「还在排队」时有意义：终态的操作不会再有下一次，
	// 给它一个时间只会让人以为还有希望。
	if op.Status == domain.StatusPending && op.NotBefore != nil {
		dto.NextAttemptAt = op.NotBefore
	}
	dto.RetryExhausted = op.RetryExhausted()
	return dto
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
