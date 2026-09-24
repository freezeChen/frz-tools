package httpapi

import (
	"net/http"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/application"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// validate / prepare / health 是同步用例（§9）：它们立即返回结果，不进 Operation，
// 因为「这份规格能不能执行」「现在有没有就绪」都是查询，排队执行没有任何意义。
func (s *Server) handleValidateRuntime(w http.ResponseWriter, r *http.Request) {
	if s.deps.Runtimes == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "runtime service is not configured"))
		return
	}

	app, _, err := s.deps.Runtimes.Validate(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.RuntimeValidateResponse{
		APIVersion:  v1.APIVersion,
		Application: app.ID,
		Valid:       true,
	})
}

func (s *Server) handlePrepareRuntime(w http.ResponseWriter, r *http.Request) {
	if s.deps.Runtimes == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "runtime service is not configured"))
		return
	}

	app, _, decision, decided, err := s.deps.Runtimes.Prepare(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	response := v1.RuntimePrepareResponse{
		APIVersion:  v1.APIVersion,
		Application: app.ID,
		Prepared:    true,
	}
	if decided {
		response.Decision = runtimeDecisionDTO(decision)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleStartRuntime(w http.ResponseWriter, r *http.Request) {
	s.createRuntimeOperation(w, r, v1.KindRuntimeStart)
}

func (s *Server) handleStopRuntime(w http.ResponseWriter, r *http.Request) {
	s.createRuntimeOperation(w, r, v1.KindRuntimeStop)
}

// createRuntimeOperation 把 start/stop 落到 Operation 上：与 POST /api/v1/operations
// 走的是同一个 Service.Create，因此锁、幂等键、请求摘要、审计与 worker 唤醒都不存在
// 第二条实现；runtime.* 于是天然支持 operation get/logs/cancel/retry。
func (s *Server) createRuntimeOperation(w http.ResponseWriter, r *http.Request, kind string) {
	if s.deps.Service == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "operation service is not configured"))
		return
	}

	var req v1.RuntimeActionRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}

	op, created, err := s.deps.Service.Create(r.Context(), v1.CreateOperationRequest{
		Kind:           kind,
		Resource:       r.PathValue("id"),
		DryRun:         req.DryRun,
		IdempotencyKey: req.IdempotencyKey,
		CreatedBy:      req.CreatedBy,
		Retry:          req.Retry,
	})
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

func (s *Server) handleRuntimeHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Runtimes == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "runtime service is not configured"))
		return
	}

	app, _, health, err := s.deps.Runtimes.Health(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.RuntimeHealthResponse{
		APIVersion:  v1.APIVersion,
		Application: app.ID,
		Ready:       health.Ready,
		CheckedAt:   health.CheckedAt,
		Detail:      health.Detail,
	})
}

func runtimeDecisionDTO(decision application.RuntimeDecision) *v1.RuntimeDecision {
	return &v1.RuntimeDecision{
		UnitName:       decision.UnitName,
		UnitPath:       decision.UnitPath,
		Tier:           decision.Tier,
		SystemdVersion: decision.SystemdVersion,
		Degradations:   decision.Degradations,
		DecidedAt:      decision.DecidedAt,
	}
}
