package sessions

import (
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
	_ "modernc.org/sqlite"
)

// The fixtures here build the same protobuf the real store holds, field
// numbers and all (antigravity_proto.go): a step's timestamp and its model
// call's figures, and a generator's model and the steps it owns. The bytes
// are built rather than captured, so a conversation's own content (which is
// encrypted at rest, and private) is never committed.

// agyPbVarint appends a field's varint key and value.
func agyPbVarint(b []byte, num int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3|0)
	return binary.AppendUvarint(b, v)
}

// agyPbBytes appends a length-delimited field.
func agyPbBytes(b []byte, num int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// agyPbPacked appends a packed repeated varint field.
func agyPbPacked(b []byte, num int, vs ...uint64) []byte {
	var inner []byte
	for _, v := range vs {
		inner = binary.AppendUvarint(inner, v)
	}
	return agyPbBytes(b, num, inner)
}

// agyPbTime is a google.protobuf.Timestamp.
func agyPbTime(sec int64, nanos int32) []byte {
	var b []byte
	b = agyPbVarint(b, 1, uint64(sec))
	if nanos != 0 {
		b = agyPbVarint(b, 2, uint64(nanos))
	}
	return b
}

// agyStepBytes is a steps.metadata: the time, and the model call's figures.
type agyStepIn struct {
	at         time.Time
	input      int
	output     int
	cacheWrite int
	cacheRead  int
	messageID  string
	responseID string
}

func agyStepBytes(in agyStepIn) []byte {
	var call []byte
	if in.input > 0 {
		call = agyPbVarint(call, 2, uint64(in.input))
	}
	if in.output > 0 {
		call = agyPbVarint(call, 3, uint64(in.output))
	}
	if in.cacheWrite > 0 {
		call = agyPbVarint(call, 4, uint64(in.cacheWrite))
	}
	if in.cacheRead > 0 {
		call = agyPbVarint(call, 5, uint64(in.cacheRead))
	}
	if in.messageID != "" {
		call = agyPbBytes(call, 7, []byte(in.messageID))
	}
	if in.responseID != "" {
		call = agyPbBytes(call, 11, []byte(in.responseID))
	}
	var b []byte
	if !in.at.IsZero() {
		b = agyPbBytes(b, 1, agyPbTime(in.at.Unix(), int32(in.at.Nanosecond())))
	}
	return agyPbBytes(b, 9, call)
}

// agyGenBytes is a gen_metadata.data: the model (field 19) and the steps it
// owns (field 2, packed).
func agyGenBytes(model string, indices ...uint64) []byte {
	var chat []byte
	chat = agyPbBytes(chat, 19, []byte(model))
	var b []byte
	b = agyPbBytes(b, 1, chat)
	return agyPbPacked(b, 2, indices...)
}

// agyFixture builds a conversation database and the summaries database, in

// agyIsolate gives a test a home of its own and forgets the kept parses, so
// one test's conversations are not another's (the package's TestMain gives
// a shared home, and agyFiles lists whatever is in it).
func agyIsolate(t *testing.T) {
	t.Helper()
	testenv.SetHome(t, t.TempDir())
	Reset()
}

// a home of the test's own (testenv), and returns the conversation id.
type agyConv struct {
	id    string
	steps map[int64][]byte
	gens  [][]byte
}

func agyFixture(t *testing.T, convs ...agyConv) {
	t.Helper()
	dir := agyConversationsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exec := func(db *sql.DB, q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range convs {
		path := filepath.Join(dir, c.id+".db")
		db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		exec(db, "CREATE TABLE steps(idx INTEGER PRIMARY KEY, metadata BLOB)")
		exec(db, "CREATE TABLE gen_metadata(idx INTEGER PRIMARY KEY, data BLOB)")
		for idx, blob := range c.steps {
			exec(db, "INSERT INTO steps(idx, metadata) VALUES (?, ?)", idx, blob)
		}
		for idx, blob := range c.gens {
			exec(db, "INSERT INTO gen_metadata(idx, data) VALUES (?, ?)", idx, blob)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// agySummaries writes conversation_summaries.db with one row per title.
func agySummaries(t *testing.T, rows map[string]struct{ title, uris string }) {
	t.Helper()
	path := agySummaryPath()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT)`); err != nil {
		t.Fatal(err)
	}
	for id, r := range rows {
		if _, err := db.Exec("INSERT INTO conversation_summaries VALUES (?, ?, ?)", id, r.title, r.uris); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAntigravityReadsOneSnapshotAcrossQueries: the App writes a generator
// and its steps in one commit. agyRead reads the models then the steps; a
// write committed between the two must not be half-seen — a step whose
// generator is not in the snapshot is a missing model, which reads as an
// unreadable conversation and drops the whole history from the page. Both
// queries share one read transaction, so the later commit is invisible.
func TestAntigravityReadsOneSnapshotAcrossQueries(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	id := "eeeeeeee-1111-2222-3333-444444444444"
	agyFixture(t, agyConv{
		id:    id,
		steps: map[int64][]byte{0: agyStepBytes(agyStepIn{at: at, input: 10, output: 1, responseID: "r0"})},
		gens:  [][]byte{agyGenBytes("gemini-3.8-flash-n", 0)},
	})
	files := agyFiles()
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	path := files[0].path

	// The App commits a new generator and a new step mid-read.
	was := agyBetweenQueries
	agyBetweenQueries = func() {
		db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("INSERT INTO gen_metadata(idx, data) VALUES (?, ?)", 1,
			agyGenBytes("gemini-3.8-flash-n", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO steps(idx, metadata) VALUES (?, ?)", 1,
			agyStepBytes(agyStepIn{at: at.Add(time.Second), input: 20, output: 2, responseID: "r1"})); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { agyBetweenQueries = was })

	usages, _, _, err := agyRead(files[0])
	if err != nil {
		t.Fatalf("agyRead saw a half-written commit: %v", err)
	}
	if len(usages) != 1 || usages[0].tokens.Input != 10 {
		t.Fatalf("usages = %+v, want the one snapshot's call (input 10)", usages)
	}
}

// TestAntigravityFoldsStepsByResponse is the contract cc-bar's scanner
// keeps: three steps whose first two share a response id are two calls, the
// shared one taking the last step's figures. Without the fold the same
// request is counted twice.
func TestAntigravityFoldsStepsByResponse(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	agyFixture(t, agyConv{
		id: "11111111-2222-3333-4444-555555555555",
		steps: map[int64][]byte{
			0: agyStepBytes(agyStepIn{at: at, input: 17, output: 9, cacheRead: 500, cacheWrite: 7, messageID: "m1", responseID: "r1"}),
			1: agyStepBytes(agyStepIn{at: at.Add(time.Second), input: 21, output: 11, cacheRead: 500, cacheWrite: 7, messageID: "m2", responseID: "r1"}),
			2: agyStepBytes(agyStepIn{at: at.Add(2 * time.Second), input: 3, output: 9, cacheRead: 500, cacheWrite: 7, messageID: "m3"}),
		},
		gens: [][]byte{agyGenBytes("gemini-3.8-flash-n", 0, 1, 2)},
	})
	files := agyFiles()
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	usages, _, _, err := agyRead(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 {
		t.Fatalf("usages = %d, want 2 (three steps, two responses)", len(usages))
	}
	if got := usages[0].tokens.Input; got != 21 {
		t.Errorf("first input = %d, want 21 (the last step of response r1)", got)
	}
	if got := usages[1].tokens.Input; got != 3 {
		t.Errorf("second input = %d, want 3", got)
	}
	if usages[0].responseID != "r1" || usages[1].responseID != "" {
		t.Errorf("response ids = %q, %q", usages[0].responseID, usages[1].responseID)
	}
}

// TestAntigravitySessionTokens checks the session reader: the folded calls
// land in one session's models and days, named by the summaries database.
func TestAntigravitySessionTokens(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	agyFixture(t, agyConv{
		id: id,
		steps: map[int64][]byte{
			0: agyStepBytes(agyStepIn{at: at, input: 17, output: 9, cacheRead: 500, cacheWrite: 7, responseID: "r1"}),
			1: agyStepBytes(agyStepIn{at: at, input: 21, output: 11, cacheRead: 500, cacheWrite: 7, responseID: "r1"}),
			2: agyStepBytes(agyStepIn{at: at, input: 3, output: 9, cacheRead: 500, cacheWrite: 7, responseID: "r9"}),
		},
		gens: [][]byte{agyGenBytes("gemini-3.8-flash-n", 0, 1, 2)},
	})
	agySummaries(t, map[string]struct{ title, uris string }{
		id: {title: "Fix the build", uris: `[{"scheme":"file","path":"/work/agy"}]`},
	})
	files := agyFiles()
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	st := parseAntigravity(files[0])
	if st.Title != "Fix the build" {
		t.Errorf("title = %q", st.Title)
	}
	if st.Cwd != "/work/agy" {
		t.Errorf("cwd = %q", st.Cwd)
	}
	want := Tokens{Input: 21 + 3, Output: 11 + 9, CacheRead: 1000, CacheWrite: 14}
	if got := st.Models["gemini-3.8-flash-n"]; got != want {
		t.Errorf("model tokens = %+v, want %+v", got, want)
	}
	if d := st.Days[dateOf(at)]; d == nil || d.Models["gemini-3.8-flash-n"] != want {
		t.Errorf("day tokens missing: %+v", d)
	}
}

// TestAntigravityCallsSource checks the call reader: one call per folded
// response, its key the fold named by the conversation (so the global Msg
// key stays unique across conversations), its ResponseID the store's.
func TestAntigravityCallsSource(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const id = "bbbbbbbb-1111-2222-3333-444444444444"
	agyFixture(t, agyConv{
		id: id,
		steps: map[int64][]byte{
			0: agyStepBytes(agyStepIn{at: at, input: 100, output: 20, responseID: "resp-a"}),
			1: agyStepBytes(agyStepIn{at: at.Add(time.Second), input: 5, output: 6, messageID: "m-only"}),
		},
		gens: [][]byte{agyGenBytes("gemini-3.8-flash-n", 0, 1)},
	})
	files := agyFiles()
	st, ok := readAntigravityCalls(files[0])
	if !ok {
		t.Fatal("read not ok")
	}
	if len(st.Calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(st.Calls))
	}
	if st.Calls[0].ResponseID != "resp-a" || st.Calls[0].Msg != id+":response:resp-a" {
		t.Errorf("first call = %+v", st.Calls[0])
	}
	if st.Calls[1].ResponseID != "" || st.Calls[1].Msg != id+":message:m-only" {
		t.Errorf("second call = %+v", st.Calls[1])
	}
	if st.Agent != "agy" || st.Session != id {
		t.Errorf("agent/session = %q/%q", st.Agent, st.Session)
	}
}

// TestAntigravityStepCallsNotDedupedAcrossConversations: a step with no
// response or message id is keyed by its own index, which is only unique
// within one conversation. Two conversations each with such a step 0 must
// both count — sessions.Calls drops a call whose Msg was seen in another
// file, so the Msg carries the conversation's name.
func TestAntigravityStepCallsNotDedupedAcrossConversations(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	agyFixture(t,
		agyConv{
			id:    "11111111-0000-0000-0000-000000000001",
			steps: map[int64][]byte{0: agyStepBytes(agyStepIn{at: at, input: 10, output: 1})},
			gens:  [][]byte{agyGenBytes("gemini-3.8-flash-n", 0)},
		},
		agyConv{
			id:    "11111111-0000-0000-0000-000000000002",
			steps: map[int64][]byte{0: agyStepBytes(agyStepIn{at: at.Add(time.Minute), input: 20, output: 2})},
			gens:  [][]byte{agyGenBytes("gemini-3.8-flash-n", 0)},
		},
	)
	calls := Calls(time.Time{})
	var got []Call
	for _, c := range calls {
		if c.Agent == "agy" {
			got = append(got, c)
		}
	}
	if len(got) != 2 {
		t.Fatalf("agy calls = %d, want 2 (one per conversation's step 0): %+v", len(got), got)
	}
	var in int
	for _, c := range got {
		in += c.Input
	}
	if in != 30 {
		t.Errorf("total input = %d, want 30 (10 + 20)", in)
	}
}

// TestAntigravityUnreadableMetadataIsNotEmpty: a database whose steps have
// no model to price them by is an error, not an empty conversation — the
// figures would be silently wrong.
func TestAntigravityUnreadableMetadataIsNotEmpty(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	agyFixture(t, agyConv{
		id:    "cccccccc-0000-1111-2222-333333333333",
		steps: map[int64][]byte{0: agyStepBytes(agyStepIn{at: at, input: 10, output: 5, responseID: "r1"})},
		gens:  nil, // no generator: no model to price the call by
	})
	files := agyFiles()
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if _, _, _, err := agyRead(files[0]); err == nil {
		t.Fatal("agyRead accepted a step with no model")
	}
	if st := parseAntigravity(files[0]); len(st.Models) != 0 {
		t.Errorf("session read figures from unreadable metadata: %+v", st.Models)
	}
}

// TestAntigravityStats checks the target page, "使用 → 会话": StatsAt
// reads from allFiles, includes an agy session, and its day-by-day usage
// carries the model and folder of the conversation.
func TestAntigravityStats(t *testing.T) {
	agyIsolate(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	id := "dddddddd-1111-2222-3333-444444444444"
	agyFixture(t, agyConv{
		id: id,
		steps: map[int64][]byte{
			0: agyStepBytes(agyStepIn{at: at, input: 17, output: 9, cacheRead: 500, cacheWrite: 7, responseID: "r1"}),
			1: agyStepBytes(agyStepIn{at: at, input: 21, output: 11, cacheRead: 500, cacheWrite: 7, responseID: "r1"}),
			2: agyStepBytes(agyStepIn{at: at.Add(2 * time.Second), input: 3, output: 9, cacheRead: 500, cacheWrite: 7, responseID: "r9"}),
		},
		gens: [][]byte{agyGenBytes("gemini-3.8-flash-n", 0, 1, 2)},
	})
	agySummaries(t, map[string]struct{ title, uris string }{
		id: {title: "Stats page session", uris: `[{"scheme":"file","path":"/work/stats"}]`},
	})

	resetCache(t)
	st := StatsAt(0, at.Add(time.Hour))
	var found bool
	for _, s := range st.Sessions {
		if s.Agent == "agy" && s.ID == id {
			found = true
			if s.Cwd != "/work/stats" {
				t.Errorf("session cwd = %q", s.Cwd)
			}
			if got := s.Tokens; got != (Tokens{Input: 21 + 3, Output: 11 + 9, CacheRead: 1000, CacheWrite: 14}) {
				t.Errorf("session tokens = %+v", got)
			}
		}
	}
	if !found {
		t.Error("agy session missing from StatsAt")
	}

	day := dateOf(at)
	var usageTokens Tokens
	for _, d := range st.Days {
		if d.Date != day {
			continue
		}
		for _, u := range d.Usage {
			if u.Agent == "agy" && u.Cwd == "/work/stats" && u.Model == "gemini-3.8-flash-n" {
				usageTokens.add(u.Tokens)
			}
		}
	}
	if usageTokens != (Tokens{Input: 21 + 3, Output: 11 + 9, CacheRead: 1000, CacheWrite: 14}) {
		t.Errorf("day usage tokens = %+v", usageTokens)
	}
}

func resetCache(t *testing.T) {
	t.Helper()
	Reset()
}
