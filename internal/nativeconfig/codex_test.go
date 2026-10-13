package nativeconfig

import (
	"strings"
	"testing"
)

func TestTrustedNativeSettings(t *testing.T) {
	for _, option := range []string{
		`projects."/workspace/session/workspace".trust_level="trusted"`,
		`projects."/workspace/session/workspace=a b".trust_level="trusted"`,
		`model_providers.mobility-fixture.base_url="http://fixture.invalid/v1"`,
		`mcp_servers."fixture-name".enabled=true`,
		`features.fixture={enabled=true, limit=2}`,
		`fixture="literal \\n and = inside a value"`,
		`fixture="` + strings.Repeat("x", 4086) + `"`,
	} {
		t.Run(option[:min(len(option), 80)], func(t *testing.T) {
			if err := Validate("fixture-model", []string{option}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := Validate("", nil); err != nil {
		t.Fatal(err)
	}
	if err := Validate(strings.Repeat("m", 128), makeOptions(32)); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSettingsCannotReplaceReservedKeys(t *testing.T) {
	for _, option := range []string{
		`developer_instructions="other"`,
		` developer_instructions="other"`,
		`developer_instructions ="other"`,
		`"developer_instructions"="other"`,
		`"developer\u005finstructions"="other"`,
		`developer_instructions.nested=true`,
		`"developer_instructions".nested=true`,
		`check_for_update_on_startup=true`,
		`"check_for_update_on_startup"=true`,
		`check_for_update_on_startup.nested=true`,
	} {
		t.Run(option, func(t *testing.T) {
			if err := Validate("", []string{option}); err == nil {
				t.Fatal("reserved native setting admitted")
			}
		})
	}
}

func TestMalformedOrUnboundedNativeSettingsFail(t *testing.T) {
	for _, option := range []string{
		"", "=true", "fixture", "fixture=", "fixture=  ", "fixture..x=true", ".fixture=true", "fixture.=true",
		"fixture .x=true", "fixture=true\nother=false", "fixture=true\rother=false", "fixture=\x00",
		`'fixture'=true`, `""=true`, `"fixture=true`, `"fixture\nkey"=true`, `"fixture\q"=true`,
		`fixture="` + strings.Repeat("x", 4087) + `"`,
	} {
		t.Run(option[:min(len(option), 80)], func(t *testing.T) {
			if err := Validate("", []string{option}); err == nil {
				t.Fatal("malformed or unbounded native setting admitted")
			}
		})
	}
	for _, model := range []string{strings.Repeat("m", 129), "model\nother", "model\rother", "model\x00"} {
		if err := Validate(model, nil); err == nil {
			t.Fatal("invalid native model admitted")
		}
	}
	if err := Validate("", makeOptions(33)); err == nil {
		t.Fatal("native option count limit bypassed")
	}
}

func makeOptions(count int) []string {
	options := make([]string, count)
	for i := range options {
		options[i] = "features.fixture=true"
	}
	return options
}
