package attach

import (
	"strings"
	"testing"
)

func TestParseArgsFilterStatsIsHidden(t *testing.T) {
	cfg, err := ParseArgs([]string{"--filter-stats", "--terminal", "0123456789abcdef", "--config", "/tmp/x/config.json"})
	if err != nil || !cfg.FilterStats || cfg.Terminal != "0123456789abcdef" {
		t.Fatal(cfg, err)
	}
	if cfg, err = ParseArgs([]string{"--config", "/tmp/x/config.json"}); err != nil || cfg.FilterStats {
		t.Fatal(cfg, err)
	}
	if strings.Contains(attachUsage, "filter-stats") {
		t.Fatal("the usage names a hidden flag")
	}
}
