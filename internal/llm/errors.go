package llm

import (
	"errors"
	"fmt"
)

// endpointFailure keeps diagnostic detail for the service log while carrying
// only a controlled category into the client-facing message mapper. Upstream
// response bodies are deliberately never retained here.
type endpointFailure struct {
	kind   endpointFailureKind
	status int
	cause  error
}

type endpointFailureKind uint8

const (
	failureConnection endpointFailureKind = iota + 1
	failureStream
	failureHTTP
	failureResponse
)

func (e *endpointFailure) Error() string {
	switch e.kind {
	case failureHTTP:
		return fmt.Sprintf("%s: HTTP %d", ErrUpstream, e.status)
	case failureStream:
		return fmt.Sprintf("upstream stream: %v", e.cause)
	case failureResponse:
		return fmt.Sprintf("%s: %v", ErrUpstream, e.cause)
	default:
		return fmt.Sprintf("upstream request: %v", e.cause)
	}
}

func (e *endpointFailure) Unwrap() error { return e.cause }

func (e *endpointFailure) Is(target error) bool {
	return target == ErrUpstream || (e.cause != nil && errors.Is(e.cause, target))
}

func endpointConnectionFailure(err error) error {
	return &endpointFailure{kind: failureConnection, cause: err}
}

func endpointStreamFailure(err error) error {
	return &endpointFailure{kind: failureStream, cause: err}
}

func endpointHTTPFailure(status int) error {
	return &endpointFailure{kind: failureHTTP, status: status}
}

func endpointResponseFailure(err error) error {
	return &endpointFailure{kind: failureResponse, cause: err}
}

// EndpointErrorMessage converts endpoint failures into stable, useful text
// safe to show to an authenticated browser. It never includes endpoint URLs,
// network addresses, response bodies or transport implementation details.
func EndpointErrorMessage(err error) string {
	switch {
	case errors.Is(err, ErrStreamHeaderTimeout):
		return "the model endpoint did not respond in time"
	case errors.Is(err, ErrStreamStalled):
		return "the model endpoint stopped responding"
	}

	var failure *endpointFailure
	if !errors.As(err, &failure) {
		return "the model endpoint failed; try again"
	}
	switch failure.kind {
	case failureConnection:
		return "could not connect to the model endpoint"
	case failureStream:
		return "the connection to the model endpoint was interrupted"
	case failureResponse:
		return "the model endpoint returned an invalid response"
	case failureHTTP:
		switch failure.status {
		case 400, 422:
			return "the model endpoint rejected the request or model"
		case 401:
			return "the model endpoint rejected the API key"
		case 402:
			return "the model provider reports insufficient credit"
		case 403:
			return "the API key is not permitted to use this model"
		case 404:
			return "the configured endpoint or model was not found"
		case 408:
			return "the model provider timed out"
		case 413:
			return "the request was too large for the model endpoint"
		case 429:
			return "the model provider's rate limit was reached"
		}
		if failure.status >= 500 && failure.status <= 599 {
			return "the model provider is temporarily unavailable"
		}
	}
	return "the model endpoint failed; try again"
}
