package maintenance

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// CorpusReport renders a whole-corpus measurement as text.
//
// # Why it prints rather than logs
//
// Every other mode here reports through slog, because what they emit is a record of
// something they DID. This did nothing: it is a measurement of a file, read once, by
// an operator who is about to paste it into a decision about a tuning constant. A
// structured log line per distribution would be unreadable in exactly the form it is
// going to be read in, and the destination is an io.Writer so a test can assert on the
// rendering without a handler.
//
// # It states magnitudes and never conclusions
//
// The temptation is to print "MIN_DISTINCT_AUTHORS should be 1". That would put a
// second opinion about the engine outside internal/markov, where it could disagree
// with the scorer, which is finding 28's shape. What it does instead is print the
// admission curve at every candidate value so the operator can see the cliff, and name
// which value is configured today.
func CorpusReport(st corpus.Stats, minAuthors int, w io.Writer) error {
	p := &printer{w: w}

	p.section("Scale")
	p.line("messages learned", "%d", st.Learned)
	p.line("edges (prefix -> next)", "%d", st.Edges)
	p.line("distinct prefixes", "%d", st.Prefixes)
	p.line("vocabulary", "%d", st.Vocabulary)
	p.line("names", "%d", st.Names)
	p.line("word-word associations", "%d", st.TopicWords)
	p.line("name-word associations", "%d", st.NameTopics)
	p.line("total tokens learned", "%d", st.TotalTokens)
	p.line("total edge mass", "%d", st.TotalEdgeMass)
	// Called out because it is not a word and it is the largest topic entry there is,
	// so the distributions below all have it as their maximum. Without this line the
	// report reads as though something is double-counting messages.
	p.line("end sentinel (not a word)", "%d  (%.1f%% of all tokens)",
		st.SentinelCount, pctU64(st.SentinelCount, st.TotalTokens))

	// The magnitude that decides whether a tanh saturates. A term squashed at
	// tanh(x/4) is a grading function at x=1 and an indicator at x=40, and which one it
	// is cannot be read off the formula without knowing what x is in a real corpus.
	p.section("Word occurrence counts (topic bucket)")
	p.quantiles(st.TopicCounts)

	p.section("Association counts (word, associate)")
	p.quantiles(st.AssocCounts)

	p.section("Associates per word")
	p.quantilesInt(st.AssocPerWord)

	p.section("Successors per prefix (ungated)")
	p.quantilesInt(st.SuccessorCounts)

	p.section("Distinct authors per edge")
	total := 0
	for _, n := range st.Authors {
		total += n
	}
	for i, n := range st.Authors {
		label := fmt.Sprintf("%d authors", i)
		switch i {
		case 0:
			// Not "unattributed by accident": learnMessage passes an empty author for
			// the bot's own output, precisely so self-learning cannot bootstrap a
			// phrase into eligibility.
			label = "0 (bot's own output)"
		case 1:
			label = "1 author"
		case corpus.AuthorHistogramMax:
			label = fmt.Sprintf("%d+ authors", i)
		}
		p.line(label, "%d  (%.2f%%)", n, pct(n, total))
	}

	p.section("Single-author edges by occurrence count")
	p.note("An edge one person said once is a sparse corpus. An edge one person said a")
	p.note("hundred times is the poisoning shape section 4 A6 describes. Today's gate")
	p.note("cannot tell them apart, and this is the joint distribution that could.")
	for i, threshold := range corpus.CountThresholds {
		p.line(fmt.Sprintf("count >= %d", threshold), "%d", st.SingleAuthorByCount[i])
	}

	p.section("Admission by MIN_DISTINCT_AUTHORS")
	p.note("Mass share is the one that predicts behaviour: generation samples in")
	p.note("proportion to probability, so refusing 86% of edges and refusing 86% of the")
	p.note("mass are very different outcomes.")
	for _, a := range st.Admission {
		marker := ""
		if a.MinAuthors == minAuthors {
			marker = "   <- configured"
		}
		p.line(fmt.Sprintf("k = %d", a.MinAuthors), "%d edges (%.2f%%), mass %d (%.2f%%)%s",
			a.Edges, a.EdgeShare*100, a.Mass, a.MassShare*100, marker)
	}

	p.section("Branch factor by prefix order")
	p.note("A gated mean at or near 1 is a deterministic walk however hot the sampler is,")
	p.note("and that is invisible in the output because the output still varies at the seed.")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	table := &printer{w: tw}
	table.printf("  order\tprefixes\tedges\tmean succ\tmedian\tmean gated\tdead prefixes\n")
	for _, o := range st.Orders {
		table.printf("  %d\t%d\t%d\t%.2f\t%d\t%.2f\t%.2f%%\n",
			o.Order, o.Prefixes, o.Edges, o.MeanSuccessors, o.MedianSucc,
			o.MeanGatedSucc, o.DeadPrefixRate*100)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	return errors.Join(p.err, table.err)
}

// printer keeps the first write error rather than checking every Fprintf, which is the
// only thing that would otherwise dominate this file.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) section(title string) {
	p.printf("\n%s\n", title)
	p.printf("%s\n", underline(len(title)))
}

func (p *printer) line(label, format string, args ...any) {
	p.printf("  %-34s %s\n", label, fmt.Sprintf(format, args...))
}

func (p *printer) note(s string) { p.printf("  # %s\n", s) }

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

// quantiles prints the shape of a distribution rather than its histogram, because the
// question every one of these answers is "what magnitude is a typical value", and a
// mean alone hides the tail that a saturating term lives in.
func (p *printer) quantiles(sorted []uint64) {
	if len(sorted) == 0 {
		p.line("(empty)", "")
		return
	}
	p.line("n", "%d", len(sorted))
	p.line("mean", "%.2f", corpus.Mean(sorted))
	for _, q := range []float64{0.5, 0.9, 0.99} {
		p.line(fmt.Sprintf("p%g", q*100), "%d", corpus.Percentile(sorted, q))
	}
	p.line("max", "%d", sorted[len(sorted)-1])
}

func (p *printer) quantilesInt(sorted []int) {
	if len(sorted) == 0 {
		p.line("(empty)", "")
		return
	}
	p.line("n", "%d", len(sorted))
	p.line("mean", "%.2f", corpus.Mean(sorted))
	for _, q := range []float64{0.5, 0.9, 0.99} {
		p.line(fmt.Sprintf("p%g", q*100), "%d", corpus.Percentile(sorted, q))
	}
	p.line("max", "%d", sorted[len(sorted)-1])
}

func underline(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func pctU64(n, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
