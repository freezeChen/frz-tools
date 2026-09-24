package v1

type ErrorCode string

const (
	CodeConfigInvalid       ErrorCode = "CONFIG_INVALID"
	CodeInvalidRequest      ErrorCode = "INVALID_REQUEST"
	CodeOperationNotFound   ErrorCode = "OPERATION_NOT_FOUND"
	CodeIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT"
	CodeLockBusy            ErrorCode = "LOCK_BUSY"
	CodeExecTimeout         ErrorCode = "EXEC_TIMEOUT"
	CodeExecCancelled       ErrorCode = "EXEC_CANCELLED"
	CodeExecExitNonZero     ErrorCode = "EXEC_EXIT_NONZERO"
	CodeDaemonRestarted     ErrorCode = "DAEMON_RESTARTED"
	CodePermissionDenied    ErrorCode = "PERMISSION_DENIED"
	CodeInternal            ErrorCode = "INTERNAL"

	CodeArtifactNotFound     ErrorCode = "ARTIFACT_NOT_FOUND"
	CodeArtifactChecksum     ErrorCode = "ARTIFACT_CHECKSUM_MISMATCH"
	CodeArtifactInUse        ErrorCode = "ARTIFACT_IN_USE"
	CodeUploadTooLarge       ErrorCode = "UPLOAD_TOO_LARGE"
	CodeStorageQuotaExceeded ErrorCode = "STORAGE_QUOTA_EXCEEDED"
	CodeSecretUnresolved     ErrorCode = "SECRET_UNRESOLVED"
	CodeApplicationNotFound  ErrorCode = "APPLICATION_NOT_FOUND"
	CodeReleaseNotFound      ErrorCode = "RELEASE_NOT_FOUND"
	CodeReleaseConflict      ErrorCode = "RELEASE_CONFLICT"

	CodeScheduleNotFound ErrorCode = "SCHEDULE_NOT_FOUND"
	CodeScheduleEnabled  ErrorCode = "SCHEDULE_ENABLED"
	CodeScheduleInvalid  ErrorCode = "SCHEDULE_INVALID"

	CodeSpecNotFound     ErrorCode = "SPEC_NOT_FOUND"
	CodeManifestInvalid  ErrorCode = "MANIFEST_INVALID"
	CodeManifestConflict ErrorCode = "MANIFEST_CONFLICT"
	CodeRuntimeNotReady  ErrorCode = "RUNTIME_NOT_READY"
	CodeRuntimeUnsupport ErrorCode = "RUNTIME_UNSUPPORTED"
	CodeHostNotFound     ErrorCode = "HOST_NOT_FOUND"
	CodeEnvNotFound      ErrorCode = "ENVIRONMENT_NOT_FOUND"

	// 重试策略本身非法。单独成一个码而不是复用 INVALID_REQUEST：调用方需要能一眼
	// 看出问题出在 retry 段，而不是整个请求体。
	CodeRetryPolicyInvalid ErrorCode = "RETRY_POLICY_INVALID"
)

var httpStatusByCode = map[ErrorCode]int{
	CodeConfigInvalid:        400,
	CodeInvalidRequest:       400,
	CodeOperationNotFound:    404,
	CodeIdempotencyConflict:  409,
	CodeLockBusy:             409,
	CodePermissionDenied:     403,
	CodeInternal:             500,
	CodeArtifactNotFound:     404,
	CodeArtifactChecksum:     409,
	CodeArtifactInUse:        409,
	CodeUploadTooLarge:       413,
	CodeStorageQuotaExceeded: 507,
	CodeSecretUnresolved:     400,
	CodeApplicationNotFound:  404,
	CodeReleaseNotFound:      404,
	CodeReleaseConflict:      409,
	CodeScheduleNotFound:     404,
	CodeScheduleEnabled:      409,
	CodeScheduleInvalid:      400,
	CodeSpecNotFound:         404,
	CodeManifestInvalid:      400,
	CodeManifestConflict:     409,
	CodeRuntimeNotReady:      409,
	CodeRuntimeUnsupport:     409,
	CodeHostNotFound:         404,
	CodeEnvNotFound:          404,
	CodeRetryPolicyInvalid:   400,
}

// HTTPStatus 返回错误码在被 API 处理器直接返回时对应的 HTTP 状态码。
// 操作结果类错误码（EXEC_*、DAEMON_RESTARTED）不会作为 HTTP 错误返回，
// 它们只出现在 Operation 记录中，因此在这里统一回落到 500。
func HTTPStatus(code ErrorCode) int {
	if s, ok := httpStatusByCode[code]; ok {
		return s
	}
	return 500
}

var exitCodeByCode = map[ErrorCode]int{
	CodeInternal:             1,
	CodeConfigInvalid:        2,
	CodeInvalidRequest:       2,
	CodeOperationNotFound:    2,
	CodeIdempotencyConflict:  3,
	CodeLockBusy:             4,
	CodeExecTimeout:          10,
	CodeExecCancelled:        11,
	CodeExecExitNonZero:      12,
	CodeDaemonRestarted:      13,
	CodePermissionDenied:     20,
	CodeArtifactNotFound:     2,
	CodeArtifactChecksum:     5,
	CodeArtifactInUse:        6,
	CodeUploadTooLarge:       7,
	CodeStorageQuotaExceeded: 8,
	CodeSecretUnresolved:     9,
	CodeApplicationNotFound:  2,
	CodeReleaseNotFound:      2,
	CodeReleaseConflict:      14,
	CodeScheduleNotFound:     2,
	CodeScheduleEnabled:      15,
	CodeScheduleInvalid:      16,
	CodeSpecNotFound:         2,
	CodeManifestInvalid:      17,
	CodeManifestConflict:     18,
	CodeRuntimeNotReady:      19,
	CodeRuntimeUnsupport:     21,
	CodeHostNotFound:         2,
	CodeEnvNotFound:          2,
	CodeRetryPolicyInvalid:   22,
}

func ExitCode(code ErrorCode) int {
	if c, ok := exitCodeByCode[code]; ok {
		return c
	}
	return 1
}
