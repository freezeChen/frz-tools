package httpapi

import (
	"context"
	"crypto/tls"
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
	"github.com/freezeChen/frz-tools/internal/pki"
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
	Backups   *application.BackupService
	Deploys   *application.DeployService
	Slots     *application.SlotService
	Store     *sqlite.Store
	Workers   int
	Logger    *slog.Logger
	// RemoteIdentities 是远程身份白名单（迭代 5a）。为 nil 表示本进程不听远程——
	// 那时任何 mTLS 请求都是装配错误，一律拒绝。
	RemoteIdentities RemoteIdentityLookup
	// Version 是构建信息（模块版本 + VCS 修订），由 /identity 回答。
	// 为空串是诚实的事实（没有注入），不是 "unknown"。
	Version string
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

// routes 是**唯一**的路由注册处：路径、访问档位与处理器写在同一行。
//
// 之所以把三条信息捆在一起，是因为它们必须一起改。把档位表单独列一份的话，
// 新加一个写端点而忘了登记，得到的是「静默放行」——一个不会报错的越权。
// Handler() 对档位做了合法性检查，漏写就是启动即崩。
func (s *Server) routes() []route {
	return []route{
		{"GET /api/v1/identity", accessRead, s.handleIdentity},
		{"GET /api/v1/health", accessRead, s.handleHealth},
		{"POST /api/v1/operations", accessWrite, s.handleCreateOperation},
		{"GET /api/v1/operations/{id}", accessRead, s.handleGetOperation},
		{"POST /api/v1/operations/{id}/cancel", accessWrite, s.handleCancelOperation},
		{"POST /api/v1/operations/{id}/retry", accessWrite, s.handleRetryOperation},
		{"GET /api/v1/operations/{id}/logs", accessRead, s.handleOperationLogs},

		{"POST /api/v1/artifacts", accessWrite, s.handleUploadArtifact},
		{"GET /api/v1/artifacts", accessRead, s.handleListArtifacts},
		{"POST /api/v1/artifacts/gc", accessWrite, s.handleCollectArtifacts},
		{"GET /api/v1/artifacts/{id}", accessRead, s.handleGetArtifact},
		{"GET /api/v1/artifacts/{id}/content", accessRead, s.handleDownloadArtifact},
		// verify 只重算一遍摘要与记录比对，不改任何状态，因此是只读。
		{"POST /api/v1/artifacts/{id}/verify", accessRead, s.handleVerifyArtifact},
		{"DELETE /api/v1/artifacts/{id}", accessWrite, s.handleDeleteArtifact},

		{"POST /api/v1/applications", accessWrite, s.handleCreateApplication},
		{"GET /api/v1/applications", accessRead, s.handleListApplications},
		{"GET /api/v1/applications/{id}", accessRead, s.handleGetApplication},
		{"GET /api/v1/applications/{id}/releases", accessRead, s.handleListApplicationReleases},
		{"PUT /api/v1/applications/{id}/spec", accessWrite, s.handlePutApplicationSpec},
		{"GET /api/v1/applications/{id}/spec", accessRead, s.handleGetApplicationSpec},
		// validate 是同步用例且无副作用（成败都在内存里判），因此是只读；
		// prepare 会真的往盘上写 unit 与环境文件，是写。
		{"POST /api/v1/applications/{id}/runtime/validate", accessRead, s.handleValidateRuntime},
		{"POST /api/v1/applications/{id}/runtime/prepare", accessWrite, s.handlePrepareRuntime},
		{"POST /api/v1/applications/{id}/runtime/start", accessWrite, s.handleStartRuntime},
		{"POST /api/v1/applications/{id}/runtime/stop", accessWrite, s.handleStopRuntime},
		{"GET /api/v1/applications/{id}/runtime/health", accessRead, s.handleRuntimeHealth},
		// 部署与回滚（迭代 3）：都走 Service.Create，与 runtime.* / backup.* 同一条创建路径，
		// 因此 operation get/logs/cancel/retry 对它们同样适用。
		// 槽位的运营视图与切换时间线（迭代 4c）。`history` 是子路径而不是查询参数：
		// 它返回的是另一种东西（事件流），不是同一个列表的另一种筛选。
		{"GET /api/v1/applications/{id}/slots", accessRead, s.handleListSlots},
		{"GET /api/v1/applications/{id}/slots/history", accessRead, s.handleSlotHistory},

		{"POST /api/v1/applications/{id}/deploy", accessWrite, s.handleDeployApplication},
		{"POST /api/v1/applications/{id}/rollback", accessWrite, s.handleRollbackApplication},
		{"POST /api/v1/releases", accessWrite, s.handleCreateRelease},
		{"GET /api/v1/releases/{id}", accessRead, s.handleGetRelease},

		{"GET /api/v1/hosts", accessRead, s.handleListHosts},
		{"POST /api/v1/hosts", accessWrite, s.handleCreateHost},
		{"GET /api/v1/hosts/{id}", accessRead, s.handleGetHost},
		{"GET /api/v1/environments", accessRead, s.handleListEnvironments},
		{"POST /api/v1/environments", accessWrite, s.handleCreateEnvironment},
		{"GET /api/v1/environments/{id}", accessRead, s.handleGetEnvironment},

		{"PUT /api/v1/backup-policies/{name}", accessWrite, s.handlePutBackupPolicy},
		{"GET /api/v1/backup-policies/{name}", accessRead, s.handleGetBackupPolicy},
		{"GET /api/v1/backup-policies", accessRead, s.handleListBackupPolicies},
		{"POST /api/v1/backups", accessWrite, s.handleRunBackup},
		{"GET /api/v1/backups", accessRead, s.handleListBackups},
		{"GET /api/v1/backups/{id}", accessRead, s.handleGetBackup},
		{"POST /api/v1/backups/{id}/verify", accessWrite, s.handleVerifyBackup},
		{"POST /api/v1/backups/{id}/restore", accessWrite, s.handleRestoreBackup},
		// prune 是**同步**端点（与制品 GC 一致）：它删的是备份内容，但结果必须当场看得见。
		{"POST /api/v1/backups/prune", accessWrite, s.handlePruneBackups},

		{"POST /api/v1/schedules", accessWrite, s.handleCreateSchedule},
		{"GET /api/v1/schedules", accessRead, s.handleListSchedules},
		{"GET /api/v1/schedules/{id}", accessRead, s.handleGetSchedule},
		{"DELETE /api/v1/schedules/{id}", accessWrite, s.handleDeleteSchedule},
		{"POST /api/v1/schedules/{id}/enable", accessWrite, s.handleEnableSchedule},
		{"POST /api/v1/schedules/{id}/disable", accessWrite, s.handleDisableSchedule},
		{"GET /api/v1/schedules/{id}/runs", accessRead, s.handleScheduleRuns},

		// 未匹配的请求落在这里。它是只读的：它做的事只有一句「没有这条路由」。
		{"/", accessRead, s.handleNotFound},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	access := make(map[string]domain.AccessScope)
	for _, rt := range s.routes() {
		if !rt.access.Valid() {
			// 编译不过做不到（档位是个字符串常量），因此把它做成**启动即崩**：
			// 一个没说清访问档位的路由绝不允许在「默认放行」的状态下跑起来。
			panic("httpapi: 路由 " + rt.pattern + " 没有声明 accessRead / accessWrite")
		}
		mux.HandleFunc(rt.pattern, rt.handler)
		access[rt.pattern] = rt.access
	}

	logger := s.logger()
	return recoverPanic(logger, logRequests(logger, s.authenticate(mux, access)))
}

// Serve 在 ln 上运行 HTTP 服务，直到 ctx 被取消，然后在 grace 内优雅关闭。
//
// 远程监听（迭代 5a）不需要第二个 Serve：把 TCP 监听器套进 tls.NewListener 再交给
// 这里即可——http.Server.Serve 会在 *tls.Conn 上做握手并填好 r.TLS，而 r.TLS 正是
// 认证中间件区分「走的是哪条路」的唯一判据。
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

// ListenTLS 起一个 mTLS 监听器（迭代 5a）。
//
// 与 ListenUnix 不同，它不做「顶掉正在运行的守护进程」那套：TCP 端口被占用时
// 直接失败——占用它的可能是另一个 opsd，也可能是完全不相干的服务，猜错了就是
// 把一个正在服务的进程踢下线。
//
// 返回的是一个 **tls.Listener**：http.Server.Serve 会在每个 *tls.Conn 上完成握手并
// 填好 r.TLS，而 r.TLS 正是认证中间件区分本机与远程的唯一判据。
func ListenTLS(addr, certFile, keyFile, clientCAFile string) (net.Listener, error) {
	tlsConfig, err := pki.ServerTLSConfig(certFile, keyFile, clientCAFile)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, domain.NewError(v1.CodeConfigInvalid, "无法监听 %s: %v", addr, err)
	}
	return tls.NewListener(listener, tlsConfig), nil
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
