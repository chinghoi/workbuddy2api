package server

import "bytes"

// isNonRetryableUpstreamRequestError identifies deterministic request-shape
// failures. Retrying the same payload with another account cannot succeed and
// must not consume or cool healthy accounts.
func isNonRetryableUpstreamRequestError(status int, body []byte) bool {
	if status != 400 && status != 422 {
		return false
	}
	return bytes.Contains(body, []byte(`"code":11133`)) ||
		bytes.Contains(body, []byte(`"code":"invalid_parameter_value"`)) ||
		bytes.Contains(body, []byte(`"type":"invalid_request_error"`)) ||
		bytes.Contains(bytes.ToLower(body), []byte("invalid request parameters"))
}
