package profile

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// An owner attachment sends the session's envelope, then the caller's frames,
// and returns the output frames once the executor admits it.
func TestOwnerAttachmentSendsTheEnvelopeAndStreamsFrames(t *testing.T) {
	got := make(chan []string, 1)
	s, ca := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/work/attach" || r.Header.Get(headerAuthorization) != bearerPrefix+testToken ||
			r.Header.Get(headerContentType) != jsonMedia {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if !FullDuplex(w) {
			t.Error("server is not full duplex")
		}
		reader := bufio.NewReader(r.Body)
		envelope, err := EnvelopeLine(reader)
		if err != nil {
			t.Error(err)
		}
		output, err := OpenTerminalOutput(w)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(output, "{\"op\":\"attach\",\"state\":\"active\"}\n")
		frames, _ := io.ReadAll(NewFrameReader(reader))
		got <- []string{string(envelope), string(frames)}
		_, _ = io.WriteString(output, "{\"op\":\"output\",\"data\":\"aGk=\"}\n")
	}))
	c, err := testClient(s.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := "4215d7d1-68ff-47ed-9dad-dbf1c8317d38"
	frames := strings.NewReader("{\"op\":\"write\",\"data\":\"aGk=\"}\n")
	replies, err := c.OpenOwnerAttachment(context.Background(), id, frames)
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(replies)
	_ = replies.Close()
	if err != nil {
		t.Fatal(err)
	}
	sent := <-got
	if sent[0] != `{"id":"`+id+"\"}\n" || sent[1] != "{\"op\":\"write\",\"data\":\"aGk=\"}\n" {
		t.Fatalf("sent %q", sent)
	}
	if !bytes.Contains(output, []byte(`"state":"active"`)) || !bytes.Contains(output, []byte(`"data":"aGk="`)) {
		t.Fatalf("output %q", output)
	}
}

// A refused attachment is the executor's outcome code; a plain 404 from an
// executor that has no attach route is unavailable with that status.
func TestOwnerAttachmentRefusalsAreOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   Code
	}{
		{http.StatusConflict, `{"error":"conflict"}`, CodeConflict},
		{http.StatusUnprocessableEntity, `{"error":"unsupported"}`, CodeUnsupported},
		{http.StatusNotFound, "404 page not found", CodeUnavailable},
	} {
		s, ca := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(headerContentType, jsonMedia)
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		c, err := testClient(s.URL, ca)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.OpenOwnerAttachment(context.Background(), "4215d7d1-68ff-47ed-9dad-dbf1c8317d38",
			strings.NewReader(""))
		var remote *RemoteError
		if !errors.As(err, &remote) || remote.Code != tc.code || remote.Status != tc.status {
			t.Fatalf("status %d: %v", tc.status, err)
		}
		c.Close()
	}
}

// A busy terminal's write acknowledgement carries an error the reader retries;
// it does not end the attachment. A failed attachment does.
func TestTerminalOutputKeepsBusyWriteAcknowledgements(t *testing.T) {
	var out bytes.Buffer
	input := "{\"op\":\"write\",\"written\":0,\"error\":\"native terminal input buffer is full\"}\n" +
		"{\"op\":\"output\",\"data\":\"aGk=\"}\n"
	if err := CopyTerminalOutput(strings.NewReader(input), &out); err != nil || out.String() != input {
		t.Fatalf("busy acknowledgement ended the attachment: %v %q", err, out.String())
	}
	if err := CopyTerminalOutput(strings.NewReader("{\"op\":\"attach\",\"error\":\"attachment unavailable\"}\n"),
		io.Discard); err == nil {
		t.Fatal("failed attachment accepted")
	}
}

// A home submission's body may carry an uploaded workspace as large as the
// executor's checkpoint limit, base64 in JSON.
func TestOwnerRunCarriesAnUploadUpToTheCheckpointLimit(t *testing.T) {
	var received int
	s, ca := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		received = len(data)
		w.Header().Set(headerContentType, jsonMedia)
		_, _ = io.WriteString(w, "{}")
	}))
	pemData, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pemData)
	c, err := Connect(Connection{Origin: s.URL, Token: testToken, Roots: roots, MaxCheckpointBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	upload := struct {
		Workspace []byte `json:"workspace"`
	}{Workspace: bytes.Repeat([]byte{1}, 1<<20)}
	var out json.RawMessage
	if err = c.CallOwner(context.Background(), "run", upload, &out); err != nil || received < 1<<20 {
		t.Fatalf("upload refused: %v (%d bytes)", err, received)
	}
	upload.Workspace = append(upload.Workspace, bytes.Repeat([]byte{1}, 1<<20)...)
	if err = c.CallOwner(context.Background(), "run", upload, &out); err == nil {
		t.Fatal("an upload over the checkpoint limit was sent")
	}
}
