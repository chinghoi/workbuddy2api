// responses.go — POST /v1/responses endpoint: translate Responses API requests
// to the chat-completions dialect accepted by WorkBuddy, then restore Responses
// API JSON/SSE output for ChatGPT.app and other clients.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// responses handles POST /v1/responses.
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	// Keep complete request/upstream/output artifacts while compatibility work is
	// in progress. data/ is gitignored and capture files are mode 0600.
	capture := startFullIOCapture(w, body)
	if capture != nil {
		w = capture
		defer capture.Close() //nolint:errcheck // debug capture must not alter API behavior
	}
	// Normalize Chat Completions token usage to the field names and detail
	// objects required by Responses API clients. This wrapper is outside the
	// capture writer so output.raw records the exact normalized client output.
	w = newResponsesUsageWriter(w)

	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// Preserve the complete client request in full_io, but remove opaque browser
	// screenshot base64 before translating tool outputs to text for the upstream
	// model. Then add routing guidance for an explicitly selected Browser plugin.
	compactedBody := compactResponsesToolOutputs(body)
	routedBody := injectBrowserPluginRouting(compactedBody)
	bridge := bridgeResponsesRequest(routedBody)
	// Responses API stores parallel function calls as separate input items.
	// Chat Completions requires those calls in one assistant.tool_calls array.
	chatBody := mergeParallelAssistantToolCalls(bridge.ChatBody)
	if capture != nil {
		// ChatStream calls PrepareBody before sending. Record that exact dialect,
		// rather than only the pre-normalized bridge output.
		capture.RecordUpstreamRequest(upstream.PrepareBody(chatBody))
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var upstreamErr *upstream.Error
				if errors.As(err, &upstreamErr) && upstreamErr.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			_ = acct.SaveAtomic()
		}

		rc, status, responseBody, transportErr := h.cfg.Upstream.ChatStream(acct, chatBody, r.Context())
		if transportErr != nil {
			lastErr = transportErr
			if capture != nil {
				capture.RecordUpstreamError(0, []byte(transportErr.Error()))
			}
			h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			if capture != nil {
				capture.RecordUpstreamError(status, responseBody)
			}
			// Invalid request parameters are payload-wide, not account-specific.
			// Return immediately so identical retries do not cool every account and
			// turn one deterministic 400 into a misleading pool-wide 503.
			if isNonRetryableUpstreamRequestError(status, responseBody) {
				writeOpenAIError(w, status, "upstream_invalid_request", string(responseBody))
				return
			}
			kind := upstream.Classify(status, string(responseBody))
			switch kind {
			case upstream.ErrHardCredit:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "12153 session dead")
			case upstream.ErrNotFound:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			default:
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(responseBody)}
			continue
		}

		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UID)
		var upstreamReader io.Reader = rc
		if capture != nil {
			upstreamReader = capture.TeeUpstream(rc)
		}
		if peek.Stream {
			_ = upstream.StreamResponsesWithTools(w, upstreamReader, peek.Model, bridge.Tools)
			return
		}
		response, err := upstream.AggregateResponseWithTools(upstreamReader, peek.Model, bridge.Tools)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	message := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		message += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", message)
}

// Compatibility wrappers retained for existing tests and callers inside the
// package. The bridge is now the single source of request conversion behavior.
func responsesToChatBody(body []byte) []byte {
	return bridgeResponsesRequest(body).ChatBody
}

func convertInputItem(item map[string]any) map[string]any {
	return bridgeInputItem(item)
}

func extractContent(content any) string {
	return bridgeExtractContent(content)
}

func imageURL(value any) string {
	return bridgeImageURL(value)
}
