package voicenote

import (
	"context"
	"errors"
)

// ErrNoEngine is returned by the stub engine. It is not a failure to handle: it is the
// answer, and Available reports it up front so the plugin declines rather than queueing
// work that cannot be done.
var ErrNoEngine = errors.New("voicenote: no transcription engine is compiled in")

// Engine turns an audio file into text.
//
// No implementation ships in this repository, and that is deliberate rather than pending.
// The one peregrine had shelled out to ffmpeg and whisper-cli and needed a 465 MiB model:
// none of that exists in a distroless image, which has no shell at all, and all three
// assets are gitignored because the model alone is over GitHub's hard 100 MiB per-file
// limit. So the feature has never run anywhere but one Windows machine, and the seam is
// what this repository can honestly own.
//
// # What a real engine must not reproduce
//
// The implementation this replaces is finding 21 plus four more defects, and they are worth
// listing because each is a thing an implementer would otherwise write again:
//
//   - A bare http.Get with no timeout, no status check and no size cap. An attacker
//     controls the URL's content length, and the bot would have written it to disk.
//   - exec.Command with no context, so a wedged whisper run could not be killed and
//     shutdown waited for it.
//   - Binary, model and scratch paths resolved against the WORKING DIRECTORY, which is the
//     one thing CLAUDE.md says must never happen: started from anywhere but the repo root
//     it silently found nothing.
//   - A scratch filename derived from the URL, sanitized by hand with a regexp.
//   - Transcripts reaching the corpus without CheckLearn, which was one of A1's four
//     unfiltered callers. That one is closed structurally now: the gate lives inside
//     learn.Learner.Message, so an engine cannot reintroduce it.
//
// A real engine takes the context it is given, writes into a directory the caller owns, and
// treats the audio as hostile input.
type Engine interface {
	// Available reports whether transcription can actually happen. Checked before anything
	// is queued, so a missing binary is one log line at startup rather than a failure reply
	// per voice note.
	Available() bool

	// Transcribe returns the text of the audio at path. It must respect ctx.
	Transcribe(ctx context.Context, path string) (string, error)
}

// stubEngine is the only Engine in this repository. It reports itself unavailable, which is
// the honest state and the quiet direction: no engine means no transcription, never
// transcription that silently produces nothing.
type stubEngine struct{}

// StubEngine returns the no-op engine. Exported so cmd/bot names what it is wiring rather
// than passing nil and relying on this package to interpret it.
func StubEngine() Engine { return stubEngine{} }

func (stubEngine) Available() bool { return false }

func (stubEngine) Transcribe(context.Context, string) (string, error) {
	return "", ErrNoEngine
}
