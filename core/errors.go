package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/genproto/googleapis/rpc/status"
)

// An Error is a call the API refused, or an operation that failed.
type Error struct {
	// HTTPStatus is the response's status code; 0 for an operation's
	// error.
	HTTPStatus int
	// Status is the closest canonical code, such as "NOT_FOUND", for
	// generic tooling.
	Status string
	// Reason is the platform's error code, such as "NOT_FOUND" or
	// "CONFLICT": what to branch on.
	Reason string
	// Message says what went wrong, for a person.
	Message string
	// RequestID identifies the request to Origin Cloud's support.
	RequestID string
	// FieldViolations name the request fields that were refused, by their
	// JSON path.
	FieldViolations []FieldViolation
	// RetryDelay is how long the API asks to wait before trying again;
	// 0 when it doesn't ask.
	RetryDelay time.Duration

	retryable bool
}

// A FieldViolation is one request field the API refused.
type FieldViolation struct {
	// Field is the field's JSON path, such as "volume.sizeGib".
	Field       string
	Description string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("origincloud: ")
	switch {
	case e.Reason != "":
		b.WriteString(e.Reason)
	case e.Status != "":
		b.WriteString(e.Status)
	default:
		fmt.Fprintf(&b, "HTTP %d", e.HTTPStatus)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request %s)", e.RequestID)
	}
	return b.String()
}

// Retryable reports whether the API answered that the same call may
// succeed if it is sent again.
func (e *Error) Retryable() bool { return e.retryable }

// ReasonOf returns the Reason of the [*Error] err wraps, or "" when it
// wraps none.
func ReasonOf(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Reason
	}
	return ""
}

const (
	typeErrorInfo   = "type.googleapis.com/google.rpc.ErrorInfo"
	typeRequestInfo = "type.googleapis.com/google.rpc.RequestInfo"
	typeRetryInfo   = "type.googleapis.com/google.rpc.RetryInfo"
	typeBadRequest  = "type.googleapis.com/google.rpc.BadRequest"
)

// The JSON a refused call answers with: a google.rpc.Status in its HTTP
// form.
type statusBody struct {
	Error *struct {
		Code    int               `json:"code"`
		Status  string            `json:"status"`
		Message string            `json:"message"`
		Details []json.RawMessage `json:"details"`
	} `json:"error"`
}

// errorFromResponse reads the error a response with a status other than
// 200 carries. A response that carries none, such as a proxy's, is an
// error by its status alone, retryable when the status is.
func errorFromResponse(httpStatus int, body []byte) *Error {
	var parsed statusBody
	if json.Unmarshal(body, &parsed) != nil || parsed.Error == nil {
		e := &Error{HTTPStatus: httpStatus, Message: http.StatusText(httpStatus)}
		switch httpStatus {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			e.Status, e.retryable = "UNAVAILABLE", true
		}
		return e
	}
	e := &Error{HTTPStatus: httpStatus, Status: parsed.Error.Status, Message: parsed.Error.Message}
	for _, raw := range parsed.Error.Details {
		var detail struct {
			Type            string `json:"@type"`
			Reason          string `json:"reason"`
			RequestID       string `json:"requestId"`
			RetryDelay      string `json:"retryDelay"`
			FieldViolations []struct {
				Field       string `json:"field"`
				Description string `json:"description"`
			} `json:"fieldViolations"`
		}
		if json.Unmarshal(raw, &detail) != nil {
			continue
		}
		switch detail.Type {
		case typeErrorInfo:
			e.Reason = detail.Reason
		case typeRequestInfo:
			e.RequestID = detail.RequestID
		case typeRetryInfo:
			e.retryable = true
			// A JSON duration ("1.5s") is one time.ParseDuration reads.
			if delay, err := time.ParseDuration(detail.RetryDelay); err == nil {
				e.RetryDelay = delay
			}
		case typeBadRequest:
			for _, v := range detail.FieldViolations {
				e.FieldViolations = append(e.FieldViolations, FieldViolation{Field: v.Field, Description: v.Description})
			}
		}
	}
	return e
}

// errorFromStatus is the error a failed operation carries.
func errorFromStatus(s *status.Status) *Error {
	e := &Error{Status: code.Code(s.GetCode()).String(), Message: s.GetMessage()}
	for _, detail := range s.GetDetails() {
		message, err := detail.UnmarshalNew()
		if err != nil {
			continue
		}
		switch d := message.(type) {
		case *errdetails.ErrorInfo:
			e.Reason = d.GetReason()
		case *errdetails.RequestInfo:
			e.RequestID = d.GetRequestId()
		case *errdetails.BadRequest:
			for _, v := range d.GetFieldViolations() {
				e.FieldViolations = append(e.FieldViolations, FieldViolation{Field: v.GetField(), Description: v.GetDescription()})
			}
		}
	}
	return e
}
