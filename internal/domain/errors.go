package domain

import (
	"errors"
	"fmt"

	v1 "frz-tools/api/v1"
)

type CodedError struct {
	Code    v1.ErrorCode
	Message string
	Details map[string]any
}

func (e *CodedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewError(code v1.ErrorCode, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *CodedError) WithDetail(key string, value any) *CodedError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

func CodeOf(err error) v1.ErrorCode {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return v1.CodeInternal
}

func MessageOf(err error) string {
	if err == nil {
		return ""
	}
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Message
	}
	return err.Error()
}
