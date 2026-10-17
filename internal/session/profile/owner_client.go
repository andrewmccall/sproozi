package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// CallOwner shares the fixed verified origin and private credential with the
// remote work catalogue. It cannot select a different origin or arbitrary path.
func (c *Client) CallOwner(ctx context.Context, action string, input, output any) error {
	switch action {
	case "run", "info", "list", "begin", "claim", "cancel", "recover":
	default:
		return errors.New("profile: unsupported owner action")
	}
	b, err := json.Marshal(input)
	if err != nil || int64(len(b)) > metadataLimit {
		return errors.New("profile: owner request exceeds bound")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/work/"+action, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set(headerAuthorization, bearerPrefix+c.token)
	r.Header.Set(headerContentType, jsonMedia)
	response, err := c.http.Do(r)
	if err != nil {
		return ownerFailure(CodeUnavailable, 0)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil || int64(len(data)) > c.maxBody {
		return ownerFailure(CodeUnavailable, response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error Code `json:"error"`
		}
		if strictDecode(data, &failure) == nil {
			if ownerErrorStatus(failure.Error, response.StatusCode) {
				return ownerFailure(failure.Error, response.StatusCode)
			}
		}
		code := CodeUnavailable
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			code = CodeDenied
		}
		return ownerFailure(code, response.StatusCode)
	}
	if strictDecode(data, output) != nil {
		return ownerFailure(CodeUnavailable, response.StatusCode)
	}
	return nil
}

func ownerFailure(code Code, status int) error {
	return fmt.Errorf("%s: %w", OwnerOutcomeMessage(code), &RemoteError{Code: code, Status: status})
}

// OwnerOutcomeMessage is fixed safe guidance, never an upstream error body.
func OwnerOutcomeMessage(code Code) string {
	switch code {
	case CodeInvalid:
		return "Invalid work request; check the session UUID, Project selector and task options"
	case CodeDenied:
		return "Work access denied; check configured credentials and approved repository, Project membership, harness and executor"
	case CodeConflict:
		return "Work intent conflicts with retained state; inspect status and preserve the original operation UUID on retries"
	case CodeExpired:
		return "Work execution expired; inspect retained status before submitting fresh work"
	case CodeUnsupported:
		return "Work continuity is incompatible; check harness support and the retained checkpoint before retrying"
	default:
		return "Work owner unavailable; restore connectivity or owner storage, then inspect status and retry with the same operation UUID"
	}
}

func ownerErrorStatus(code Code, status int) bool {
	return errorResponse(response{Error: code}, status)
}
