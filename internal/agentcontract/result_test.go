package agentcontract

import (
	"strings"
	"testing"
)

func TestDecodeResultPreservesFinalTextAndEmptyAvailability(t *testing.T) {
	for _, tc := range []struct {
		document string
		want     Result
	}{
		{`{"version":"sproozi.run-result/v1","runUID":"run-123","result":{"text":"Repaired café ☕","truncated":false}}`, Result{Text: "Repaired café ☕"}},
		{`{"version":"sproozi.run-result/v1","runUID":"run-123","result":{"text":"","truncated":false}}`, Result{}},
		{`{"version":"sproozi.run-result/v1","runUID":"run-123","result":{"text":"Partial answer…","truncated":true}}`, Result{Text: "Partial answer…", Truncated: true}},
	} {
		got, err := DecodeResult([]byte(tc.document), "run-123")
		if err != nil || got != tc.want {
			t.Fatalf("DecodeResult = %#v, %v; want %#v", got, err, tc.want)
		}
	}
}

func TestDecodeResultRejectsUnavailableAndForeignEnvelopes(t *testing.T) {
	valid := `{"version":"sproozi.run-result/v1","runUID":"run-123","result":{"text":"answer","truncated":false}}`
	// The same boundary accepts the complete document before rejecting each defect.
	if got, err := DecodeResult([]byte(valid), "run-123"); err != nil || got.Text != "answer" {
		t.Fatalf("valid result = %#v, %v", got, err)
	}
	for name, document := range map[string]string{
		"absent":            "",
		"truncated JSON":    valid[:len(valid)-1],
		"extra document":    valid + `{}`,
		"foreign UID":       strings.Replace(valid, "run-123", "run-456", 1),
		"unknown version":   strings.Replace(valid, "sproozi.run-result/v1", "other/v1", 1),
		"missing result":    `{"version":"sproozi.run-result/v1","runUID":"run-123"}`,
		"null result":       `{"version":"sproozi.run-result/v1","runUID":"run-123","result":null}`,
		"missing text":      strings.Replace(valid, `"text":"answer",`, "", 1),
		"null text":         strings.Replace(valid, `"answer"`, "null", 1),
		"missing truncated": strings.Replace(valid, `,"truncated":false`, "", 1),
		"null truncated":    strings.Replace(valid, "false", "null", 1),
		"unknown outer":     strings.Replace(valid, `"result":`, `"phase":"Succeeded","result":`, 1),
		"unknown inner":     strings.Replace(valid, `"text":`, `"credential":"secret","text":`, 1),
		"duplicate outer":   strings.Replace(valid, `"runUID":`, `"runUID":"run-456","runUID":`, 1),
		"duplicate inner":   strings.Replace(valid, `"text":`, `"text":"other","text":`, 1),
		"oversized":         strings.Replace(valid, "answer", strings.Repeat("a", 4000), 1),
		"invalid UTF-8":     strings.Replace(valid, "answer", string([]byte{0xff}), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := DecodeResult([]byte(document), "run-123"); err == nil {
				t.Fatalf("accepted unavailable result %#v", got)
			}
		})
	}
	if _, err := DecodeResult([]byte(valid), ""); err == nil {
		t.Fatal("accepted missing expected UID")
	}
}
