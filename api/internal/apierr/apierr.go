package apierr

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

type Code string

const (
	CodeConflict            Code = "CONFLICT"
	CodeIdempotencyMismatch Code = "IDEMPOTENCY_MISMATCH"
	CodeInternal            Code = "INTERNAL"
	CodeNotFound            Code = "NOT_FOUND"
	CodeRateLimited         Code = "RATE_LIMITED"
	CodeUnavailable         Code = "UNAVAILABLE"
	CodeUnauthorized        Code = "UNAUTHORIZED"
	CodeValidationFailed    Code = "VALIDATION_FAILED"
)

type Error struct {
	Code    Code
	Message string
	Details map[string]any
	cause   error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error {
	return e.cause
}

func New(code Code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

func WrapInternal(err error) *Error {
	return &Error{Code: CodeInternal, Message: "Внутренняя ошибка сервера", cause: err}
}

func Status(code Code) int {
	switch code {
	case CodeValidationFailed:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeIdempotencyMismatch:
		return http.StatusUnprocessableEntity
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func Write(c *gin.Context, err error) {
	var known *Error
	if !errors.As(err, &known) {
		known = WrapInternal(err)
	}

	message := known.Message
	if Status(known.Code) == http.StatusInternalServerError {
		message = "Внутренняя ошибка сервера"
	}

	details := known.Details
	if details == nil {
		details = map[string]any{}
	}

	c.AbortWithStatusJSON(Status(known.Code), gin.H{
		"error": gin.H{
			"code":    known.Code,
			"message": message,
			"details": details,
		},
	})
}
