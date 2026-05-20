package cmd

import "testing"

func TestRootCommandHasThumbnailSubcommand(t *testing.T) {
	root := newRootCmd()

	cmd, _, err := root.Find([]string{"thumbnail"})
	if err != nil {
		t.Fatalf("Find thumbnail returned error: %v", err)
	}
	if cmd == root || cmd.Name() != "thumbnail" {
		t.Fatalf("thumbnail subcommand not registered")
	}
}
