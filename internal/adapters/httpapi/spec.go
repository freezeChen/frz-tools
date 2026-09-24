package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/manifest"
	"github.com/freezeChen/frz-tools/internal/domain"
)

func (s *Server) handlePutApplicationSpec(w http.ResponseWriter, r *http.Request) {
	if s.deps.Specs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "spec service is not configured"))
		return
	}

	var req v1.PutApplicationSpecRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	// manifest 必须先过严格解码（KnownFields）与迁移链：这是唯一能挡住「未知字段被
	// 静默丢弃」的入口，因此这里不接受已经结构化的 spec，也不在本层做字段校验——
	// 校验规则只有 domain.ApplicationSpec 一份。
	spec, err := manifest.ParseReader(strings.NewReader(req.Manifest))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}

	if err := s.checkUnpackMediaType(r.Context(), spec); err != nil {
		writeError(w, s.logger(), err)
		return
	}

	stored, err := s.deps.Specs.PutSpec(r.Context(), r.PathValue("id"), spec, req.UpdatedBy)
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ApplicationSpecResponse{APIVersion: v1.APIVersion, Spec: specDTO(stored)})
}

func (s *Server) handleGetApplicationSpec(w http.ResponseWriter, r *http.Request) {
	if s.deps.Specs == nil {
		writeError(w, s.logger(), domain.NewError(v1.CodeInternal, "spec service is not configured"))
		return
	}

	spec, err := s.deps.Specs.GetSpec(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, s.logger(), err)
		return
	}
	writeJSON(w, http.StatusOK, v1.ApplicationSpecResponse{APIVersion: v1.APIVersion, Spec: specDTO(spec)})
}

// checkUnpackMediaType 在能拿到制品 mediaType 时判定 unpack 策略是否与之冲突
// （MANIFEST_CONFLICT）。取不到 mediaType 的情况下不做判定，而不是把「查不到制品」
// 当成 manifest 的错：
//
//   - 制品尚未上传：制品的存在性是发布流程（迭代 3）的校验对象，manifest 本身合法；
//   - 本部署未启用制品存储（artifactStore.root 为空）：此时根本不存在任何制品，
//     冲突无从谈起，提交 manifest 也不该因此失败。
//
// 其余错误照常上抛，避免把真正的内部故障藏进「跳过校验」里。
func (s *Server) checkUnpackMediaType(ctx context.Context, spec *domain.ApplicationSpec) error {
	if s.deps.Artifacts == nil {
		return nil
	}

	ref := spec.Artifact.ID
	if ref == "" {
		ref = spec.Artifact.Digest
	}
	artifact, err := s.deps.Artifacts.Get(ctx, ref)
	switch {
	case err == nil:
		return spec.CheckUnpackMediaType(artifact.MediaType)
	case domain.CodeOf(err) == v1.CodeArtifactNotFound, domain.CodeOf(err) == v1.CodeConfigInvalid:
		return nil
	default:
		return err
	}
}

// specDTO 把领域规格转成 wire 形态。两个需要翻译的地方：Duration 回到 manifest 里
// 的秒数（manifest 只接受整秒），以及类型别名转成 wire 的字符串取值。
func specDTO(spec *domain.ApplicationSpec) v1.ApplicationSpec {
	secrets := make(map[string]v1.SecretRef, len(spec.Exec.SecretEnvironment))
	for name, ref := range spec.Exec.SecretEnvironment {
		secrets[name] = v1.SecretRef{Kind: string(ref.Kind), Name: ref.Name}
	}
	if len(secrets) == 0 {
		secrets = nil
	}

	return v1.ApplicationSpec{
		APIVersion:  spec.APIVersion,
		Kind:        spec.Kind,
		Application: spec.Application,
		Runtime:     string(spec.Runtime),
		Artifact: v1.SpecArtifact{
			ID:     spec.Artifact.ID,
			Digest: spec.Artifact.Digest,
			Unpack: v1.SpecUnpack{
				Strategy:        string(spec.Artifact.Unpack.Strategy),
				StripComponents: spec.Artifact.Unpack.StripComponents,
			},
		},
		Exec: v1.SpecExec{
			Argv:              spec.Exec.Argv,
			WorkingDirectory:  spec.Exec.WorkingDirectory,
			RunUser:           spec.Exec.RunUser,
			Environment:       spec.Exec.Environment,
			SecretEnvironment: secrets,
			Ports:             spec.Exec.Ports,
		},
		Health: v1.SpecHealth{
			Readiness: v1.SpecReadiness{
				Type:                 string(spec.Health.Readiness.Type),
				Target:               spec.Health.Readiness.Target,
				ConsecutiveSuccesses: spec.Health.Readiness.ConsecutiveSuccesses,
			},
			StartTimeoutSeconds: wholeSeconds(spec.Health.StartTimeout),
			StopTimeoutSeconds:  wholeSeconds(spec.Health.StopTimeout),
		},
		Logs: v1.SpecLogs{Directory: spec.Logs.Directory},
		Systemd: v1.SpecSystemd{
			UnitName:      spec.Systemd.UnitName,
			RestartPolicy: string(spec.Systemd.RestartPolicy),
		},
	}
}

func wholeSeconds(d time.Duration) int {
	return int(d / time.Second)
}
