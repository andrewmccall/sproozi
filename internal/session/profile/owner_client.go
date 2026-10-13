package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		return err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil || int64(len(data)) > c.maxBody {
		return errors.New("profile: owner response exceeds bound")
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("profile: owner request rejected or unavailable")
	}
	return strictDecode(data, output)
}
