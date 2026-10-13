package model

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

const (
	responseIncompleteEvent = "response.incomplete"
)

const (
	responseFailedEvent = "response.failed"
)

var diagnosticSecrets = regexp.MustCompile(`(?i)(?:bearer\s+\S+|(?:sk-|gh[opsur]_)[a-z0-9_-]+|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|(?:access_token|refresh_token|id_token|api_key|authorization)\s*[=:]\s*[^\s,;]+)`)
var diagnosticIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_.\[\]-]{1,128}$`)

func diagnosticText(text, credential string) string {
	if credential != "" {
		text = strings.ReplaceAll(text, credential, "<REDACTED>")
	}
	text = diagnosticSecrets.ReplaceAllString(text, "<REDACTED>")
	if len(text) > 1024 {
		text = text[:1024] + "..."
	}
	return text
}

func diagnosticID(value, credential string) string {
	if diagnosticIdentifier.MatchString(value) && diagnosticText(value, credential) == value {
		return value
	}
	return ""
}

// ResponseDiagnostic is separate from the credential-free gateway audit.
// ErrorMessage is opt-in diagnostic text, not an authorization decision.
type ResponseDiagnostic struct {
	Kind            string   `json:"diagnostic"`
	RunUID          string   `json:"runUID"`
	RequestID       string   `json:"requestID,omitempty"`
	HTTPStatus      int      `json:"httpStatus"`
	ContentType     string   `json:"contentType"`
	ContentEncoding string   `json:"contentEncoding,omitempty"`
	BodyFormat      string   `json:"bodyFormat"`
	BodyBytes       int      `json:"bodyBytes"`
	Events          []string `json:"events,omitempty"`
	TerminalEvent   string   `json:"terminalEvent,omitempty"`
	ErrorCode       string   `json:"errorCode,omitempty"`
	ErrorParam      string   `json:"errorParam,omitempty"`
	ErrorMessage    string   `json:"errorMessage,omitempty"`
	ValidationError string   `json:"validationError,omitempty"`
}

// describeResponse observes an already bounded provider body. It never copies
// output text, reasoning content, request input, headers or raw event payloads.
func describeResponse(resp *http.Response, body []byte, credential string) ResponseDiagnostic {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	d := ResponseDiagnostic{Kind: "model-provider-response", HTTPStatus: resp.StatusCode,
		BodyBytes: len(body), RequestID: diagnosticID(resp.Header.Get("X-Request-ID"), credential),
		ContentType: diagnosticText(resp.Header.Get("Content-Type"), credential), BodyFormat: "other"}
	if encoding := resp.Header.Get("Content-Encoding"); encoding == "gzip" || encoding == "br" || encoding == "zstd" {
		d.ContentEncoding = encoding
	}
	prefix := bytes.TrimSpace(body)
	switch {
	case bytes.HasPrefix(prefix, []byte("data:")), bytes.HasPrefix(prefix, []byte("event:")):
		d.BodyFormat = "sse"
	case bytes.HasPrefix(prefix, []byte("{")), bytes.HasPrefix(prefix, []byte("[")):
		d.BodyFormat = "json"
	case len(prefix) == 0:
		d.BodyFormat = "empty"
	}
	decode := func(raw []byte) {
		var event struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
			Error   *struct {
				Code    string
				Param   string
				Message string
			} `json:"error"`
			Response *struct {
				Error *struct {
					Code    string
					Param   string
					Message string
				}
			} `json:"response"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		kind := diagnosticID(event.Type, credential)
		if kind != "" && len(d.Events) < 16 {
			found := false
			for _, previous := range d.Events {
				if previous == kind {
					found = true
				}
			}
			if !found {
				d.Events = append(d.Events, kind)
			}
		}
		switch kind {
		case "response.completed", responseFailedEvent, responseIncompleteEvent, errorEventType:
			d.TerminalEvent = kind
		}
		if event.Response != nil && event.Response.Error != nil {
			event.Code, event.Param, event.Message = event.Response.Error.Code, event.Response.Error.Param, event.Response.Error.Message
		} else if event.Error != nil {
			event.Code, event.Param, event.Message = event.Error.Code, event.Error.Param, event.Error.Message
		} else if event.Detail != "" {
			event.Message = event.Detail
		}
		if kind == errorEventType || kind == responseFailedEvent || kind == responseIncompleteEvent || resp.StatusCode >= 400 {
			d.ErrorCode, d.ErrorParam = diagnosticID(event.Code, credential), diagnosticID(event.Param, credential)
			d.ErrorMessage = diagnosticText(event.Message, credential)
		}
	}
	if mediaType != eventStreamMediaType && d.BodyFormat != "sse" {
		decode(body)
		return d
	}
	for frame := range bytes.SplitSeq(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
		var data []string
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = append(data, strings.TrimPrefix(string(line[5:]), " "))
			}
		}
		if len(data) > 0 {
			decode([]byte(strings.Join(data, "\n")))
		}
	}
	return d
}
