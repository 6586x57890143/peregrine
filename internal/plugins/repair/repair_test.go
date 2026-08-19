package repair

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// TestABoundaryComesFromTheCorpusBeforeTheOverride.
//
// This is the whole point of M18 over M17. The stamp is the right answer and the operator's
// override is the fallback, not the other way round: a human's memory of a deploy time is what
// the generation stamp exists to stop depending on, and if the override won it would be
// depending on it forever.
func TestABoundaryComesFromTheCorpusBeforeTheOverride(t *testing.T) {
	set := dbtest.Set(t)
	s := dbtest.Guild(t, set, "111")

	stamped := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	override := time.Now().Add(-1 * time.Hour)

	if err := s.Update(func(w *storage.Writer) error {
		return w.RecordLearnGeneration(2, stamped)
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(nil, set, nil, Options{Override: override})
	got, err := svc.boundary(s, Job{Name: "associations", FixedIn: 2})
	if err != nil {
		t.Fatalf("boundary: %v", err)
	}
	if !got.Equal(stamped) {
		t.Errorf("boundary = %s, want the stamped value %s: the override won over what the "+
			"corpus recorded", got, stamped)
	}
}

// TestTheOverrideCoversAGenerationThatShippedBeforeStamping.
//
// Generation 2 shipped in M14 without a stamp, so its real start instant is unrecoverable.
// That one case is what the override is for, and it should not grow a second.
func TestTheOverrideCoversAGenerationThatShippedBeforeStamping(t *testing.T) {
	set := dbtest.Set(t)
	s := dbtest.Guild(t, set, "111")
	override := time.Now().Add(-24 * time.Hour).Truncate(time.Second)

	svc := New(nil, set, nil, Options{Override: override})
	got, err := svc.boundary(s, Job{Name: "associations", FixedIn: 2})
	if err != nil {
		t.Fatalf("boundary: %v", err)
	}
	if !got.Equal(override) {
		t.Errorf("boundary = %s, want the override %s", got, override)
	}
}

// TestAJobWithNoKnownBoundaryRefusesToRun.
//
// Declining is the safe direction, and both alternatives are silent failures: a repair that
// treated "unknown" as the zero time would walk nothing and report success, and one that
// treated it as "no bound" would walk everything and re-count data the fixed writer already
// wrote. The error has to name the variable that fixes it, because the operator cannot
// otherwise tell which of those two happened.
func TestAJobWithNoKnownBoundaryRefusesToRun(t *testing.T) {
	set := dbtest.Set(t)
	s := dbtest.Guild(t, set, "111")

	svc := New(nil, set, nil, Options{})
	_, err := svc.boundary(s, Job{Name: "associations", FixedIn: 2})
	if err == nil {
		t.Fatal("a job with no stamp and no override produced a boundary")
	}
	if !strings.Contains(err.Error(), "PEREGRINE_REPAIR_BEFORE") {
		t.Errorf("the error does not name the variable that fixes it: %v", err)
	}
}

// TestAFinishedJobIsNotRunAgain, which is what the completion marker is for: without it every
// restart would re-walk all of history.
func TestAFinishedJobIsNotRunAgain(t *testing.T) {
	set := dbtest.Set(t)
	s := dbtest.Guild(t, set, "111")

	if err := s.Update(func(w *storage.Writer) error {
		return w.SetRepairState("associations", storage.RepairDone)
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(nil, set, nil, Options{Enabled: []string{AllJobs}})
	svc.logger = quietLogger()

	if got := svc.pending(s); len(got) != 0 {
		t.Errorf("a job marked done is still pending: %v", got)
	}
}

// TestOnlyEnabledJobsArePending, since a repair re-reads the whole of history and running one
// the operator did not name is not a decision this service gets to make.
func TestOnlyEnabledJobsArePending(t *testing.T) {
	set := dbtest.Set(t)
	s := dbtest.Guild(t, set, "111")

	for _, c := range []struct {
		name    string
		enabled []string
		want    int
	}{
		{"nothing enabled", nil, 0},
		{"an unrelated job", []string{"something-else"}, 0},
		{"by name", []string{"associations"}, 1},
		{"by all", []string{AllJobs}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := New(nil, set, nil, Options{Enabled: c.enabled})
			svc.logger = quietLogger()
			if got := len(svc.pending(s)); got != c.want {
				t.Errorf("%d jobs pending, want %d", got, c.want)
			}
		})
	}
}

// quietLogger keeps test output readable. The service logs at Info on paths these tests
// exercise, and none of it is what is being asserted.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
