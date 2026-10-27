package config

import (
	"strings"
	"testing"
)

// Comment lines may DOCUMENT the expansion syntax without the operator
// having those variables set — the shipped template's header says
// ${ENV_VAR} and must not cost a fresh install its startup (§9.12).
// Values still expand, and unset ${VAR} in a value still fails loudly.
func TestExpandEnvCommentsPassThrough(t *testing.T) {
	t.Setenv("GABKEEP_TEST_PRESENT", "hunter2")

	out, err := expandEnv("# like ${TOTALLY_UNSET} or ${GABKEEP_TEST_PRESENT}\n" +
		"  # indented ${TOTALLY_UNSET:-fallback} too\n" +
		"key: ${GABKEEP_TEST_PRESENT}\n" +
		"fallback: ${TOTALLY_UNSET:-dflt}\n")
	if err != nil {
		t.Fatalf("expandEnv with unset var in comment: %v", err)
	}
	want := "key: hunter2"
	if !strings.Contains(out, want) {
		t.Errorf("value expansion = %q, want it to contain %q", out, want)
	}
	if !strings.Contains(out, "fallback: dflt") {
		t.Errorf("default-value expansion = %q", out)
	}
	if !strings.Contains(out, "${TOTALLY_UNSET} or") {
		t.Errorf("comment was rewritten: %q", out)
	}

	if _, err := expandEnv("key: ${TOTALLY_UNSET}\n"); err == nil {
		t.Error("unset ${VAR} in a value must still fail")
	} else if !strings.Contains(err.Error(), "TOTALLY_UNSET") {
		t.Errorf("error = %v, want it to name the variable", err)
	}
}
