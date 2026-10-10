package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plantedSecret is the value of an allowlisted variable the fuzzed documents may
// reference. A load error must never carry it, whatever the document holds.
const plantedSecret = "planted-fuzz-secret-value"

// The other planted secrets parse: as a duration over the ceiling, a lookback
// short of a 1h interval and a level above info, so the documents referencing
// them reach the clamp, lookback and level diagnostics. A *Shown constant is
// the form such a diagnostic would print the secret in.
const (
	plantedDuration      = "999h"
	plantedDurationShown = "999h0m0s"
	plantedShort         = "3h7m11s"
	plantedLevel         = "Error+5"
	plantedLevelShown    = "ERROR+5"
)

// FuzzLoad drives the config decode, the untrusted boundary an operator's file
// crosses: no input panics, and no error or warning carries an expanded secret.
func FuzzLoad(f *testing.F) {
	f.Add(minimal)
	f.Add("connections:\n  - name: a\n    url: https://github.com\n    token: ${FORGE_SCOUT_FUZZ_TOKEN}\n    owners: [o]\n")
	f.Add("connections:\n  - name: a\n    url: https://github.com\n    token: a${FORGE_SCOUT_FUZZ_TOKEN}\n    owners: [o]\n")
	f.Add("connections:\n  - name: a\n    url: ${FORGE_SCOUT_FUZZ_TOKEN}\n    token: x\n    owners: [o]\n")
	f.Add("scan_interval: ${FORGE_SCOUT_FUZZ_TOKEN}\n" + minimal)
	f.Add("scan_interval: ${FORGE_SCOUT_FUZZ_DURATION}\nlookback: 720h\n" + minimal)
	f.Add("lookback: ${FORGE_SCOUT_FUZZ_DURATION}\n" + minimal)
	f.Add("scan_interval: 1h\nlookback: ${FORGE_SCOUT_FUZZ_SHORT}\n" + minimal)
	f.Add("log_level: ${FORGE_SCOUT_FUZZ_LEVEL}\n" + minimal)
	f.Add("connections:\n  - name: ${FORGE_SCOUT_FUZZ_TOKEN}\n    url: https://github.com\n    token: x\n    owners: [${FORGE_SCOUT_FUZZ_TOKEN}]\n")
	f.Add("connections:\n  - name: ${GITHUB_TOKEN}")
	f.Add("connections: [1, 2]\n")
	f.Add("a: &x [*x]\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, doc string) {
		t.Setenv("FORGE_SCOUT_FUZZ_TOKEN", plantedSecret)
		t.Setenv("GITHUB_TOKEN", plantedSecret)
		t.Setenv("FORGE_SCOUT_FUZZ_DURATION", plantedDuration)
		t.Setenv("FORGE_SCOUT_FUZZ_SHORT", plantedShort)
		t.Setenv("FORGE_SCOUT_FUZZ_LEVEL", plantedLevel)
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("Setup: %v", err)
		}
		_, warns, err := Load(path)
		for _, v := range []string{plantedSecret, plantedDuration, plantedDurationShown, plantedShort, plantedLevel, plantedLevelShown} {
			if strings.Contains(strings.ToLower(doc), strings.ToLower(v)) {
				continue
			}
			if err != nil && strings.Contains(err.Error(), v) {
				t.Fatalf("Load error carries the planted secret %q: %q", v, err)
			}
			for _, w := range warns {
				for _, a := range w.Attrs {
					if strings.Contains(a.Value.String(), v) {
						t.Fatalf("Load warning %q carries the planted secret %q in %s", w.Msg, v, a.Key)
					}
				}
			}
		}
	})
}
