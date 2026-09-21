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
)

var httpStatusByCode = map[ErrorCode]int{
	CodeConfigInvalid:       400,
	CodeInvalidRequest:      400,
	CodeOperationNotFound:   404,
	CodeIdempotencyConflict: 409,
	CodeLockBusy:            409,
	CodePermissionDenied:    403,
	CodeInternal:            500,
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
	CodeInternal:            1,
	CodeConfigInvalid:       2,
	CodeInvalidRequest:      2,
	CodeOperationNotFound:   2,
	CodeIdempotencyConflict: 3,
	CodeLockBusy:            4,
	CodeExecTimeout:         10,
	CodeExecCancelled:       11,
	CodeExecExitNonZero:     12,
	CodeDaemonRestarted:     13,
	CodePermissionDenied:    20,
}

func ExitCode(code ErrorCode) int {
	if c, ok := exitCodeByCode[code]; ok {
		return c
	}
	return 1
}
