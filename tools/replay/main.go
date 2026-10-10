// Command replay regenerates the replies in a tuning archive against a corpus snapshot.
//
//	go run ./tools/replay -db ./snapshot.db -tuning ./tuning [-n 200] [-seed 1] [-cpuprofile cpu.out]
//
// It exists because the golden harness runs against a 150-line synthetic fixture, and
// SPEC.md section 10 keeps saying the same thing: revisit against real ingested text. This
// is that: the prompts people actually sent, the corpus the bot actually had, and a seeded
// source so two runs differ only by the change between them. It prints each prompt with
// the reply the bot sent then and the one it would send now, plus the latency spread.
//
// The corpus opens read-only, so a snapshot pulled off the host is the intended input and
// the file is not modified. Conversation memory is replayed per channel in archive order,
// so the steering terms see roughly what production saw.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"time"

	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/text"
	"github.com/6586x57890143/peregrine/internal/tuning"
)

func main() {
	db := flag.String("db", "", "corpus snapshot (opened read-only)")
	dir := flag.String("tuning", "", "directory of tuning JSONL files")
	n := flag.Int("n", 200, "replay at most the last n reply samples")
	seed := flag.Uint64("seed", 1, "PCG seed, so two runs are comparable")
	cpu := flag.String("cpuprofile", "", "write a CPU profile here")
	quiet := flag.Bool("q", false, "print only the summary")
	misses := flag.Bool("misses", false, "print every unattested trigram in the new replies")
	flag.Parse()
	if err := run(*db, *dir, *n, *seed, *cpu, *quiet, *misses); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(db, dir string, n int, seed uint64, cpu string, quiet, misses bool) error {
	samples, err := load(dir)
	if err != nil {
		return err
	}
	if len(samples) > n {
		samples = samples[len(samples)-n:]
	}

	store, err := storage.OpenReadOnly(db)
	if err != nil {
		return err
	}
	defer store.Close()

	// The production dials, as the most recent tuning snapshot reported them.
	opts := generate.Options{
		MaxNGram: 4, MinWords: 4, MaxWords: 12, Temperature: 1, TopK: 40, TopP: 0.95,
		KNDiscount: 0.75, KNRawMix: 0.25, MinDistinctAuthors: 2, SoloRepeatLimit: 2,
		SoloMaxOrder: 2, PromptRelevance: 0.6, RoastChance: 0.1,
	}
	gen := generate.NewWithSource(storage.Single(store), opts, rand.New(rand.NewPCG(seed, seed)))
	mem := generate.NewMemories(200)

	if cpu != "" {
		f, err := os.Create(cpu)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}

	took := make([]time.Duration, 0, len(samples))
	var now, then coherence
	if misses {
		now.misses = os.Stdout
	}
	words, produced := 0, 0
	for _, s := range samples {
		m := mem.For(s.Channel)
		start := time.Now()
		reply, outcome, err := gen.Sentence(generate.Request{
			GuildID: "replay", Prompt: s.Prompt, Roast: s.Roast, Memory: m,
		})
		took = append(took, time.Since(start))
		if err != nil {
			return err
		}
		m.Add(s.Prompt, nil)
		if reply != "" {
			m.Add(reply, nil)
		}
		if err := store.View(func(r *storage.Reader) error {
			now.add(r, reply)
			then.add(r, s.Reply)
			return nil
		}); err != nil {
			return err
		}
		if reply != "" {
			produced++
			words += len(strings.Fields(reply))
		}
		if !quiet {
			fmt.Printf("%6dms  %q\n    then: %q\n    now:  %q (%s)\n",
				took[len(took)-1].Milliseconds(), s.Prompt, s.Reply, reply, outcome)
		}
	}

	fmt.Printf("\nthen: %v\nnow:  %v\n", then, now)
	if produced > 0 {
		fmt.Printf("now produced %d of %d, mean %.1f words\n", produced, len(samples), float64(words)/float64(produced))
	}

	slices.Sort(took)
	pct := func(p float64) time.Duration { return took[int(p*float64(len(took)-1))] }
	fmt.Printf("%d replies: p50 %v  p90 %v  max %v\n", len(took),
		pct(0.5).Round(time.Millisecond), pct(0.9).Round(time.Millisecond), took[len(took)-1].Round(time.Millisecond))
	return nil
}

// load reads every reply Sample with a prompt, oldest first. Undecodable lines are skipped
// for the reason -tuning-report skips them: the last line of a file copied off a running
// host is routinely half-written.
func load(dir string) ([]tuning.Sample, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var out []tuning.Sample
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var s tuning.Sample
			if json.Unmarshal(sc.Bytes(), &s) != nil || s.Kind != tuning.KindSample {
				continue
			}
			if s.Trigger == "reply" && s.Prompt != "" {
				out = append(out, s)
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// coherence is the share of a reply's word pairs and triples the corpus has seen, and of
// its four-word windows: the first two rise as joins get less arbitrary, and the third is
// the recitation check, because a sentence copied whole from one message attests
// everything. Both directions have to be read together.
type coherence struct {
	bi, tri, quad, biN, triN, quadN int

	// misses, when set, receives every unattested trigram with its reply, which is how
	// the share above gets explained rather than just reported.
	misses io.Writer
}

func (c *coherence) add(r *storage.Reader, reply string) {
	words := text.Tokenize(reply)
	for i := range words {
		words[i] = text.LowerExceptURLs(words[i])
	}
	seen := func(prefix []string, next string) bool {
		_, ok, err := r.Successor(strings.Join(prefix, " "), next)
		return err == nil && ok
	}
	for i := 1; i < len(words); i++ {
		c.biN++
		if seen(words[i-1:i], words[i]) {
			c.bi++
		}
		if i >= 2 {
			c.triN++
			if seen(words[i-2:i], words[i]) {
				c.tri++
			} else if c.misses != nil {
				fmt.Fprintf(c.misses, "miss %q | %s\n", strings.Join(words[i-2:i+1], " "), strings.Join(words, " "))
			}
		}
		if i >= 3 {
			c.quadN++
			if seen(words[i-3:i], words[i]) {
				c.quad++
			}
		}
	}
}

func (c coherence) String() string {
	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	return fmt.Sprintf("attested bigrams %.1f%%  trigrams %.1f%%  4-grams %.1f%% (recitation)",
		pct(c.bi, c.biN), pct(c.tri, c.triN), pct(c.quad, c.quadN))
}
