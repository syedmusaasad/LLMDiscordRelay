package tmuxctl

import (
	"fmt"
)

// KeyRecipe is the exact ordered sequence of tmux key names sent to a Claude
// Code pane.
type KeyRecipe []string

// ApprovalApproveRecipe answers a permission prompt with option 1 ("Yes").
// verified against Claude Code 2.1.319
var ApprovalApproveRecipe = KeyRecipe{"1"}

// ApprovalDenyRecipe cancels a permission prompt. Escape rejects the tool use;
// pressing the "No" digit would also interrupt the turn.
// verified against Claude Code 2.1.319
var ApprovalDenyRecipe = KeyRecipe{"Escape"}

// TrustAcceptRecipe answers the folder-trust dialog: the default is "No, exit",
// so move down to "Yes, I trust this folder" and confirm.
// verified against Claude Code 2.1.319
var TrustAcceptRecipe = KeyRecipe{"Down", "Enter"}

// TrustDenyRecipe dismisses the folder-trust dialog without trusting.
// verified against Claude Code 2.1.319
var TrustDenyRecipe = KeyRecipe{"Escape"}

// QuestionSelectRecipe moves from the first option of an AskUserQuestion picker
// to the wanted one (Down repeated) and confirms with Enter.
// verified against Claude Code 2.1.319
var QuestionSelectRecipe = KeyRecipe{"Down", "Enter"}

// SendRecipe sends each named tmux key in order. Recipes deliberately use named
// keys rather than literal escape bytes so tmux preserves terminal semantics.
func SendRecipe(target string, recipe KeyRecipe) error {
	for _, key := range recipe {
		cmd, err := NewCommand("send-keys", "-t", target, key)
		if err != nil {
			return err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmuxctl: send key %q: %w: %s", key, err, out)
		}
	}
	return nil
}

// QuestionRecipe returns the keys that select the zero-based option: Down once
// per index, then Enter.
func QuestionRecipe(option int) (KeyRecipe, error) {
	if option < 0 {
		return nil, fmt.Errorf("tmuxctl: negative question option %d", option)
	}
	recipe := make(KeyRecipe, 0, option+1)
	for i := 0; i < option; i++ {
		recipe = append(recipe, QuestionSelectRecipe[0])
	}
	return append(recipe, QuestionSelectRecipe[len(QuestionSelectRecipe)-1]), nil
}

// SelectQuestion sends the picker recipe for the zero-based option index.
func SelectQuestion(target string, option int) error {
	recipe, err := QuestionRecipe(option)
	if err != nil {
		return err
	}
	return SendRecipe(target, recipe)
}
