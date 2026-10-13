package cli

import "testing"

func TestCustodyCommandsArePipelineMutations(t *testing.T) {
	for _, path := range [][]string{{"custody", "validate"}, {"axi", "custody", "validate"}, {"custody", "release"}, {"custody", "rebind"}, {"custody", "reconcile"}, {"axi", "custody", "release"}, {"axi", "custody", "rebind"}, {"axi", "custody", "reconcile"}} {
		root := newRootCmd()
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if !mutatesPipelineControl(cmd) {
			t.Fatalf("unguarded %v", path)
		}
		if cmd.Flags().Lookup("run") != nil {
			t.Fatalf("custody mutation accepts observational run selector: %v", path)
		}
	}
}
