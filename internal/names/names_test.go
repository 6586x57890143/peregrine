package names_test

import (
	"errors"
	"strconv"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
)

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

// fakeSession answers GuildMember from a map, and counts the calls, because how many REST
// requests this makes is the thing that mattered: a message mentioning three people used to
// cost nine of them.
type fakeSession struct {
	nicks map[string]string
	calls int
	fail  bool
}

func (f *fakeSession) GuildMember(_, userID string, _ ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.calls++
	if f.fail {
		return nil, errors.New("no such member")
	}
	nick, ok := f.nicks[userID]
	if !ok {
		return &discordgo.Member{}, nil
	}
	return &discordgo.Member{Nick: nick}, nil
}

// TestResolveReturnsBothSpellingsOfAPerson.
//
// The same person appears twice when they have a nickname, deliberately: the corpus learns both
// spellings, because people address each other by whichever they see.
func TestResolveReturnsBothSpellingsOfAPerson(t *testing.T) {
	s := &fakeSession{nicks: map[string]string{snowflake(1): "birdlover"}}
	mentions := []*discordgo.User{{ID: snowflake(1), Username: "alice"}}

	users, seen := names.Resolve(s, mentions, "g1")
	if len(users) != 2 {
		t.Fatalf("Resolve returned %d users, want the username and the nickname: %+v", len(users), users)
	}
	// DISPLAY FORM FIRST, and the order is load-bearing rather than incidental: Primary returns
	// this slice's first entry and Substitute uses the first spelling it sees for an ID, so both
	// would start writing a lowercase handle into the corpus where a human would have typed the
	// nickname.
	if users[0].Name != "birdlover" || users[1].Name != "alice" {
		t.Errorf("names = %q and %q, want the nickname birdlover before the username alice",
			users[0].Name, users[1].Name)
	}
	// Both entries are the same person, so the ID set holds one.
	if len(seen) != 1 {
		t.Errorf("the consumed ID set holds %d entries, want 1", len(seen))
	}
}

// TestResolveDeduplicatesRepeatedMentions, because Discord lists a user once per mention and a
// message can mention the same person five times.
func TestResolveDeduplicatesRepeatedMentions(t *testing.T) {
	s := &fakeSession{}
	mentions := []*discordgo.User{
		{ID: snowflake(1), Username: "alice"},
		{ID: snowflake(1), Username: "alice"},
		{ID: snowflake(1), Username: "alice"},
	}

	users, _ := names.Resolve(s, mentions, "g1")
	if len(users) != 1 {
		t.Errorf("Resolve returned %d users for one person mentioned three times", len(users))
	}
	if s.calls != 1 {
		t.Errorf("made %d GuildMember requests for one person, want 1", s.calls)
	}
}

// TestResolveWithoutAGuildMakesNoRequests. A direct message has no guild, so there is no
// nickname to fetch, and asking anyway would be a REST call whose answer cannot exist.
func TestResolveWithoutAGuildMakesNoRequests(t *testing.T) {
	s := &fakeSession{nicks: map[string]string{snowflake(1): "birdlover"}}
	mentions := []*discordgo.User{{ID: snowflake(1), Username: "alice"}}

	users, _ := names.Resolve(s, mentions, "")
	if s.calls != 0 {
		t.Errorf("made %d GuildMember requests with no guild ID", s.calls)
	}
	if len(users) != 1 {
		t.Errorf("returned %d users, want just the username", len(users))
	}
}

// TestResolveToleratesAFailedLookup. A member the bot cannot fetch still has a username, and
// losing the whole mention over a nickname would cost the corpus more than the nickname is
// worth.
func TestResolveToleratesAFailedLookup(t *testing.T) {
	s := &fakeSession{fail: true}
	mentions := []*discordgo.User{{ID: snowflake(1), Username: "alice"}}

	users, _ := names.Resolve(s, mentions, "g1")
	if len(users) != 1 || users[0].Name != "alice" {
		t.Errorf("users = %+v, want the username alone", users)
	}
}

