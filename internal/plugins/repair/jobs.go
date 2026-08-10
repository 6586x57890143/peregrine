package repair

import (
	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// AllJobs is the value that enables every known job, so an operator repairing a corpus after a
// long gap does not have to name each one and discover the list by reading source.
const AllJobs = "all"

// Job is one repair: what it is called, which generation of the write path fixed the bug, and
// what it writes for a message that predates that fix.
//
// A TABLE rather than a plugin per repair, mirroring the chat reactor's step table, which is
// this repository's established shape for a list of named units each carrying a function. The
// alternative is cloning a service, and the M17 version of this file was that clone: a whole
// core.Service, two adapters, two storage accessors and two config variables, all naming one
// index. The second repair would have doubled it.
type Job struct {
	// Name keys the completion marker and the cursor namespace.
	//
	// RENAMING A JOB RESTARTS IT, which is deliberate rather than a hazard: a renamed job is
	// a different question about the corpus, and silently inheriting the old job's "done"
	// marker would mean the new question never gets asked.
	Name string

	// FixedIn is the learn generation that corrected the writer.
	//
	// The boundary is when that generation FIRST RAN here, which the corpus records: every
	// message older than it went through the broken path and needs repairing, and everything
	// newer already went through the fixed one. That is what makes a repair additive rather
	// than a destructive rebuild, and it is why the boundary is not an operator's memory.
	FixedIn int

	// Why is the one-line reason this job exists, for the log line an operator reads when it
	// starts. A pass that re-reads all of history should say what it is for.
	Why string

	// Apply repairs one historical message.
	//
	// It must write ONLY what its own index needs. Re-reading history through the ordinary
	// learn path would count every n-gram a second time, which is finding 13, so a job that
	// reaches for Learner.Message is a bug rather than a shortcut.
	//
	// It must also be SAFE TO RUN against a message that was already correct, because the
	// boundary is approximate whenever it comes from the operator override rather than from a
	// generation stamp.
	Apply func(w *storage.Writer, m *discordgo.Message, author names.User, mentioned []names.User) error
}

// jobs is every repair this binary knows how to perform.
//
// One entry today. That is not an argument against the table: both of the shapes it replaces
// (a cloned service, or a switch in the caller) cost more at two entries than this costs at
// one, and the tests below turn "add a repair" into a reviewable table row rather than a new
// package. The same was true of the chat step table when it had three steps.
func jobs(l *learn.Learner) []Job {
	return []Job{
		{
			Name:    "associations",
			FixedIn: 2,
			Why:     "the backfill wrote no co-occurrence associations at all before M14 (findings 33 and 46)",
			Apply: func(w *storage.Writer, m *discordgo.Message, author names.User, mentioned []names.User) error {
				return l.Associations(w, m.Content, author, mentioned)
			},
		},
	}
}

// JobNames lists the known job names, for the config error that refuses an unknown one.
func JobNames(l *learn.Learner) []string {
	all := jobs(l)
	out := make([]string, 0, len(all))
	for _, j := range all {
		out = append(out, j.Name)
	}
	return out
}
