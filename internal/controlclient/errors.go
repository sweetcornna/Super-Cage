package controlclient

import "strings"

// AgentError is a refusal in the agent transport's vocabulary.
type AgentError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	NextAction string `json:"next_action"`
	Retryable  bool   `json:"retryable"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *AgentError) Error() string { return e.Message }

// HTTPError names an HTTP refusal.
func HTTPError(code int) *AgentError {
	e := &AgentError{Code: "request_rejected", Message: "Super Cage rejected the operation. Check the input and run doctor.", NextAction: "doctor", HTTPStatus: code}
	switch code {
	case 401:
		e.Code = "authentication_failed"
		e.NextAction = "service.start"
	case 423:
		e.Code = "account_locked"
		e.Message = "Unlock Super Cage before configuring the environment."
		e.NextAction = "account.unlock"
	case 403:
		e.Code = "user_action_required"
		e.Message = "This operation requires a local system or account action."
		e.NextAction = "open_desktop"
	case 409:
		e.Code = "state_conflict"
		e.Message = "The environment state does not permit this operation. Read status before retrying."
		e.NextAction = "status"
	case 410:
		e.Code = "job_not_found"
		e.Message = "The original job is no longer available. Inspect state before submitting again."
		e.NextAction = "status"
	case 404:
		e.Code = "unsupported_operation"
		e.Message = "This controller or guest does not support the operation."
		e.NextAction = "capabilities"
	case 429, 503:
		e.Code = "service_busy"
		e.NextAction = "status"
		e.Retryable = true
	}
	return e
}

// AgentTarget separates the desktop's client configuration from its selected
// workload. A selected workload always requires an explicit generation.
type AgentTarget struct {
	Scope      string `json:"scope"`
	Connection string `json:"connection_id,omitempty"`
}

// Connection is the connection generation a request names: the selected
// one, or the local workload.
func Connection(target AgentTarget) string {
	if target.Connection != "" {
		return target.Connection
	}
	return "local"
}

// CheckSocket refuses a workload WebSocket path or target the UI would never
// open.
func CheckSocket(target AgentTarget, path string) error {
	if !strings.HasPrefix(path, "/api/") || strings.ContainsAny(path, "\r\n\\") || strings.Contains(path, "#") || strings.Contains(path, "://") {
		return &AgentError{Code: "invalid_input", Message: "Use a relative /api/ WebSocket path", NextAction: "routes"}
	}
	if target.Scope == "desktop" {
		return &AgentError{Code: "invalid_input", Message: "Use local or selected scope for a workload WebSocket", NextAction: "client.context"}
	}
	if target.Scope == "client" && target.Connection == "" {
		return &AgentError{Code: "invalid_input", Message: "Client sockets require an explicit connection generation", NextAction: "client.context"}
	}
	return nil
}
