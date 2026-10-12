package profile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/andrewmccall/sproozi/internal/session"
)

const terminalInputLimit = int64(8 << 20)
const terminalOutputLimit = int64(64 << 20)
const terminalFrameLimit = 256 << 10

// frameReader admits only the native terminal's write/resize frames. It never
// admits native control/configure commands or a user-selected executable.
type frameReader struct {
	scanner *bufio.Scanner
	pending []byte
	total   int64
}

func newFrameReader(input io.Reader) *frameReader {
	s := bufio.NewScanner(input)
	s.Buffer(make([]byte, 4096), terminalFrameLimit)
	return &frameReader{scanner: s}
}
func (f *frameReader) Read(data []byte) (int, error) {
	if len(f.pending) == 0 {
		if !f.scanner.Scan() {
			if err := f.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := f.scanner.Bytes()
		f.total += int64(len(line)) + 1
		if f.total > terminalInputLimit {
			return 0, errors.New("profile: terminal input exceeds attachment bound")
		}
		var frame struct {
			Op      string `json:"op"`
			Data    []byte `json:"data,omitempty"`
			Rows    uint16 `json:"rows,omitempty"`
			Columns uint16 `json:"columns,omitempty"`
		}
		if strictDecode(line, &frame) != nil || frame.Op != "write" && frame.Op != "resize" || frame.Op == "write" && (frame.Rows != 0 || frame.Columns != 0) || frame.Op == "resize" && (len(frame.Data) > 0 || frame.Rows == 0 || frame.Columns == 0) {
			return 0, errors.New("profile: invalid terminal frame")
		}
		f.pending = append(bytes.Clone(line), '\n')
	}
	n := copy(data, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}
func envelopeLine(reader *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		part, err := reader.ReadSlice('\n')
		if int64(len(data)+len(part)) > metadataLimit {
			return nil, errors.New("profile: terminal envelope exceeds bound")
		}
		data = append(data, part...)
		if err == nil {
			return data, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

func (h *handler) attach(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get(headerContentType))
	if err != nil || media != jsonMedia {
		writeError(w, http.StatusUnsupportedMediaType, CodeInvalid)
		return
	}
	backend, ok := h.config.Backend.(session.TerminalPlacement)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, CodeUnsupported)
		return
	}
	reader := bufio.NewReader(r.Body)
	line, err := envelopeLine(reader)
	var input request
	if err != nil || strictDecode(line, &input) != nil || input.Version != protocolVersion || validateLaunch(input.Launch) != nil || input.Launch.Placement != h.config.Placement || input.Incarnation == nil || !incarnationValid(*input.Incarnation) || len(input.Checkpoint) > 0 {
		writeError(w, http.StatusBadRequest, CodeInvalid)
		return
	}
	if err = h.validateAdmission(actionActivate, input.Launch); err != nil {
		h.fail(w, err)
		return
	}
	o, err := h.config.Backend.Inspect(r.Context(), input.Launch)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !sameLaunch(o.Launch, input.Launch) || o.State != session.PlacementActive || o.Incarnation != *input.Incarnation {
		writeError(w, http.StatusConflict, CodeConflict)
		return
	}
	controller := http.NewResponseController(w)
	if err = controller.EnableFullDuplex(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeUnsupported)
		return
	}
	w.WriteHeader(http.StatusOK)
	if err = controller.Flush(); err != nil {
		return
	}
	output := &terminalOutput{writer: w, controller: controller}
	ctx, cancel := context.WithDeadline(r.Context(), input.Launch.Deadline)
	defer cancel()
	if err = backend.Attach(ctx, input.Launch, *input.Incarnation, newFrameReader(reader), output); err != nil {
		_, _ = io.WriteString(output, "{\"op\":\"attach\",\"error\":\"attachment unavailable\"}\n")
	}
}

type terminalOutput struct {
	writer     io.Writer
	controller *http.ResponseController
	total      int64
}

func (o *terminalOutput) Write(data []byte) (int, error) {
	o.total += int64(len(data))
	if o.total > terminalOutputLimit {
		return 0, errors.New("profile: terminal output exceeds attachment bound")
	}
	n, err := o.writer.Write(data)
	if err == nil {
		err = o.controller.Flush()
	}
	return n, err
}

type attachmentBody struct {
	io.Reader
	closer io.Closer
}

func (b *attachmentBody) Close() error {
	if b.closer != nil {
		return b.closer.Close()
	}
	return nil
}

// Attach carries native terminal JSON frames over the fixed authenticated origin.
// The attachment owns input until return; a blocking input must be a ReadCloser
// so request cancellation can unblock it. Reconnect before the per-stream byte
// bound. Disconnect ends only the attachment and never stops the native writer.
func (c *Client) Attach(ctx context.Context, l session.Launch, i session.Incarnation, input io.Reader, output io.Writer) error {
	if validateLaunch(l) != nil || !incarnationValid(i) || input == nil || output == nil {
		return errors.New("profile: invalid attachment")
	}
	ctx, cancel := context.WithDeadline(ctx, l.Deadline)
	defer cancel()
	envelope, err := json.Marshal(request{Version: protocolVersion, Launch: l, Incarnation: &i})
	if err != nil || int64(len(envelope))+1 > metadataLimit {
		return errors.New("profile: invalid attachment envelope")
	}
	envelope = append(envelope, '\n')
	body := &attachmentBody{Reader: io.MultiReader(bytes.NewReader(envelope), newFrameReader(input))}
	body.closer, _ = input.(io.Closer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/"+actionAttach, body)
	if err != nil {
		return err
	}
	req.Header.Set(headerAuthorization, bearerPrefix+c.token)
	req.Header.Set(headerContentType, jsonMedia)
	// Long native attachments use the immutable execution deadline instead of
	// the short control-request timeout. Origin and transport stay unchanged.
	client := *c.http
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		_ = body.Close()
		return err
	}
	defer func() { _ = resp.Body.Close(); _ = body.Close() }()
	media, _, err := mime.ParseMediaType(resp.Header.Get(headerContentType))
	if err != nil || media != jsonMedia {
		return errors.New("profile: invalid attachment response")
	}
	if resp.StatusCode != http.StatusOK {
		data, err := io.ReadAll(io.LimitReader(resp.Body, metadataLimit+1))
		var r response
		if err != nil || int64(len(data)) > metadataLimit || strictDecode(data, &r) != nil || r.Version != protocolVersion || !errorResponse(r, resp.StatusCode) {
			return errors.New("profile: attachment unavailable")
		}
		return &RemoteError{Code: r.Error, Status: resp.StatusCode}
	}
	return copyTerminalOutput(resp.Body, output)
}

func copyTerminalOutput(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(io.LimitReader(input, terminalOutputLimit+1))
	scanner.Buffer(make([]byte, 4096), terminalFrameLimit)
	var total int64
	for scanner.Scan() {
		line := scanner.Bytes()
		total += int64(len(line)) + 1
		if total > terminalOutputLimit {
			return errors.New("profile: terminal output exceeds attachment bound")
		}
		var frame struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &frame) != nil {
			return errors.New("profile: invalid native terminal response")
		}
		if _, err := output.Write(append(bytes.Clone(line), '\n')); err != nil {
			return err
		}
		if frame.Error != "" {
			return errors.New("profile: native attachment failed")
		}
	}
	return scanner.Err()
}

var _ session.TerminalPlacement = (*Client)(nil)
