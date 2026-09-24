package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/manifest"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// handleDeployApplication 部署一个版本。
//
// 它在**创建 Operation 之前**就完成能提前做的事（解码 manifest、解析制品与版本号、
// 找到或新建 release 记录、把这一版的规格写下来）：让「制品不存在」「版本号已被别的制品
// 占用」「应用名对不上」这类错误在提交时报出来，而不是先建一条注定失败的 Operation。
func (s *Server) handleDeployApplication(w http.ResponseWriter, r *http.Request) {
	if s.deps.Deploys == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeRuntimeUnsupport, "本部署未启用应用部署能力"))
		return
	}

	var req v1.DeployRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}
	// manifest 必须先过严格解码（KnownFields）与迁移链，理由与 spec put 完全相同：
	// 那是唯一能挡住「未知字段被静默丢弃」的入口，校验规则也只有 domain 那一份。
	spec, err := manifest.ParseReader(strings.NewReader(req.Manifest))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	if err := s.checkUnpackMediaType(r.Context(), spec); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	application := r.PathValue("id")
	target, err := s.deps.Deploys.PrepareDeploy(r.Context(), application, spec, req.CreatedBy)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	if target.AlreadyActive {
		// 要上的版本已经是当前版本：不产生 Operation，也不动任何东西。
		writeJSON(w, http.StatusOK, v1.DeployResponse{
			APIVersion: v1.APIVersion,
			Noop:       true,
			Release:    releaseDTO(target.Release),
		})
		return
	}

	s.createDeployOperation(w, r, v1.KindAppDeploy, application, target.Release.ID,
		req.IdempotencyKey, req.CreatedBy, req.Retry)
}

// handleRollbackApplication 回滚到某个既有版本。
//
// 目标版本在**创建期**就选定并写进 Operation：这样「没有可回滚的版本」会立刻报出来，
// 而不是先建一条注定失败的 Operation；执行时也不必再猜一次该回到哪里。
func (s *Server) handleRollbackApplication(w http.ResponseWriter, r *http.Request) {
	if s.deps.Deploys == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeRuntimeUnsupport, "本部署未启用应用部署能力"))
		return
	}

	var req v1.RollbackRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, s.logger(), err)
			return
		}
	}

	application := r.PathValue("id")
	target, err := s.deps.Deploys.PrepareRollback(r.Context(), application, req.To)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	s.createDeployOperation(w, r, v1.KindAppRollback, application, target.Release.ID,
		req.IdempotencyKey, req.CreatedBy, req.Retry)
}

// createDeployOperation 把「部署/回滚哪个 release」写进 Operation 并交给 Service.Create。
//
// spec 里存的是 **releaseId**（而不是"当前规格"）：要跑的那个版本的配置在创建期就选好了，
// 执行时不该再去读当前规格——否则回滚会回成一个「新配置 + 旧二进制」的混合体。
func (s *Server) createDeployOperation(
	w http.ResponseWriter,
	r *http.Request,
	kind, application, releaseID, idempotencyKey, createdBy string,
	retry *v1.RetrySpec,
) {
	if s.deps.Service == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "operation service is not configured"))
		return
	}
	if idempotencyKey == "" {
		idempotencyKey = r.Header.Get("Idempotency-Key")
	}

	options, err := jsonMarshalRelease(releaseID)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	op, created, err := s.deps.Service.Create(r.Context(), v1.CreateOperationRequest{
		Kind:           kind,
		Resource:       application,
		Spec:           options,
		IdempotencyKey: idempotencyKey,
		CreatedBy:      createdBy,
		Retry:          retry,
	})
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	release, err := s.deps.Catalogs.GetRelease(r.Context(), releaseID)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	status := http.StatusAccepted
	if !created {
		// 幂等键命中既有操作：复用而不是新建。
		status = http.StatusOK
	}
	writeJSON(w, status, v1.DeployResponse{
		APIVersion: v1.APIVersion,
		Release:    releaseDTO(release),
		Operation:  ptrOperation(operationDTO(op)),
	})
}

// jsonMarshalRelease 把 releaseId 编成 Operation 的 spec。
//
// 单独一个函数是为了让「Operation 里存的是什么」只有一处：它后来会被 worker 里的
// decodeReleaseID 读回去，两边对不上的表现是「部署一直失败，说什么参数解析不了」。
func jsonMarshalRelease(releaseID string) ([]byte, error) {
	raw, err := json.Marshal(struct {
		ReleaseID string `json:"releaseId"`
	}{ReleaseID: releaseID})
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "无法序列化部署参数: %v", err)
	}
	return raw, nil
}

func ptrOperation(op v1.Operation) *v1.Operation { return &op }
