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

func TestRootCommandHasSplitFlag(t *testing.T) {
	root := newRootCmd()

	if flag := root.PersistentFlags().Lookup("split"); flag == nil {
		t.Fatalf("split flag not registered")
	}
}

func TestRootCommandHasDeleteSourceFlag(t *testing.T) {
	root := newRootCmd()

	if flag := root.PersistentFlags().Lookup("delete-source"); flag == nil {
		t.Fatalf("delete-source flag not registered")
	}
}

func TestRootCommandReportFlagDefaultsToEnabled(t *testing.T) {
	root := newRootCmd()
	flag := root.PersistentFlags().Lookup("report")
	if flag == nil {
		t.Fatal("report flag not registered")
	}
	if flag.DefValue != "true" {
		t.Fatalf("report default = %q, want true", flag.DefValue)
	}
}

func TestRootCommandHasWatchAndInstallServiceSubcommands(t *testing.T) {
	root := newRootCmd()

	for _, name := range []string{"watch", "install-service"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("Find %s returned error: %v", name, err)
		}
		if cmd == root || cmd.Name() != name {
			t.Fatalf("%s subcommand not registered", name)
		}
	}
}
