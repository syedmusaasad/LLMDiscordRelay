// refusal reads a captured tmux pane on stdin and exits 0 when the relay's own
// pane parser (inject.IsResumeRefused) classifies it as a refused `--resume`.
// It is a helper of test/live/claude_smoke.sh.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/inject"
)

func main() {
	pane, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if inject.IsResumeRefused(string(pane)) {
		return
	}
	os.Exit(1)
}
