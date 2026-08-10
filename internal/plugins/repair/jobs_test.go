package repair

import (
	"testing"

	"github.com/6586x57890143/peregrine/internal/learn"
)

// These three are the whole reason the job table is worth having over a cloned service: they
// turn "add a repair" into a reviewable table row whose mistakes are caught mechanically.
//
// Two dead seed tiers have already shipped in this repository (findings 34 and 37), both found
// by reading constants against each other rather than by a failing test, and both because a
// new entry duplicated something an existing entry already covered. A table without tests that
// check its entries against each other has the same hole.

func testJobs(t *testing.T) []Job {
	t.Helper()
	all := jobs(nil)
	if len(all) == 0 {
		t.Fatal("no repair jobs are registered, so every test here would pass vacuously")
	}
	return all
}

// TestEveryJobHasADistinctName.
//
// The name keys TWO pieces of state: the completion marker in meta and the cursor namespace.
// A collision would make two repairs share a cursor, so the second would skip every message
// the first had already walked and report success having repaired nothing.
func TestEveryJobHasADistinctName(t *testing.T) {
	seen := map[string]bool{}
	for _, j := range testJobs(t) {
		if j.Name == "" {
			t.Error("a job has an empty name, which would key its state on the empty string")
		}
		if j.Name == AllJobs {
			t.Errorf("a job is named %q, which is the value that means every job", AllJobs)
		}
		if seen[j.Name] {
			t.Errorf("two jobs are named %q, so they would share a cursor and a done marker", j.Name)
		}
		seen[j.Name] = true
	}
}

// TestEveryJobNamesAGenerationThatExists.
//
// FixedIn points at the learn generation that corrected the writer, and the boundary is when
// that generation first ran. A job pointing past learn.Generation names a fix that has not
// shipped, so its boundary can never be known and it would decline forever while looking
// enabled.
func TestEveryJobNamesAGenerationThatExists(t *testing.T) {
	for _, j := range testJobs(t) {
		if j.FixedIn <= 0 {
			t.Errorf("job %q has FixedIn=%d, which is not a generation", j.Name, j.FixedIn)
		}
		if j.FixedIn > learn.Generation {
			t.Errorf("job %q repairs up to generation %d but the write path is only at %d: "+
				"either the generation constant was not bumped with the fix, or the job names "+
				"a fix that has not shipped", j.Name, j.FixedIn, learn.Generation)
		}
	}
}

// TestEveryJobExplainsItself.
//
// A pass that re-reads all of Discord history should say what it is for in the line it logs
// when it starts, because that log line is the only thing an operator has to judge whether the
// REST budget it is about to spend is worth spending.
func TestEveryJobExplainsItself(t *testing.T) {
	for _, j := range testJobs(t) {
		if j.Why == "" {
			t.Errorf("job %q has no Why, so it starts a full history walk without saying why", j.Name)
		}
		if j.Apply == nil {
			t.Errorf("job %q has no Apply, so it would walk history and write nothing", j.Name)
		}
	}
}

// TestJobNamesMatchTheTable, since the config error that refuses an unknown job name is built
// from JobNames and would otherwise be able to drift from the table it describes.
func TestJobNamesMatchTheTable(t *testing.T) {
	all := testJobs(t)
	names := JobNames(nil)
	if len(names) != len(all) {
		t.Fatalf("JobNames returned %d entries for %d jobs", len(names), len(all))
	}
	for i, j := range all {
		if names[i] != j.Name {
			t.Errorf("JobNames[%d] = %q, want %q", i, names[i], j.Name)
		}
	}
}