// TestRecordWritesACanonicalNameAndAnAlias, so a nickname resolves back to the person.
func TestRecordWritesACanonicalNameAndAnAlias(t *testing.T) {
	s := dbtest.Store(t)

	if err := s.Update(func(w *storage.Writer) error {
		canonical, err := names.Record(w, "birdlover", snowflake(1), "alice")
		if err != nil {
			return err
		}
		if canonical != "alice" {
			t.Errorf("canonical = %q, want the username", canonical)
		}
		return nil
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		// The nickname is an alias pointing at the canonical name.
		got, ok := names.Canonical(r, "birdlover")
		if !ok || got != "alice" {
			t.Errorf("Canonical(birdlover) = %q, %v; want alice, true", got, ok)
		}
		// And the canonical name resolves to itself.
		got, ok = names.Canonical(r, "alice")
		if !ok || got != "alice" {
			t.Errorf("Canonical(alice) = %q, %v; want alice, true", got, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCanonicalDoesNotRecognizeAnUnknownWord, which is what stops every noun in a message being
// treated as somebody's name.
func TestCanonicalDoesNotRecognizeAnUnknownWord(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.View(func(r *storage.Reader) error {
		for _, word := range []string{"", "bird", "definitelynotaname"} {
			if got, ok := names.Canonical(r, word); ok {
				t.Errorf("Canonical(%q) = %q, true; want not recognized", word, got)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFromContentFindsNamesNobodyMentioned, which is the point of the second phase: people talk
// about each other by name far more often than they @ each other.
func TestFromContentFindsNamesNobodyMentioned(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		_, err := names.Record(w, "alice", snowflake(1), "alice")
		return err
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		got := names.FromContent(r, "has anyone seen alice today", nil)
		if len(got) != 1 || got[0].Name != "alice" {
			t.Errorf("FromContent = %+v, want alice", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFromContentSkipsPeopleAlreadyMentioned. A message that both @s and names somebody must not
// count them as two people, because the caller feeds this list to the association indexes.
func TestFromContentSkipsPeopleAlreadyMentioned(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		_, err := names.Record(w, "alice", snowflake(1), "alice")
		return err
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		seen := map[string]struct{}{snowflake(1): {}}
		if got := names.FromContent(r, "alice is here", seen); len(got) != 0 {
			t.Errorf("FromContent = %+v, want nothing: this person was already consumed", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFromContentResolvesAnAliasToItsCanonicalRecord, because the record carrying the user ID is
// the canonical one and the alias is just a spelling.
func TestFromContentResolvesAnAliasToItsCanonicalRecord(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Update(func(w *storage.Writer) error {
		_, err := names.Record(w, "birdlover", snowflake(1), "alice")
		return err
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		got := names.FromContent(r, "birdlover was right", nil)
		if len(got) != 1 {
			t.Fatalf("FromContent = %+v, want one user", got)
		}
		if got[0].Name != "alice" || got[0].UserID != snowflake(1) {
			t.Errorf("user = %+v, want the canonical name and the user ID", got[0])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestOfMessageDegradesToMentionsWhenTheCorpusIsUnreadable is the fail-soft direction. Losing
// the names somebody typed costs the bot some association data; losing the message costs it the
// message.
//
// A closed store is the only way to make the read fail, and it is exactly what a caller would
// hit during shutdown.
func TestOfMessageDegradesToMentionsWhenTheCorpusIsUnreadable(t *testing.T) {
	s := dbtest.Store(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sess := &fakeSession{}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:       snowflake(5),
		Content:  "alice was right",
		Mentions: []*discordgo.User{{ID: snowflake(1), Username: "alice"}},
	}}

	got := names.OfMessage(sess, storage.Single(s), m, "g1")
	if len(got) != 1 || got[0].Name != "alice" {
		t.Errorf("OfMessage = %+v, want the @mentions alone", got)
	}
}

// TestNilMentionsAreSkipped rather than dereferenced. discordgo's slices are built from JSON and
// a nil entry is cheap to guard against.
func TestNilMentionsAreSkipped(t *testing.T) {
	s := &fakeSession{}
	users, _ := names.Resolve(s, []*discordgo.User{nil, {ID: snowflake(1), Username: "alice"}}, "")
	if len(users) != 1 {
		t.Errorf("users = %+v, want just alice", users)
	}
}

// TestSpellingsCoversAllThreeNames.
//
// A Discord user has up to three, and people type whichever they see. GlobalName was missing
// from all three sites that hand-built this, which since usernames became lowercase handles is
// the one most people are actually addressed by.
func TestSpellingsCoversAllThreeNames(t *testing.T) {
	u := &discordgo.User{ID: snowflake(1), Username: "alice", GlobalName: "Alice A"}
	member := &discordgo.Member{Nick: "birdlover"}

	got := names.Spellings(u, member)
	if len(got) != 3 {
		t.Fatalf("Spellings = %+v, want the nickname, the global name and the username", got)
	}
	// Display first, which Primary and Substitute both depend on.
	for i, want := range []string{"birdlover", "Alice A", "alice"} {
		if got[i].Name != want {
			t.Errorf("Spellings[%d].Name = %q, want %q", i, got[i].Name, want)
		}
	}
	// Every entry is the same person under the same canonical form, which is what lets
	// Record write them as aliases pointing at one name.
	for _, u := range got {
		if u.UserID != snowflake(1) || u.Username != "alice" {
			t.Errorf("entry %+v does not carry the canonical identity", u)
		}
	}
}

// TestSpellingsCollapsesDuplicates. A nickname identical to the username is one name, and a
// person whose global name only differs in case is not two people.
func TestSpellingsCollapsesDuplicates(t *testing.T) {
	u := &discordgo.User{ID: snowflake(1), Username: "alice", GlobalName: "Alice"}
	got := names.Spellings(u, &discordgo.Member{Nick: "alice"})
	if len(got) != 1 {
		t.Errorf("Spellings = %+v, want one spelling", got)
	}
}

// TestPrimaryToleratesANilUser, because indexing Spellings would panic and a nil Author has
// turned up in a fixture here before. Production always having one is not a reason to write the
// version that crashes if it does not.
func TestPrimaryToleratesANilUser(t *testing.T) {
	if got := names.Primary(nil, nil); got != (names.User{}) {
		t.Errorf("Primary(nil, nil) = %+v, want the zero User", got)
	}
	got := names.Primary(&discordgo.User{ID: snowflake(1), Username: "alice"}, nil)
	if got.Name != "alice" {
		t.Errorf("Primary = %+v, want the username when there is nothing else", got)
	}
}

// TestSubstituteTurnsMentionsIntoNames.
//
// The tokenizer keeps <@123> as ONE token, so without this the most explicit way to name
// somebody reaches generation as a blob Canonical cannot resolve, and the corpus stores an ID
// where a name belongs.
func TestSubstituteTurnsMentionsIntoNames(t *testing.T) {
	users := []names.User{
		{Name: "birdlover", UserID: snowflake(1), Username: "alice"},
		{Name: "alice", UserID: snowflake(1), Username: "alice"},
		{Name: "bob", UserID: snowflake(2), Username: "bob"},
	}

	in := "<@" + snowflake(1) + "> and <@!" + snowflake(2) + "> are at it again"
	want := "birdlover and bob are at it again"
	if got := names.Substitute(in, users); got != want {
		t.Errorf("Substitute = %q, want %q", got, want)
	}
}

// TestSubstituteLeavesAnUnknownIDAlone. Stripping it would drop a word out of the middle of a
// sentence and cost the structure around it, and leaving it is exactly today's behaviour for
// every mention.
func TestSubstituteLeavesAnUnknownIDAlone(t *testing.T) {
	users := []names.User{{Name: "alice", UserID: snowflake(1), Username: "alice"}}

	in := "<@" + snowflake(9) + "> said something"
	if got := names.Substitute(in, users); got != in {
		t.Errorf("Substitute = %q, want it unchanged", got)
	}
}

// TestSubstituteIgnoresRoleMentions. A role is not a person this package can name, and <@&123>
// has an ampersand where the pattern wants a digit.
func TestSubstituteIgnoresRoleMentions(t *testing.T) {
	users := []names.User{{Name: "alice", UserID: snowflake(1), Username: "alice"}}

	in := "<@&" + snowflake(1) + "> get in here"
	if got := names.Substitute(in, users); got != in {
		t.Errorf("Substitute = %q, want a role mention left alone", got)
	}
}
