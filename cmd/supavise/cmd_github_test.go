package main

import "testing"

// The commands that set up the server's GitHub App exist and take the flags the guide documents.
func TestGitHubAppCommands(t *testing.T) {
	for _, tc := range []struct {
		path  []string
		flags []string
	}{
		{[]string{"github", "app", "create"}, []string{"owner", "name", "replace"}},
		{[]string{"github", "app", "status"}, []string{"json"}},
		{[]string{"github", "app", "remove"}, nil},
	} {
		cmd, _, err := rootCmd.Find(tc.path)
		if err != nil || cmd == nil || cmd.Name() != tc.path[len(tc.path)-1] {
			t.Fatalf("%v: not a command (%v)", tc.path, err)
		}
		for _, f := range tc.flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("%v has no --%s", tc.path, f)
			}
		}
		if cmd.Long == "" && cmd.Short == "" {
			t.Errorf("%v has no help", tc.path)
		}
	}
}
