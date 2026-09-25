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

	// 备份。策略本身的非法沿用 MANIFEST_INVALID / MANIFEST_CONFLICT——
	// BackupPolicy 与 ApplicationSpec 一样是版本化 manifest，不另立一套。
	CodeBackupNotFound           ErrorCode = "BACKUP_NOT_FOUND"
	CodeBackupPreflightFailed    ErrorCode = "BACKUP_PREFLIGHT_FAILED"
	CodeBackupVerifyFailed       ErrorCode = "BACKUP_VERIFY_FAILED"
	CodeBackupRestoreUnconfirmed ErrorCode = "BACKUP_RESTORE_UNCONFIRMED"
	CodeBackupInUse              ErrorCode = "BACKUP_IN_USE"
	CodeBackupKeyUnresolved      ErrorCode = "BACKUP_KEY_UNRESOLVED"
	// CodeBackupRestoreFailed 是恢复**执行**失败。它与 EXEC_EXIT_NONZERO 刻意分开：
	// 后者说「一条命令跑失败了」，前者说「这次恢复没成功」。运维要据此判断
	// 「数据回来了没有」，混成一个码就等于把这个问题留给日志去翻。
	CodeBackupRestoreFailed ErrorCode = "BACKUP_RESTORE_FAILED"
	// CodeArtifactUnpackFailed 是**制品解不开**：不是 tar/zip、条目名逃出 release 目录、
	// 落点已被符号链接占住……它与 MANIFEST_INVALID 刻意分开：manifest 没问题，是制品
	// 本身与它声称的形态不符，而运维要做的事不同（重新上传制品，而不是改 manifest）。
	CodeArtifactUnpackFailed ErrorCode = "ARTIFACT_UNPACK_FAILED"
	// CodeDeployRolledBack 是**部署失败但已回滚**：进程与配置都回到了上一个稳定版本。
	// 它与 RUNTIME_NOT_READY 之类的区别在于它回答了运维的第一个问题——「现在线上跑的是
	// 哪个版本」。刻意不加进 1d 的重试白名单：部署失败通常不是瞬时故障，自动重试会把
	// 同一个坏版本反复推上去。
	CodeDeployRolledBack ErrorCode = "DEPLOY_ROLLED_BACK"
	// CodeNginxConfigInvalid 是**候选配置没通过校验**，或受管文件没有被主配置加载
	// （迭代 4）。两种情况都意味着「切流这件事现在不能做」，而**流量一点都没动**——
	// 它与 DEPLOY_ROLLED_BACK 刻意分开：那个码说的是「改动过线上并撤销了它」，
	// 而这个码回答的是「什么都没发生，去改配置」。
	CodeNginxConfigInvalid ErrorCode = "NGINX_CONFIG_INVALID"
	// CodeNginxReloadFailed 是 **reload 失败且换回原配置也没成功**：流量现在处于
	// 不确定状态，必须人工介入。刻意不复用 CONFIG_INVALID：那个码是「拒绝切流」，
	// 这个码是「切到一半出事了」，运维要做的事完全不同。
	CodeNginxReloadFailed ErrorCode = "NGINX_RELOAD_FAILED"
)

var httpStatusByCode = map[ErrorCode]int{
	CodeConfigInvalid:            400,
	CodeInvalidRequest:           400,
	CodeOperationNotFound:        404,
	CodeIdempotencyConflict:      409,
	CodeLockBusy:                 409,
	CodePermissionDenied:         403,
	CodeInternal:                 500,
	CodeArtifactNotFound:         404,
	CodeArtifactChecksum:         409,
	CodeArtifactInUse:            409,
	CodeUploadTooLarge:           413,
	CodeStorageQuotaExceeded:     507,
	CodeSecretUnresolved:         400,
	CodeApplicationNotFound:      404,
	CodeReleaseNotFound:          404,
	CodeReleaseConflict:          409,
	CodeScheduleNotFound:         404,
	CodeScheduleEnabled:          409,
	CodeScheduleInvalid:          400,
	CodeSpecNotFound:             404,
	CodeManifestInvalid:          400,
	CodeManifestConflict:         409,
	CodeRuntimeNotReady:          409,
	CodeRuntimeUnsupport:         409,
	CodeHostNotFound:             404,
	CodeEnvNotFound:              404,
	CodeRetryPolicyInvalid:       400,
	CodeBackupNotFound:           404,
	CodeBackupPreflightFailed:    409,
	CodeBackupVerifyFailed:       409,
	CodeBackupRestoreUnconfirmed: 409,
	CodeBackupInUse:              409,
	CodeBackupKeyUnresolved:      400,
	CodeBackupRestoreFailed:      409,
	CodeArtifactUnpackFailed:     409,
	CodeDeployRolledBack:         409,
	CodeNginxConfigInvalid:       409,
	CodeNginxReloadFailed:        409,
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
	CodeInternal:                 1,
	CodeConfigInvalid:            2,
	CodeInvalidRequest:           2,
	CodeOperationNotFound:        2,
	CodeIdempotencyConflict:      3,
	CodeLockBusy:                 4,
	CodeExecTimeout:              10,
	CodeExecCancelled:            11,
	CodeExecExitNonZero:          12,
	CodeDaemonRestarted:          13,
	CodePermissionDenied:         20,
	CodeArtifactNotFound:         2,
	CodeArtifactChecksum:         5,
	CodeArtifactInUse:            6,
	CodeUploadTooLarge:           7,
	CodeStorageQuotaExceeded:     8,
	CodeSecretUnresolved:         9,
	CodeApplicationNotFound:      2,
	CodeReleaseNotFound:          2,
	CodeReleaseConflict:          14,
	CodeScheduleNotFound:         2,
	CodeScheduleEnabled:          15,
	CodeScheduleInvalid:          16,
	CodeSpecNotFound:             2,
	CodeManifestInvalid:          17,
	CodeManifestConflict:         18,
	CodeRuntimeNotReady:          19,
	CodeRuntimeUnsupport:         21,
	CodeHostNotFound:             2,
	CodeEnvNotFound:              2,
	CodeRetryPolicyInvalid:       22,
	CodeBackupNotFound:           2,
	CodeBackupPreflightFailed:    23,
	CodeBackupVerifyFailed:       24,
	CodeBackupRestoreUnconfirmed: 25,
	CodeBackupInUse:              26,
	CodeBackupKeyUnresolved:      27,
	CodeBackupRestoreFailed:      28,
	CodeArtifactUnpackFailed:     30,
	CodeDeployRolledBack:         29,
	CodeNginxConfigInvalid:       31,
	CodeNginxReloadFailed:        32,
}

func ExitCode(code ErrorCode) int {
	if c, ok := exitCodeByCode[code]; ok {
		return c
	}
	return 1
}
