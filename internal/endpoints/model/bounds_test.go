package model

import (
	"strings"
	"testing"
)

func TestReadBoundedAcceptsLimit(t *testing.T) {
	data, err := readBounded(strings.NewReader("1234"), 4)
	if err != nil || string(data) != "1234" {
		t.Fatalf("readBounded() = %q, %v", data, err)
	}
}

func TestReadBoundedRejectsOversize(t *testing.T) {
	if _, err := readBounded(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("expected oversized body to be rejected")
	}
}
