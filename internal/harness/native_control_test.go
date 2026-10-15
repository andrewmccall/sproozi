package harness

import (
	"strings"
	"testing"
)

func TestNativeReplyBindsOperationStateAndThread(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	const valid = `{"op":"activate","state":"active","threadId":"12345678-1234-1234-1234-123456789abc","pid":42}`
	reply, err := DecodeNativeReply([]byte(valid), "activate", "active", thread)
	if err != nil || reply.PID != 42 || reply.ThreadID != thread {
		t.Fatalf("reply=%+v error=%v", reply, err)
	}
	for _, data := range []string{
		strings.Replace(valid, `"activate"`, `"status"`, 1),
		strings.Replace(valid, `"active"`, `"prepared"`, 1),
		strings.Replace(valid, thread, "22345678-1234-1234-1234-123456789abc", 1),
		strings.Replace(valid, `"pid":42`, `"error":"failed"`, 1),
		strings.Replace(valid, `"pid":42`, `"unknown":42`, 1),
		valid + `{}`, `null`, strings.Repeat(" ", 64<<10) + valid,
	} {
		if _, err := DecodeNativeReply([]byte(data), "activate", "active", thread); err == nil {
			t.Fatalf("invalid reply accepted: %.200s", data)
		}
	}
}

func TestInitialNativeReplyBindsObservedIdentityOnce(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	const empty = `{"op":"status","state":"active"}`
	const observed = `{"op":"status","state":"active","threadId":"12345678-1234-1234-1234-123456789abc"}`
	for _, tc := range []struct {
		data, bound, want string
		ok                bool
	}{
		{empty, "", "", true}, {observed, "", thread, true}, {observed, thread, thread, true},
		{empty, thread, "", false}, {observed, "22345678-1234-1234-1234-123456789abc", "", false},
		{strings.Replace(observed, thread, "not-a-native-id", 1), "", "", false},
	} {
		reply, err := DecodeInitialNativeReply([]byte(tc.data), "status", "active", tc.bound)
		if (err == nil) != tc.ok || err == nil && reply.ThreadID != tc.want {
			t.Fatalf("bound=%q reply=%+v error=%v", tc.bound, reply, err)
		}
	}
}
