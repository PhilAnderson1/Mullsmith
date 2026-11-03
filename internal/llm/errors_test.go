package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestEndpointErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"connection", endpointConnectionFailure(errors.New("dial secret.internal: refused")), "could not connect to the model endpoint"},
		{"interrupted stream", endpointStreamFailure(errors.New("reset by peer")), "the connection to the model endpoint was interrupted"},
		{"header timeout", ErrStreamHeaderTimeout, "the model endpoint did not respond in time"},
		{"stream stall", ErrStreamStalled, "the model endpoint stopped responding"},
		{"bad response", endpointResponseFailure(errors.New("provider detail")), "the model endpoint returned an invalid response"},
		{"bad request", endpointHTTPFailure(400), "the model endpoint rejected the request or model"},
		{"bad key", endpointHTTPFailure(401), "the model endpoint rejected the API key"},
		{"credit", endpointHTTPFailure(402), "the model provider reports insufficient credit"},
		{"forbidden", endpointHTTPFailure(403), "the API key is not permitted to use this model"},
		{"missing", endpointHTTPFailure(404), "the configured endpoint or model was not found"},
		{"provider timeout", endpointHTTPFailure(408), "the model provider timed out"},
		{"too large", endpointHTTPFailure(413), "the request was too large for the model endpoint"},
		{"unprocessable", endpointHTTPFailure(422), "the model endpoint rejected the request or model"},
		{"rate limit", endpointHTTPFailure(429), "the model provider's rate limit was reached"},
		{"provider failure", endpointHTTPFailure(503), "the model provider is temporarily unavailable"},
		{"other status", endpointHTTPFailure(418), "the model endpoint failed; try again"},
		{"unknown failure", errors.New("local detail"), "the model endpoint failed; try again"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EndpointErrorMessage(tt.err); got != tt.want {
				t.Fatalf("EndpointErrorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEndpointFailurePreservesDiagnosticsAndClassification(t *testing.T) {
	cause := errors.New("connection refused at private address")
	err := endpointConnectionFailure(cause)
	if !errors.Is(err, ErrUpstream) || !errors.Is(err, cause) {
		t.Fatalf("endpoint failure lost error classification: %v", err)
	}
	if got := fmt.Sprint(err); got != "upstream request: connection refused at private address" {
		t.Fatalf("diagnostic error = %q", got)
	}
}
